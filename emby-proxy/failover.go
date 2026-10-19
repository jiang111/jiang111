package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// 故障接管：
// 1. 各台每 probeInterval 探测其它节点（经对方 nginx 访问站点的健康检查地址，和客户端走同一条路），
//    连续 downAfter 次失败判定失联：分配和视频直连都不再选它，它的用户由活着的节点负责；连续 upAfter 次成功判定恢复。
// 2. 配置了 Cloudflare 令牌时，失联满 dnsDelay（再按节点名排序错开 dnsStagger）后，活着的节点把它的 IP
//    从客户端域名的 A 记录里删掉；恢复满 dnsDelay 后加回。主节点挂了时由从节点动手。
//    删除前重新查询，至少留一条才删；记录被删光时把自己加回去。参与分配的节点都应在 DNS 里。
//    主节点可以只做控制（不填本机公网地址）：不参与分配、不在 DNS 里，节点失联时由它第一个动手。

const (
	peerHealthPath = "/__emby_proxy/health"
	probeInterval  = 10 * time.Second
	probeTimeout   = 5 * time.Second
	downAfter      = 3
	upAfter        = 2
	dnsDelay       = time.Minute
	dnsStagger     = 20 * time.Second
	dnsTTL         = 60
)

// DNSFailover 是自动改 DNS 的设置（主节点填写，同步给从节点）。
type DNSFailover struct {
	Token  string `json:"token,omitempty"`   // Cloudflare API 令牌（只需该域名的 DNS 编辑权限）
	ZoneID string `json:"zone_id,omitempty"` // Cloudflare Zone ID
	Name   string `json:"name,omitempty"`    // 客户端域名，如 emby.example.com
}

func (d *DNSFailover) enabled() bool {
	return d != nil && d.Token != "" && d.ZoneID != "" && d.Name != ""
}

type peerHealth struct {
	Name     string `json:"name"`
	Addr     string `json:"addr"`
	Down     bool   `json:"down"`
	Since    int64  `json:"since"` // 进入当前状态的时间
	fails    int
	oks      int
	LastErr  string `json:"last_err,omitempty"`
	InDNS    *bool  `json:"in_dns,omitempty"` // 最近一次查询时是否在 DNS 里（未开启自动改 DNS 时为空）
	LastSeen int64  `json:"last_probe"`
}

type failover struct {
	mu      sync.Mutex
	peers   map[string]*peerHealth
	dnsErr  string
	dnsLog  []string // 最近的 DNS 操作
	dnsTime int64
}

// healthDown 是探测判定的失联（peerDown 还包括转发失败后的临时标记）。
func (m *Manager) healthDown(name string) bool {
	m.fo.mu.Lock()
	defer m.fo.mu.Unlock()
	p := m.fo.peers[name]
	return p != nil && p.Down
}

// watchPeers 返回要探测的其它节点：主节点用所有连过来的从节点（含已超时的，要能发现它挂了），
// 从节点用主节点下发的成员列表。
func (m *Manager) watchPeers() []Member {
	self := m.selfName()
	var out []Member
	m.mu.RLock()
	role, sticky := m.cfg.Cluster.Role, m.cfg.Cluster.Sticky
	list := m.cfg.Members
	m.mu.RUnlock()
	if !sticky {
		return nil
	}
	switch role {
	case "node":
		out = append(out, list...)
	case "master":
		if m.cluster == nil {
			return nil
		}
		m.cluster.mu.Lock()
		for _, n := range m.cluster.nodes {
			if n.Addr != "" {
				out = append(out, Member{Name: n.Name, Addr: n.Addr, Host: n.Host})
			}
		}
		m.cluster.mu.Unlock()
	}
	res := out[:0]
	for _, mb := range out {
		if mb.Name != self {
			res = append(res, mb)
		}
	}
	sort.Slice(res, func(i, j int) bool { return res[i].Name < res[j].Name })
	return res
}

// probeTarget 选一个独立端口站点的客户端访问地址作为探测地址。
func (m *Manager) probeTarget() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.cfg.Sites {
		if s.Enabled && s.PublicURL != "" && s.Mode == "port" {
			if u, err := url.Parse(s.PublicURL); err == nil && u.Host != "" {
				return strings.TrimRight(s.PublicURL, "/") + peerHealthPath
			}
		}
	}
	return ""
}

