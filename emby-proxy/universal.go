package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

// 通用反代地址：不用在面板添加站点，客户端直接填
//   入口地址/密钥/https://emby.example.com:443
// 就能反代这台 Emby（地址改写、推流中转、访问控制都和普通站点一样）。
// 必须带密钥（防止别人拿你的服务器当公共反代），只在入口端口（路径 / 域名模式那个端口）上生效。
// 设置在主节点修改，同步给从节点。

type UniversalConfig struct {
	Enabled bool   `json:"enabled"`
	Key     string `json:"key"`
}

var uniRe = regexp.MustCompile(`^(https?):/{1,2}([^/?#]+)`)

type uniCache struct {
	mu    sync.Mutex
	sites map[string]*SiteRT
	stats map[string]*Stats
}

func (c *UniversalConfig) normalize() error {
	c.Key = strings.Trim(strings.TrimSpace(c.Key), "/")
	if c.Enabled && c.Key == "" {
		c.Key = randPassword(10)
	}
	if c.Key != "" && !accessKeyRe.MatchString(c.Key) || c.Enabled && len(c.Key) < 6 {
		return errors.New("密钥至少 6 位，只能是字母、数字、_ 或 -")
	}
	return nil
}

// serveUniversal 处理 /密钥/https://host:port/... 形式的请求，不是这种格式返回 false。
func (m *Manager) serveUniversal(w http.ResponseWriter, r *http.Request, esc string) bool {
	m.mu.RLock()
	u := m.cfg.Universal
	m.mu.RUnlock()
	if u == nil || !u.Enabled || u.Key == "" {
		return false
	}
	rest, ok := strings.CutPrefix(esc, "/"+u.Key+"/")
	if !ok {
		return false
	}
	mt := uniRe.FindStringSubmatch(rest)
	if mt == nil {
		return false
	}
	up, err := parseUpstream(mt[1] + "://" + mt[2])
	if err != nil {
		return false
	}
	prefix := "/" + u.Key + "/" + mt[0]
	rt := m.uniSite(up.Scheme + "://" + up.Host)
	if rt == nil {
		return false
	}
	rt.serve(w, r, prefix)
	return true
}

// uniSite 返回（必要时创建）通用地址对应的临时站点。
func (m *Manager) uniSite(upstream string) *SiteRT {
	sum := sha256.Sum256([]byte(upstream))
	id := "u" + hex.EncodeToString(sum[:4])
	m.uni.mu.Lock()
	defer m.uni.mu.Unlock()
	if rt := m.uni.sites[id]; rt != nil {
		return rt
	}
	if m.uni.sites == nil || len(m.uni.sites) >= 64 {
		m.uni.sites = map[string]*SiteRT{}
	}
	if m.uni.stats == nil {
		m.uni.stats = map[string]*Stats{}
	}
	st := m.uni.stats[id]
	if st == nil {
		st = &Stats{}
		m.uni.stats[id] = st
	}
	host := upstream[strings.Index(upstream, "://")+3:]
	s := Site{ID: id, Name: "通用 " + host, Enabled: true, Upstream: upstream, Mode: "path", Path: id, StreamMode: "proxy"}
	m.mu.RLock()
	rt, err := newSiteRT(m, s, st)
	if err == nil {
		rt.epKey = []byte(m.cfg.ClusterSecret)
	}
	m.mu.RUnlock()
	if err != nil {
		return nil
	}
	m.uni.sites[id] = rt
	return rt
}

func (a *Admin) saveUniversal(w http.ResponseWriter, r *http.Request) {
	var c UniversalConfig
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if err := c.normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	m := a.m
	m.mu.Lock()
	m.cfg.Universal = &c
	m.mu.Unlock()
	m.uni.mu.Lock()
	m.uni.sites = nil
	m.uni.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": c.Key})
}
