package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// 多服务器同步：一台做主节点，其余做从节点。
// 从节点每 30 秒带令牌请求主节点的 /api/cluster/sync，内容有变化就整体替换站点和设置，
// 连同推流签名密钥和上传的证书一起同步；面板密码、监听端口、同步设置本身各节点独立。
// 同步内容包含证书私钥和签名密钥，主节点地址必须是 HTTPS（面板 HTTPS 地址，见 nginx 配置）。

type ClusterConfig struct {
	Role      string `json:"role,omitempty"`       // ""=单机 | master | node
	Token     string `json:"token,omitempty"`      // 主节点：同步令牌；从节点：主节点给的令牌
	MasterURL string `json:"master_url,omitempty"` // 从节点：主节点面板地址，如 https://master.example.com:8443
	NodeName  string `json:"node_name,omitempty"`  // 从节点：在主节点上显示的名字
}

// syncPayload 是主节点下发的内容。
type syncPayload struct {
	Sites         []*Site `json:"sites"`
	EntryURL      string  `json:"entry_url,omitempty"`
	PublicHost    string  `json:"public_host,omitempty"`
	PortBind      string  `json:"port_bind,omitempty"`
	NginxCert     string  `json:"nginx_cert,omitempty"` // 主节点手动指定的证书路径（各节点需自备同路径文件）
	NginxKey      string  `json:"nginx_key,omitempty"`
	NginxIPv6     bool    `json:"nginx_ipv6,omitempty"`
	ClusterSecret string  `json:"cluster_secret"`
	CertPEM       []byte  `json:"cert_pem,omitempty"` // 主节点上传的证书（各节点写到自己的数据目录）
	KeyPEM        []byte  `json:"key_pem,omitempty"`
}

type nodeInfo struct {
	Name     string `json:"name"`
	IP       string `json:"ip"`
	LastSeen int64  `json:"last_seen"`
	Version  string `json:"version"` // 节点当前持有的配置版本
	Current  bool   `json:"current"` // 是否已是最新
}

type syncStatus struct {
	LastTry int64  `json:"last_try"`
	LastOK  int64  `json:"last_ok"`
	Version string `json:"version"`
	Error   string `json:"error,omitempty"`
	Changed int64  `json:"changed"` // 上次实际应用新配置的时间
}

type Cluster struct {
	m      *Manager
	mu     sync.Mutex
	nodes  map[string]*nodeInfo // 主节点：已连接的从节点
	status syncStatus           // 从节点：同步状态
	kick   chan struct{}
}

func newCluster(m *Manager) *Cluster {
	return &Cluster{m: m, nodes: map[string]*nodeInfo{}, kick: make(chan struct{}, 1)}
}

func (m *Manager) isNode() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Cluster.Role == "node"
}

// payload 生成要下发的内容和它的版本号（内容哈希）。
func (c *Cluster) payload() ([]byte, string, error) {
	m := c.m
	cert, _ := os.ReadFile(filepath.Join(m.sslDir(), "fullchain.pem"))
	key, _ := os.ReadFile(filepath.Join(m.sslDir(), "privkey.pem"))
	m.mu.RLock()
	p := syncPayload{
		Sites: m.cfg.Sites, EntryURL: m.cfg.EntryURL, PublicHost: m.cfg.PublicHost, PortBind: m.cfg.PortBind,
		ClusterSecret: m.cfg.ClusterSecret, NginxIPv6: m.cfg.NginxIPv6,
	}
	// 用的是上传的证书就把文件发下去；手动路径则只发路径
	hostCert := filepath.Join(m.hostSSLDir(), "fullchain.pem")
	if m.cfg.NginxCert == "" || m.cfg.NginxCert == hostCert {
		if len(cert) > 0 && len(key) > 0 {
			p.CertPEM, p.KeyPEM = cert, key
		}
	} else {
		p.NginxCert, p.NginxKey = m.cfg.NginxCert, m.cfg.NginxKey
	}
	data, err := json.Marshal(p)
	m.mu.RUnlock()
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(data)
	return data, hex.EncodeToString(sum[:8]), nil
}