func probePeer(target, addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", target, nil)
	if err != nil {
		return err
	}
	resp, err := peerTransport(addr).RoundTrip(req)
	if err != nil {
		return errors.New(simplifyNetErr(err))
	}
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64))
	resp.Body.Close()
	if resp.StatusCode != 200 || string(b) != "ok" {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// probePeers 探测一轮，更新各节点状态。probe 可替换（测试用）。
func (m *Manager) probePeers(now time.Time, probe func(target, addr string) error) {
	peers := m.watchPeers()
	target := m.probeTarget()
	if target == "" || len(peers) == 0 {
		return
	}
	errs := make([]error, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = probe(target, p.Addr)
		}()
	}
	wg.Wait()
	fo := &m.fo
	fo.mu.Lock()
	defer fo.mu.Unlock()
	if fo.peers == nil {
		fo.peers = map[string]*peerHealth{}
	}
	seen := map[string]bool{}
	for i, p := range peers {
		seen[p.Name] = true
		h := fo.peers[p.Name]
		if h == nil {
			h = &peerHealth{Name: p.Name, Since: now.Unix()}
			fo.peers[p.Name] = h
		}
		h.Addr, h.LastSeen = p.Addr, now.Unix()
		if err := errs[i]; err != nil {
			h.fails, h.oks, h.LastErr = h.fails+1, 0, err.Error()
			if !h.Down && h.fails >= downAfter {
				h.Down, h.Since = true, now.Unix()
				log.Printf("节点 %s（%s）失联：%v，它的用户改由其它节点负责", p.Name, p.Addr, err)
			}
		} else {
			h.oks, h.fails, h.LastErr = h.oks+1, 0, ""
			if h.Down && h.oks >= upAfter {
				h.Down, h.Since = false, now.Unix()
				log.Printf("节点 %s（%s）已恢复", p.Name, p.Addr)
			}
		}
	}
	for name := range fo.peers {
		if !seen[name] {
			delete(fo.peers, name)
		}
	}
}

// ---- Cloudflare DNS ----

var cfAPI = "https://api.cloudflare.com/client/v4" // 测试时替换
var cfClient = &http.Client{Timeout: 15 * time.Second}

type cfRecord struct {
	ID      string `json:"id"`
	Content string `json:"content"`
}

func cfDo(d *DNSFailover, method, path string, body any) (json.RawMessage, error) {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, cfAPI+"/zones/"+url.PathEscape(d.ZoneID)+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cfClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接 Cloudflare 失败: %s", simplifyNetErr(err))
	}
	defer resp.Body.Close()
	var out struct {
		Success bool `json:"success"`
		Errors  []struct{ Message string }
		Result  json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("Cloudflare 返回 HTTP %d", resp.StatusCode)
	}
	if !out.Success {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(out.Errors) > 0 {
			msg = out.Errors[0].Message
		}
		return nil, errors.New("Cloudflare: " + msg)
	}
	return out.Result, nil
}

func cfListA(d *DNSFailover) ([]cfRecord, error) {
	res, err := cfDo(d, "GET", "/dns_records?type=A&per_page=100&name="+url.QueryEscape(d.Name), nil)
	if err != nil {
		return nil, err
	}
	var rs []cfRecord
	if err := json.Unmarshal(res, &rs); err != nil {
		return nil, err
	}
	return rs, nil
}

func cfAddA(d *DNSFailover, ip string) error {
	_, err := cfDo(d, "POST", "/dns_records", map[string]any{"type": "A", "name": d.Name, "content": ip, "ttl": dnsTTL, "proxied": false})
	return err
}

func cfDelete(d *DNSFailover, id string) error {
	_, err := cfDo(d, "DELETE", "/dns_records/"+url.PathEscape(id), nil)
	return err
}

// hostIP 去掉地址里的端口。
func hostIP(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return strings.Trim(addr, "[]")
}

