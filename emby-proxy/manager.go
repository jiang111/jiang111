package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	path string

	mu        sync.RWMutex
	cfg       *Config
	sites     map[string]*SiteRT
	byPath    map[string]*SiteRT
	byHost    map[string]*SiteRT
	byPort    map[int]*SiteRT
	siteErr   map[string]string
	listeners map[int]*http.Server
	stats     map[string]*Stats
	traffic   monthTraffic // 本月反代流量（流量均衡用）
	fo        failover     // 故障接管：各节点探测状态

	logs    *LogRing
	trs     [3]*http.Transport // 按 tlsVerify / tlsIgnoreExpired / tlsInsecure
	saveMu  sync.Mutex
	bg      sync.WaitGroup // 后台保存（发现推流域名时）
	cluster *Cluster

	peerMu    sync.Mutex
	downUntil map[string]time.Time // 暂时连不上的节点（同一用户固定同一台）

	certMu   sync.Mutex
	certKey  string
	certLeaf *x509.Certificate
	started  time.Time

	acctMu  sync.Mutex
	acctKey []byte // 站点账号的加密密钥（data/account.key）

	ka *keepalive // 自动保号

	lines lineHub     // 多线路：各线路的探测状态和正在用的线路
	imgs  *imageCache // 海报 / 图片缓存
}

// TLS 校验方式
const (
	tlsVerify        = iota // 正常校验
	tlsIgnoreExpired        // 校验域名和证书链，只忽略过期
	tlsInsecure             // 完全不校验
)

// testRoots 仅测试用（nil 表示系统根证书）
var testRoots *x509.CertPool

func tlsConfig(mode int) *tls.Config {
	switch mode {
	case tlsInsecure:
		return &tls.Config{InsecureSkipVerify: true}
	case tlsIgnoreExpired:
		// 走 HTTP 代理时 Transport 用这里的配置；直连时用 newTransport 里的 DialTLSContext（能拿到 IP 主机名）
		return &tls.Config{InsecureSkipVerify: true, VerifyConnection: func(cs tls.ConnectionState) error {
			return verifyIgnoringExpiry(cs, cs.ServerName)
		}}
	}
	return &tls.Config{}
}

// verifyIgnoringExpiry 校验证书链和主机名，只是把校验时间放到证书有效期内。
// host 为空时拒绝（不能在不知道主机名的情况下放行）。
func verifyIgnoringExpiry(cs tls.ConnectionState, host string) error {
	if host == "" {
		return errors.New("无法确定上游主机名，不能只忽略过期，请改用「完全不校验证书」")
	}
	if len(cs.PeerCertificates) == 0 {
		return errors.New("上游没有提供证书")
	}
	leaf := cs.PeerCertificates[0]
	at := time.Now()
	if at.After(leaf.NotAfter) {
		at = leaf.NotAfter.Add(-time.Second)
	}
	if at.Before(leaf.NotBefore) {
		at = leaf.NotBefore.Add(time.Second)
	}
	inter := x509.NewCertPool()
	for _, c := range cs.PeerCertificates[1:] {
		inter.AddCert(c)
	}
	_, err := leaf.Verify(x509.VerifyOptions{DNSName: host, Intermediates: inter, CurrentTime: at, Roots: testRoots})
	return err
}

func newTransport(mode int) *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second, // 转码起播可能较慢
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       tlsConfig(mode),
	}
	if mode == tlsIgnoreExpired {
		// Go 对 IP 地址不填 ServerName，自己握手才能拿到要校验的主机名。
		// 只协商 http/1.1：自定义握手协商出 h2 会影响 WebSocket 等需要 HTTP/1 的请求。
		tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			raw, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			cfg := &tls.Config{InsecureSkipVerify: true, NextProtos: []string{"http/1.1"},
				VerifyConnection: func(cs tls.ConnectionState) error { return verifyIgnoringExpiry(cs, host) }}
			if net.ParseIP(host) == nil {
				cfg.ServerName = host
			}
			c := tls.Client(raw, cfg)
			if err := c.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, err
			}
			return c, nil
		}
	}
	return tr
}

func NewManager(path string, cfg *Config) *Manager {
	m := &Manager{
		path: path, cfg: cfg,
		listeners: map[int]*http.Server{},
		stats:     map[string]*Stats{},
		logs:      newLogRing(1000),
		trs:       [3]*http.Transport{newTransport(tlsVerify), newTransport(tlsIgnoreExpired), newTransport(tlsInsecure)},
		started:   time.Now(),
	}
	m.loadStats()
	m.loadTraffic()
	m.ka = newKeepalive(m)
	m.imgs = newImageCache(imageCacheBytes())
	m.lines.notify = func(text string) {
		go func() {
			cfg := m.keepaliveConfig()
			if cfg.TelegramToken != "" && cfg.TelegramChat != "" {
				if err := telegramSend(cfg.TelegramToken, cfg.TelegramChat, text); err != nil {
					log.Printf("Telegram 通知失败：%v", err)
				}
			}
		}()
	}
	return m
}

func (m *Manager) transport(insecure, ignoreExpired bool) http.RoundTripper {
	switch {
	case insecure:
		return m.trs[tlsInsecure]
	case ignoreExpired:
		return m.trs[tlsIgnoreExpired]
	}
	return m.trs[tlsVerify]
}

