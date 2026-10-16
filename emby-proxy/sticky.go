package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 同一用户固定同一台：多台服务器用同一个域名时，DNS 可能把同一个用户的 API 请求和推流请求分到不同服务器，
// 上游会看到同一账号来自多个 IP。开启后按「客户端 IP」一致性哈希选出唯一的负责节点（各节点算出的结果相同），
// 请求落到别的节点时转发给负责节点，只由负责节点访问上游：上游看到的 IP 数和用户直连时一样。
// 负责节点失联时只有它的用户换到其它节点；转发连不上时就地处理，不中断播放。

// Member 是参与分配的节点。
type Member struct {
	Name   string  `json:"name"`
	Addr   string  `json:"addr"`             // 节点公网 IP（也可带端口，默认用站点客户端访问地址里的端口）
	Host   string  `json:"host,omitempty"`   // 节点直连域名（视频直连时推流请求 302 到这里）
	Weight float64 `json:"weight,omitempty"` // 分配权重（流量均衡），0 视为 1
}

const hopHeader = "X-Emby-Proxy-Hop"

// memberAliveWindow 内同步过的从节点才算在线
const memberAliveWindow = 90 * time.Second

var hostname, _ = os.Hostname()

func (m *Manager) selfName() string {
	m.mu.RLock()
	n := m.cfg.Cluster.NodeName
	m.mu.RUnlock()
	if n == "" {
		n = hostname
	}
	return n
}

// members 返回当前参与分配的节点（按名称排序）。主节点实时计算，从节点用主节点下发的列表。
func (m *Manager) members() []Member {
	m.mu.RLock()
	role, sticky := m.cfg.Cluster.Role, m.cfg.Cluster.Sticky
	adv, host, list := m.cfg.Cluster.AdvertiseAddr, m.cfg.Cluster.DirectHost, m.cfg.Members
	var weights map[string]float64
	if m.cfg.Cluster.Balance {
		weights = cloneWeights(m.cfg.Cluster.Weights)
	}
	m.mu.RUnlock()
	if !sticky {
		return nil
	}
	if role == "node" {
		return list
	}
	if role != "master" || m.cluster == nil {
		return nil
	}
	var out []Member
	if adv != "" {
		name := m.selfName()
		out = append(out, Member{Name: name, Addr: adv, Host: host, Weight: weights[name]})
	}
	m.cluster.mu.Lock()
	for _, n := range m.cluster.nodes {
		if n.Addr != "" && time.Since(time.Unix(n.LastSeen, 0)) < memberAliveWindow {
			out = append(out, Member{Name: n.Name, Addr: n.Addr, Host: n.Host, Weight: weights[n.Name]})
		}
	}
	m.cluster.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// owner 用 rendezvous 哈希为客户端 IP 选出负责节点；跳过本机认为暂时连不上的节点。
func (m *Manager) owner(clientIP string) (Member, bool) {
	ms := m.members()
	if len(ms) < 2 {
		return Member{}, false
	}
	var best Member
	var bestScore float64
	found := false
	for _, mb := range ms {
		if m.peerDown(mb.Name) {
			continue
		}
		sum := sha256.Sum256([]byte(mb.Name + "|" + clientIP))
		if s := hrwScore(binary.BigEndian.Uint64(sum[:8]), weightOf(mb)); !found || s > bestScore {
			best, bestScore, found = mb, s, true
		}
	}
	return best, found
}

// ---- 视频直连 ----
// 请求落到非负责节点时，API 请求照常转发；推流请求（视频、HLS、推流域名转发）改为 302 到负责节点的直连域名，
// 视频只经过负责节点一台，不再「客户端 ⇄ 本机 ⇄ 负责节点」两段都走一遍。负责节点看到的仍是客户端 IP，上游看到的仍只有负责节点。

// cleanHost 校验直连域名：只允许域名（字母数字、- 和 .），不带协议、端口、路径；不合法返回空。
func cleanHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" || len(h) > 253 || !strings.Contains(h, ".") || net.ParseIP(h) != nil {
		return ""
	}
	for _, l := range strings.Split(h, ".") {
		if !labelRe.MatchString(l) {
			return ""
		}
	}
	return h
}

// directURL 返回推流请求应 302 到的负责节点地址；不需要跳转时返回空。
func (s *SiteRT) directURL(r *http.Request, prefix string, owner Member) string {
	if owner.Host == "" || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		return ""
	}
	s.m.mu.RLock()
	on, self := s.m.cfg.Cluster.DirectStream, s.m.cfg.Cluster.DirectHost
	s.m.mu.RUnlock()
	if !on {
		return ""
	}
	// 客户端已经是按本机直连域名来的（节点间的成员列表暂时不一致时）：不再跳转，避免来回 302
	h := r.Host
	if hh, _, err := net.SplitHostPort(h); err == nil {
		h = hh
	}
	if self != "" && strings.EqualFold(h, self) {
		return ""
	}
	esc := r.URL.EscapedPath()
	rest := esc
	if len(prefix) > 0 && len(esc) >= len(prefix) {
		rest = esc[len(prefix):]
	}
	if !isMediaPath(rest) && !strings.HasPrefix(rest, epPrefix) {
		if _, matched, _ := s.matchRoute(rest, r); !matched {
			return ""
		}
	}
	pub, err := url.Parse(s.cfg.PublicURL)
	if err != nil || pub.Host == "" {
		return ""
	}
	host := owner.Host
	if p := pub.Port(); p != "" {
		host = net.JoinHostPort(host, p)
	}
	return pub.Scheme + "://" + host + r.URL.RequestURI()
}

