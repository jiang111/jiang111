package main

import (
	"crypto/tls"
	"encoding/json"
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

	logs    *LogRing
	tr      *http.Transport
	trInsec *http.Transport
	saveMu  sync.Mutex
	started time.Time
}

func newTransport(insecure bool) *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           d.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          512,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 180 * time.Second, // 转码起播可能较慢
		ExpectContinueTimeout: time.Second,
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: insecure},
	}
}

func NewManager(path string, cfg *Config) *Manager {
	m := &Manager{
		path: path, cfg: cfg,
		listeners: map[int]*http.Server{},
		stats:     map[string]*Stats{},
		logs:      newLogRing(1000),
		tr:        newTransport(false),
		trInsec:   newTransport(true),
		started:   time.Now(),
	}
	m.loadStats()
	return m
}

func (m *Manager) transport(insecure bool) http.RoundTripper {
	if insecure {
		return m.trInsec
	}
	return m.tr
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
		ln, err := net.Listen("tcp", ":"+strconv.Itoa(port))
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
	m.mu.RLock()
	data, err := json.MarshalIndent(m.cfg, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		return err
	}
	m.saveMu.Lock()
	defer m.saveMu.Unlock()
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path)
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
	Requests int64 `json:"requests"`
	Bytes    int64 `json:"bytes"`
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
		m.stats[id] = st
	}
}

func (m *Manager) saveStats() {
	m.mu.RLock()
	out := map[string]savedStats{}
	for _, s := range m.cfg.Sites {
		if st := m.stats[s.ID]; st != nil {
			out[s.ID] = savedStats{st.Requests.Load(), st.Bytes.Load()}
		}
	}
	m.mu.RUnlock()
	data, _ := json.Marshal(out)
	tmp := m.statsPath() + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		os.Rename(tmp, m.statsPath())
	}
}