// serveSync 是主节点的同步接口，用令牌鉴权（不走面板登录）。
func (c *Cluster) serveSync(w http.ResponseWriter, r *http.Request) {
	m := c.m
	m.mu.RLock()
	role, token := m.cfg.Cluster.Role, m.cfg.Cluster.Token
	m.mu.RUnlock()
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if role != "master" || token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		time.Sleep(300 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "同步令牌不正确，或这台服务器不是主节点")
		return
	}
	data, ver, err := c.payload()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	name := strings.TrimSpace(r.Header.Get("X-Node-Name"))
	if name == "" {
		name = clientIP(r)
	}
	if len(name) > 64 {
		name = name[:64]
	}
	// 记录节点自己报告持有的版本（节点应用新配置后会马上再请求一次，报告新版本）
	have := strings.Trim(r.Header.Get("If-None-Match"), `"`)
	c.mu.Lock()
	if len(c.nodes) < 200 || c.nodes[name] != nil {
		c.nodes[name] = &nodeInfo{Name: name, IP: clientIP(r), LastSeen: time.Now().Unix(), Version: have}
	}
	c.mu.Unlock()
	w.Header().Set("ETag", `"`+ver+`"`)
	w.Header().Set("Cache-Control", "no-store")
	if have == ver {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func (c *Cluster) nodeList(current string) []nodeInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []nodeInfo{}
	for _, n := range c.nodes {
		v := *n
		v.Current = v.Version == current
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// run 是从节点的同步循环。
func (c *Cluster) run() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if c.m.isNode() {
			c.syncOnce()
		}
		select {
		case <-t.C:
		case <-c.kick:
		}
	}
}

func (c *Cluster) syncNow() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

var syncClient = &http.Client{Timeout: 30 * time.Second}

func (c *Cluster) syncOnce() {
	err := c.pull()
	c.mu.Lock()
	c.status.LastTry = time.Now().Unix()
	if err != nil {
		c.status.Error = err.Error()
	} else {
		c.status.Error = ""
		c.status.LastOK = c.status.LastTry
	}
	c.mu.Unlock()
	if err != nil {
		log.Printf("从主节点同步失败: %v", err)
	}
}