// ---- 节点暂时不可用标记 ----

// peerDown：探测判定失联，或刚转发失败（1 分钟内）。
func (m *Manager) peerDown(name string) bool {
	if m.healthDown(name) {
		return true
	}
	m.peerMu.Lock()
	defer m.peerMu.Unlock()
	return time.Now().Before(m.downUntil[name])
}

func (m *Manager) markPeerDown(name string) {
	m.peerMu.Lock()
	if m.downUntil == nil {
		m.downUntil = map[string]time.Time{}
	}
	m.downUntil[name] = time.Now().Add(time.Minute)
	m.peerMu.Unlock()
}

// ---- 转发标记：带签名，防止客户端伪造来源 IP 或造成转发环路 ----

func (m *Manager) hopSig(ts, ip, site, from string) string {
	m.mu.RLock()
	key := m.cfg.ClusterSecret
	m.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("hop|" + ts + "|" + ip + "|" + site + "|" + from))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:22]
}

func (m *Manager) makeHop(ip, site string) string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	from := m.selfName()
	return strings.Join([]string{ts, ip, site, url.QueryEscape(from), m.hopSig(ts, ip, site, from)}, "|")
}

// verifyHop 校验转发标记，返回原始客户端 IP 和来源节点名。
func (m *Manager) verifyHop(v, site string) (ip, from string, ok bool) {
	parts := strings.Split(v, "|")
	if len(parts) != 5 || parts[2] != site {
		return "", "", false
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Since(time.Unix(ts, 0)).Abs() > 2*time.Minute {
		return "", "", false
	}
	from, _ = url.QueryUnescape(parts[3])
	if !hmac.Equal([]byte(parts[4]), []byte(m.hopSig(parts[0], parts[1], site, from))) {
		return "", "", false
	}
	return parts[1], from, true
}

// ---- 转发 ----

// peerTLS 是连接其它节点时的 TLS 配置（测试里会设置 RootCAs）
var peerTLS = &tls.Config{}

// peerTransports 每个节点一个连接池。
// 转发请求的 URL 都是同一个公网域名（用于 SNI 和证书校验），如果共用一个 Transport，
// 连接池按域名复用连接，会把发给 A 的请求塞进连到 B 的空闲连接里，所以必须按节点地址分开。
var peerTransports sync.Map // addr → *http.Transport

func peerTransport(addr string) *http.Transport {
	if t, ok := peerTransports.Load(addr); ok {
		return t.(*http.Transport)
	}
	t := &http.Transport{
		DialContext: func(ctx context.Context, network, dialAddr string) (net.Conn, error) {
			target := addr
			if _, _, err := net.SplitHostPort(target); err != nil {
				_, port, _ := net.SplitHostPort(dialAddr)
				target = net.JoinHostPort(strings.Trim(target, "[]"), port)
			}
			return (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext(ctx, network, target)
		},
		TLSClientConfig:     peerTLS.Clone(),
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	actual, _ := peerTransports.LoadOrStore(addr, t)
	return actual.(*http.Transport)
}

func isDialErr(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// forward 把请求转给负责节点。返回 false 表示负责节点连不上，调用方应就地处理。
func (s *SiteRT) forward(w http.ResponseWriter, r *http.Request, owner Member, ip string, cw *countWriter) bool {
	pub, err := url.Parse(s.cfg.PublicURL)
	if err != nil || pub.Host == "" {
		return false
	}
	// 转发失败要就地处理时还得用到请求体（Transport 出错会关掉它），小请求体先缓存起来。
	// 大请求体（极少见，如上传）不缓存，转发失败直接报错。
	var body []byte
	canFallback := true
	if r.Body != nil && r.Body != http.NoBody && r.ContentLength != 0 {
		buf, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
		if err != nil {
			return true
		}
		if len(buf) > 1<<20 {
			canFallback = false
			r.Body = readCloser{io.MultiReader(bytes.NewReader(buf), r.Body), r.Body}
		} else {
			body = buf
			r.Body = io.NopCloser(bytes.NewReader(buf))
		}
	}
	fallback := false
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = pub.Scheme
			pr.Out.URL.Host = pub.Host
			pr.Out.Host = pub.Host
			pr.Out.Header.Set(hopHeader, s.m.makeHop(ip, s.cfg.ID))
		},
		Transport:     peerTransport(owner.Addr),
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if isDialErr(err) && canFallback {
				fallback = true
				s.m.markPeerDown(owner.Name)
				return
			}
			if r.Context().Err() != nil {
				cw.status = 499
				return
			}
			w.WriteHeader(http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(cw, r)
	if fallback && body != nil {
		r.Body = io.NopCloser(bytes.NewReader(body))
	}
	return !fallback
}