// dnsFailover 按探测结果调整 DNS：失联满 dnsDelay（加错开时间）删掉，恢复满 dnsDelay 加回，记录删光时把自己加回。
func (m *Manager) dnsFailover(now time.Time) {
	m.mu.RLock()
	d := m.cfg.DNS
	selfAddr := m.cfg.Cluster.AdvertiseAddr
	controlOnly := m.cfg.Cluster.Role == "master" && selfAddr == ""
	m.mu.RUnlock()
	if !d.enabled() {
		return
	}
	self := m.selfName()
	m.fo.mu.Lock()
	type peerState struct {
		name, ip string
		down     bool
		since    time.Time
		inDNS    *bool
	}
	var ps []peerState
	names := []string{self}
	for _, h := range m.fo.peers {
		ps = append(ps, peerState{h.Name, hostIP(h.Addr), h.Down, time.Unix(h.Since, 0), h.InDNS})
		names = append(names, h.Name)
	}
	// 需要动手时才查询（避免每 10 秒都调 Cloudflare）：失联的还在（或不确定在不在）DNS 里、恢复的不在 DNS 里；
	// 另外每 5 分钟核对一次
	sort.Strings(names)
	rank := sort.SearchStrings(names, self)
	if controlOnly {
		// 只做控制的主节点不参与分配，不会和别人互删，不用错开；而且要它先动手：
		// 失联节点超过 memberAliveWindow 没同步就被移出成员列表，其它从节点随之停止探测它，不会再去删它
		rank = 0
	}
	wait := dnsDelay + time.Duration(rank)*dnsStagger
	need := now.Unix()-m.fo.dnsTime >= 300
	for _, p := range ps {
		if now.Sub(p.since) >= wait && (p.down && (p.inDNS == nil || *p.inDNS) || !p.down && p.inDNS != nil && !*p.inDNS) {
			need = true
		}
	}
	m.fo.mu.Unlock()
	if !need {
		return
	}
	rs, err := cfListA(d)
	var acts []string
	if err == nil {
		has := map[string]string{} // IP → 记录 ID
		for _, r := range rs {
			has[r.Content] = r.ID
		}
		if len(rs) == 0 && selfAddr != "" {
			// 记录被删光了（例如两台同时把对方删掉）：把自己加回去
			if err = cfAddA(d, hostIP(selfAddr)); err == nil {
				acts = append(acts, "记录被删光，已加回本机 "+hostIP(selfAddr))
				has[hostIP(selfAddr)] = "new"
			}
		}
		for _, p := range ps {
			if err != nil || p.ip == "" || now.Sub(p.since) < wait {
				continue
			}
			id, in := has[p.ip]
			switch {
			case p.down && in && len(has) >= 2:
				if err = cfDelete(d, id); err == nil {
					delete(has, p.ip)
					acts = append(acts, fmt.Sprintf("%s 失联，已从 %s 删除 %s", p.name, d.Name, p.ip))
				}
			case !p.down && !in:
				if err = cfAddA(d, p.ip); err == nil {
					has[p.ip] = "new"
					acts = append(acts, fmt.Sprintf("%s 已恢复，已把 %s 加回 %s", p.name, p.ip, d.Name))
				}
			}
		}
		m.fo.mu.Lock()
		for _, h := range m.fo.peers {
			_, in := has[hostIP(h.Addr)]
			h.InDNS = &in
		}
		m.fo.mu.Unlock()
	}
	m.fo.mu.Lock()
	// 动过 DNS 后下一轮马上再核对（两台同时把对方删掉时，10 秒内发现记录被删光并把自己加回）；出错 1 分钟后重试
	switch {
	case len(acts) > 0:
		m.fo.dnsTime = 0
	case err != nil:
		m.fo.dnsTime = now.Unix() - 240
	default:
		m.fo.dnsTime = now.Unix()
	}
	m.fo.dnsErr = ""
	if err != nil {
		m.fo.dnsErr = err.Error()
	}
	for _, a := range acts {
		log.Printf("故障接管：%s", a)
		m.fo.dnsLog = append(m.fo.dnsLog, now.Format("01-02 15:04:05")+" "+a)
	}
	if len(m.fo.dnsLog) > 20 {
		m.fo.dnsLog = m.fo.dnsLog[len(m.fo.dnsLog)-20:]
	}
	m.fo.mu.Unlock()
	if err != nil {
		log.Printf("故障接管：调整 DNS 失败: %v", err)
	}
}

// healthLoop 每 probeInterval 探测一轮并按需调整 DNS。
func (c *Cluster) healthLoop() {
	for now := range time.Tick(probeInterval) {
		c.m.probePeers(now, probePeer)
		c.m.dnsFailover(now)
	}
}

type failoverView struct {
	Peers   []peerHealth `json:"peers"`
	DNS     bool         `json:"dns"`      // 已开启自动改 DNS
	DNSName string       `json:"dns_name"` // 客户端域名
	DNSErr  string       `json:"dns_err,omitempty"`
	DNSLog  []string     `json:"dns_log,omitempty"`
}

func (m *Manager) failoverView() failoverView {
	m.mu.RLock()
	d := m.cfg.DNS
	m.mu.RUnlock()
	v := failoverView{Peers: []peerHealth{}, DNS: d.enabled()}
	if d != nil {
		v.DNSName = d.Name
	}
	m.fo.mu.Lock()
	for _, h := range m.fo.peers {
		v.Peers = append(v.Peers, *h)
	}
	v.DNSErr = m.fo.dnsErr
	v.DNSLog = append([]string(nil), m.fo.dnsLog...)
	m.fo.mu.Unlock()
	sort.Slice(v.Peers, func(i, j int) bool { return v.Peers[i].Name < v.Peers[j].Name })
	return v
}