func (c *Cluster) pull() error {
	m := c.m
	m.mu.RLock()
	cc := m.cfg.Cluster
	m.mu.RUnlock()
	if cc.MasterURL == "" || cc.Token == "" {
		return errors.New("未填写主节点地址或同步令牌")
	}
	req, err := http.NewRequest("GET", strings.TrimRight(cc.MasterURL, "/")+"/api/cluster/sync", nil)
	if err != nil {
		return fmt.Errorf("主节点地址不正确: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cc.Token)
	name := cc.NodeName
	if name == "" {
		name, _ = os.Hostname()
	}
	req.Header.Set("X-Node-Name", name)
	c.mu.Lock()
	if c.status.Version != "" {
		req.Header.Set("If-None-Match", `"`+c.status.Version+`"`)
	}
	c.mu.Unlock()
	resp, err := syncClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接主节点失败: %s", simplifyNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		var e struct{ Error string }
		json.Unmarshal(body, &e)
		if e.Error == "" {
			e.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return errors.New("主节点拒绝: " + e.Error)
	}
	var p syncPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return fmt.Errorf("主节点返回内容无法解析: %v", err)
	}
	if err := c.apply(&p); err != nil {
		return err
	}
	c.mu.Lock()
	c.status.Version = strings.Trim(resp.Header.Get("ETag"), `"`)
	c.status.Changed = time.Now().Unix()
	c.mu.Unlock()
	log.Printf("已从主节点同步配置（%d 个站点）", len(p.Sites))
	c.syncNow() // 马上再请求一次：带上新版本号，主节点据此显示本机「已是最新」
	return nil
}

// apply 把主节点下发的内容应用到本机。
func (c *Cluster) apply(p *syncPayload) error {
	m := c.m
	if p.ClusterSecret == "" {
		return errors.New("主节点返回内容缺少签名密钥")
	}
	if !validBind(p.PortBind) {
		return errors.New("主节点的 port_bind 无效")
	}
	m.mu.RLock()
	tmp := &Config{Listen: m.cfg.Listen, AdminListen: m.cfg.AdminListen}
	m.mu.RUnlock()
	for _, s := range p.Sites {
		if s == nil {
			return errors.New("主节点返回了空站点")
		}
		tmp.Sites = append(tmp.Sites, s)
	}
	for _, s := range tmp.Sites {
		if err := validateSite(tmp, s); err != nil {
			return fmt.Errorf("站点「%s」在本机无效: %v（检查是否与本机面板/入口端口冲突）", s.Name, err)
		}
	}
	if len(p.CertPEM) > 0 {
		if _, err := parseLeaf(p.CertPEM, p.KeyPEM); err != nil {
			return fmt.Errorf("主节点下发的证书无效: %v", err)
		}
		if err := os.MkdirAll(m.sslDir(), 0o700); err != nil {
			return err
		}
		if old, _ := os.ReadFile(filepath.Join(m.sslDir(), "fullchain.pem")); !bytes.Equal(old, p.CertPEM) {
			if err := writeFileAtomic(filepath.Join(m.sslDir(), "privkey.pem"), p.KeyPEM); err != nil {
				return err
			}
			if err := writeFileAtomic(filepath.Join(m.sslDir(), "fullchain.pem"), p.CertPEM); err != nil {
				return err
			}
		}
	}

	m.mu.Lock()
	m.cfg.Sites = tmp.Sites
	m.cfg.EntryURL, m.cfg.PublicHost = p.EntryURL, p.PublicHost
	m.cfg.ClusterSecret = p.ClusterSecret
	m.cfg.NginxIPv6 = p.NginxIPv6
	if len(p.CertPEM) > 0 {
		m.cfg.NginxCert, m.cfg.NginxKey = filepath.Join(m.hostSSLDir(), "fullchain.pem"), filepath.Join(m.hostSSLDir(), "privkey.pem")
	} else {
		m.cfg.NginxCert, m.cfg.NginxKey = p.NginxCert, p.NginxKey
	}
	if p.PortBind != m.cfg.PortBind {
		m.cfg.PortBind = p.PortBind
		m.rebindAll()
	} else {
		m.reload()
	}
	m.mu.Unlock()
	return m.save()
}

// ---- 面板接口 ----

type clusterView struct {
	Role      string      `json:"role"`
	Token     string      `json:"token,omitempty"`
	MasterURL string      `json:"master_url,omitempty"`
	NodeName  string      `json:"node_name,omitempty"`
	PanelURL  string      `json:"panel_url,omitempty"`
	Version   string      `json:"version,omitempty"`
	Nodes     []nodeInfo  `json:"nodes,omitempty"`
	Status    *syncStatus `json:"status,omitempty"`
}

func (c *Cluster) view() clusterView {
	m := c.m
	m.mu.RLock()
	cc := m.cfg.Cluster
	v := clusterView{Role: cc.Role, MasterURL: cc.MasterURL, NodeName: cc.NodeName, PanelURL: m.cfg.PanelURL}
	m.mu.RUnlock()
	switch cc.Role {
	case "master":
		v.Token = cc.Token
		_, ver, _ := c.payload()
		v.Version = ver
		v.Nodes = c.nodeList(ver)
	case "node":
		c.mu.Lock()
		st := c.status
		c.mu.Unlock()
		v.Status = &st
		if cc.Token != "" {
			v.Token = cc.Token[:4] + "…"
		}
	}
	return v
}

func (a *Admin) clusterSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Role          string  `json:"role"`
		MasterURL     string  `json:"master_url"`
		Token         string  `json:"token"`
		NodeName      string  `json:"node_name"`
		PanelURL      *string `json:"panel_url"`
		NewToken      bool    `json:"new_token"`
		AllowInsecure bool    `json:"allow_http"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	switch body.Role {
	case "", "master", "node":
	default:
		writeErr(w, 400, "未知的角色")
		return
	}
	body.MasterURL = strings.TrimRight(strings.TrimSpace(body.MasterURL), "/")
	body.Token = strings.TrimSpace(body.Token)
	if body.Role == "node" {
		u, err := url.Parse(body.MasterURL)
		if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
			writeErr(w, 400, "主节点地址格式不正确，例如 https://master.example.com:8443")
			return
		}
		if u.Scheme == "http" && !body.AllowInsecure {
			writeErr(w, 400, "同步内容包含证书私钥和签名密钥，主节点地址必须是 https://")
			return
		}
	}
	var panelURL string
	if body.PanelURL != nil {
		panelURL = strings.TrimRight(strings.TrimSpace(*body.PanelURL), "/")
		if panelURL != "" {
			u, err := url.Parse(panelURL)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.Path != "" {
				writeErr(w, 400, "面板 HTTPS 地址格式不正确，例如 https://master.example.com:8443")
				return
			}
		}
	}
	m := a.m
	m.mu.Lock()
	cc := &m.cfg.Cluster
	prevRole := cc.Role
	cc.Role = body.Role
	switch body.Role {
	case "master":
		if prevRole != "master" || cc.Token == "" || body.NewToken {
			cc.Token = randHex(16)
		}
		cc.MasterURL, cc.NodeName = "", ""
	case "node":
		cc.MasterURL, cc.NodeName = body.MasterURL, strings.TrimSpace(body.NodeName)
		if body.Token != "" {
			cc.Token = body.Token
		} else if prevRole != "node" {
			m.mu.Unlock()
			writeErr(w, 400, "请填写主节点上显示的同步令牌")
			return
		}
	default:
		cc.Token, cc.MasterURL, cc.NodeName = "", "", ""
	}
	if body.PanelURL != nil {
		m.cfg.PanelURL = panelURL
	}
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if body.Role == "node" {
		a.cluster.mu.Lock()
		a.cluster.status = syncStatus{}
		a.cluster.mu.Unlock()
		a.cluster.syncNow()
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// nodeReadOnly 在从节点上拒绝修改站点和同步内容（会被主节点覆盖）。
func (a *Admin) nodeReadOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if a.m.isNode() {
			writeErr(w, http.StatusConflict, "这台服务器是从节点，配置由主节点同步，请到主节点上修改")
			return
		}
		h(w, r)
	}
}
