package main

import (
	"net/http"
	"slices"
	"time"
)

// accessView 是面板「访问控制」页的数据。
type accessView struct {
	Config  AccessConfig `json:"config"`
	Geo     geoView      `json:"geo"`
	Bans    []banEntry   `json:"bans"`
	Catches []catchEntry `json:"catches"`
	MyIP    string       `json:"my_ip"`
	MyCC    string       `json:"my_cc,omitempty"`
}

func (a *Admin) accessState(w http.ResponseWriter, r *http.Request) {
	m := a.m
	m.mu.RLock()
	c := AccessConfig{Mode: "off", BanHits: 10, BanMinutes: 1440}
	if m.cfg.Access != nil {
		c = *m.cfg.Access
	}
	m.mu.RUnlock()
	bans := m.bans.list(time.Now())
	for i := range bans {
		bans[i].CC = m.geoLookupStr(bans[i].IP)
	}
	ip := clientIP(r)
	writeJSON(w, 200, accessView{Config: c, Geo: m.geoView(), Bans: bans, Catches: m.catches.all(), MyIP: ip, MyCC: m.geoLookupStr(ip)})
}

func (a *Admin) saveAccess(w http.ResponseWriter, r *http.Request) {
	var c AccessConfig
	if err := readJSON(r, &c); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if err := c.normalize(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 开白名单时把正在操作的人自动加进信任 IP，免得把自己挡在外面（面板不受影响，但自己的播放器会被拦）
	if c.Mode == "allow" {
		if ip := clientIP(r); ip != "" && !slices.Contains(c.Trusted, ip) {
			if n, err := parseNet(ip); err == nil && !inNets(n.IP, parseNets(c.Trusted)) && !inNets(n.IP, parseNets(c.IPs)) {
				c.Trusted = append(c.Trusted, ip)
			}
		}
	}
	m := a.m
	m.mu.Lock()
	m.cfg.Access = &c
	m.acl.Store(newACL(&c))
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	go m.geoTick(time.Now())
	writeJSON(w, 200, map[string]any{"ok": true, "config": c})
}

func (a *Admin) unban(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IP string `json:"ip"`
	}
	if err := readJSON(r, &body); err != nil || body.IP == "" {
		writeErr(w, 400, "请求格式错误")
		return
	}
	a.m.bans.unban(body.IP)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) clearCatches(w http.ResponseWriter, r *http.Request) {
	a.m.catches.clear()
	writeJSON(w, 200, map[string]any{"ok": true})
}