// reload 根据当前配置重建路由表并增减端口监听。调用方需持有 m.mu 写锁。
func (m *Manager) reload() {
	sites := map[string]*SiteRT{}
	byPath := map[string]*SiteRT{}
	byHost := map[string]*SiteRT{}
	byPort := map[int]*SiteRT{}
	siteErr := map[string]string{}
	for _, sc := range m.cfg.Sites {
		st := m.stats[sc.ID]
		if st == nil {
			st = &Stats{}
			m.stats[sc.ID] = st
		}
		if !sc.Enabled {
			continue
		}
		rt, err := newSiteRT(m, *sc, st)
		if err != nil {
			siteErr[sc.ID] = err.Error()
			continue
		}
		rt.epKey = []byte(m.cfg.ClusterSecret)
		sites[sc.ID] = rt
		switch sc.Mode {
		case "port":
			byPort[sc.Port] = rt
		case "path":
			byPath[strings.ToLower(sc.Path)] = rt
		case "host":
			byHost[sc.Host] = rt
		}
	}
	for port, srv := range m.listeners {
		if byPort[port] == nil {
			srv.Close()
			delete(m.listeners, port)
			log.Printf("关闭端口 %d", port)
		}
	}
	for port, rt := range byPort {
		if m.listeners[port] != nil {
			continue
		}
		ln, err := net.Listen("tcp", net.JoinHostPort(m.cfg.PortBind, strconv.Itoa(port)))
		if err != nil {
			siteErr[rt.cfg.ID] = "端口监听失败: " + err.Error()
			delete(sites, rt.cfg.ID)
			delete(byPort, port)
			log.Printf("站点 %s 监听端口 %d 失败: %v", rt.cfg.Name, port, err)
			continue
		}
		srv := newServer(m.portHandler(port))
		m.listeners[port] = srv
		go srv.Serve(ln)
		log.Printf("站点 %s 监听端口 %d → %s", rt.cfg.Name, port, rt.cfg.Upstream)
	}
	m.sites, m.byPath, m.byHost, m.byPort, m.siteErr = sites, byPath, byHost, byPort, siteErr
}

// rebindAll 关闭所有独立端口后重新监听（监听地址变化时用）。调用方需持有 m.mu 写锁。
func (m *Manager) rebindAll() {
	for port, srv := range m.listeners {
		srv.Close()
		delete(m.listeners, port)
	}
	m.reload()
}

func newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       180 * time.Second,
		ErrorLog:          log.New(discard{}, "", 0),
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

func (m *Manager) portHandler(port int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.RLock()
		rt := m.byPort[port]
		m.mu.RUnlock()
		if rt == nil {
			http.Error(w, "site disabled", http.StatusServiceUnavailable)
			return
		}
		rt.serve(w, r, "")
	})
}

// ServeHTTP 是共用入口：先按域名匹配，再按第一段路径匹配。
func (m *Manager) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/__health" {
		w.Write([]byte("ok"))
		return
	}
	host := firstHeader(r, "X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	host, _ = splitHP(host, "http")
	esc := r.URL.EscapedPath()
	seg := strings.TrimPrefix(esc, "/")
	if i := strings.IndexByte(seg, '/'); i >= 0 {
		seg = seg[:i]
	}
	m.mu.RLock()
	rt := m.byHost[host]
	prefix := ""
	if rt == nil && seg != "" {
		if rt = m.byPath[strings.ToLower(seg)]; rt != nil {
			prefix = "/" + seg
		}
	}
	m.mu.RUnlock()
	if rt == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte("emby-proxy: 没有匹配的站点（检查域名或路径前缀）\n"))
		return
	}
	rt.serve(w, r, prefix)
}

func (m *Manager) save() error {
	// saveMu 包住「序列化 + 写盘」，避免两个并发保存乱序写入、旧快照覆盖新快照
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	m.mu.RLock()
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	if err := writeFileAtomic(m.path, data); err != nil {
		return err
	}
	m.writeNginxConf()
	return nil
}

func (m *Manager) persistStreamHost(id, key string) {
	m.mu.Lock()
	added := false
	for _, s := range m.cfg.Sites {
		if s.ID != id {
			continue
		}
		found := false
		for _, h := range s.StreamHosts {
			if h == key {
				found = true
				break
			}
		}
		if !found && len(s.StreamHosts) < 100 {
			s.StreamHosts = append(s.StreamHosts, key)
			added = true
			log.Printf("站点 %s 发现推流域名 %s", s.Name, key)
		}
	}
	m.mu.Unlock()
	if added {
		if err := m.save(); err != nil {
			log.Printf("保存配置失败: %v", err)
		}
	}
}

// ---- 累计流量持久化 ----

type savedStats struct {
	Requests      int64 `json:"requests"`
	Bytes         int64 `json:"bytes"`
	RelayRequests int64 `json:"relay_requests,omitempty"`
	RelayBytes    int64 `json:"relay_bytes,omitempty"`
}

func (m *Manager) statsPath() string { return filepath.Join(filepath.Dir(m.path), "stats.json") }

func (m *Manager) loadStats() {
	data, err := os.ReadFile(m.statsPath())
	if err != nil {
		return
	}
	var saved map[string]savedStats
	if json.Unmarshal(data, &saved) != nil {
		return
	}
	for id, v := range saved {
		st := &Stats{}
		st.Requests.Store(v.Requests)
		st.Bytes.Store(v.Bytes)
		st.RelayRequests.Store(v.RelayRequests)
		st.RelayBytes.Store(v.RelayBytes)
		m.stats[id] = st
	}
}

func (m *Manager) saveStats() {
	m.mu.RLock()
	out := map[string]savedStats{}
	for _, s := range m.cfg.Sites {
		if st := m.stats[s.ID]; st != nil {
			out[s.ID] = savedStats{st.Requests.Load(), st.Bytes.Load(), st.RelayRequests.Load(), st.RelayBytes.Load()}
		}
	}
	m.mu.RUnlock()
	data, _ := json.Marshal(out)
	writeFileAtomic(m.statsPath(), data)
}
