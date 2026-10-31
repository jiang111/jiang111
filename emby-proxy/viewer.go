package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// 只读账号：给朋友 / 合租的人看站点地址、在线状态、线路延迟、流量和播放统计，
// 看不到上游地址、请求头、转发规则、Emby 账号、证书和任何设置，也不能做任何修改。
// 密码只在本机（不同步、不导出）；改动只读密码后所有只读登录立即失效。

type ctxKey int

const roleKey ctxKey = 0

func roleOf(r *http.Request) string {
	if v, ok := r.Context().Value(roleKey).(string); ok {
		return v
	}
	return ""
}

func (a *Admin) viewerKey() []byte {
	a.m.mu.RLock()
	defer a.m.mu.RUnlock()
	if len(a.m.cfg.ViewerPasswords) == 0 {
		return nil
	}
	return []byte(a.m.cfg.Secret + "|viewer|" + strings.Join(a.m.cfg.ViewerPasswords, "\n"))
}

func (a *Admin) signViewer(exp int64) string {
	mac := hmac.New(sha256.New, a.viewerKey())
	mac.Write([]byte("viewer|" + strconv.FormatInt(exp, 10)))
	return "v." + strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

// role 返回登录令牌对应的角色：admin / viewer，无效为空。
func (a *Admin) role(tok string) string {
	if a.valid(tok) {
		return "admin"
	}
	rest, ok := strings.CutPrefix(tok, "v.")
	if !ok || a.viewerKey() == nil {
		return ""
	}
	es, _, _ := strings.Cut(rest, ".")
	exp, err := strconv.ParseInt(es, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	if hmac.Equal([]byte(tok), []byte(a.signViewer(exp))) {
		return "viewer"
	}
	return ""
}

// authView 允许管理员和只读账号访问（只读的接口用）。
func (a *Admin) authView(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		role := ""
		if err == nil {
			role = a.role(c.Value)
		}
		if role == "" {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), roleKey, role)))
	}
}

// matchViewer 判断密码是不是某个只读密码。
func (a *Admin) matchViewer(pw string) bool {
	a.m.mu.RLock()
	defer a.m.mu.RUnlock()
	ok := false
	for _, v := range a.m.cfg.ViewerPasswords {
		if subtle.ConstantTimeCompare([]byte(pw), []byte(v)) == 1 {
			ok = true
		}
	}
	return ok
}

func (a *Admin) saveViewers(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Passwords []string `json:"passwords"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var list []string
	for _, p := range body.Passwords {
		p = strings.TrimSpace(p)
		if p == "" || slices.Contains(list, p) {
			continue
		}
		if len(p) < 6 {
			writeErr(w, 400, "只读密码至少 6 位")
			return
		}
		list = append(list, p)
	}
	m := a.m
	m.mu.Lock()
	if slices.Contains(list, m.cfg.AdminPassword) {
		m.mu.Unlock()
		writeErr(w, 400, "只读密码不能和面板密码相同")
		return
	}
	m.cfg.ViewerPasswords = list
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

var errViewer = errors.New("只读账号不能修改")

// viewerState 是只读账号看到的状态：去掉上游地址等一切敏感信息。
func (a *Admin) viewerState(w http.ResponseWriter) {
	m := a.m
	cluster := a.cluster.view()
	var snap *statsSnapshot
	if cluster.Role == "master" {
		snap = a.cluster.statsSnapshot()
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := []siteView{}
	for _, s := range m.cfg.Sites {
		if !s.Enabled {
			continue
		}
		pub := &Site{ID: s.ID, Name: s.Name, Enabled: true, Mode: s.Mode, Port: s.Port, Path: s.Path, Host: s.Host,
			PublicURL: s.PublicURL, AccessKey: s.AccessKey, Icon: s.Icon, StreamMode: s.StreamMode, Remark: s.Remark}
		v := siteView{Site: pub, Status: "disabled"}
		if st := m.stats[s.ID]; st != nil {
			v.Stats = st.view()
			v.Stats.IPs = nil
		}
		if snap != nil {
			cs, _ := snap.site(s.ID, cluster.SelfName, v.Stats, cluster.AdvertiseAddr != "")
			cs.IPs = nil
			v.ClusterStats = &cs
		}
		if m.sites[s.ID] != nil {
			v.Status = "running"
		}
		lv, _ := m.lineViews(s.ID)
		for i := range lv {
			lv[i].URL, lv[i].Err = "", ""
		}
		v.Lines = lv
		views = append(views, v)
	}
	writeJSON(w, 200, map[string]any{
		"role":     "viewer",
		"settings": map[string]any{"listen": m.cfg.Listen, "entry_url": m.cfg.EntryURL, "public_host": m.cfg.PublicHost},
		"sites":    views,
		"cluster":  map[string]any{"role": cluster.Role, "self_name": cluster.SelfName},
		"uptime":   int64(time.Since(m.started).Seconds()),
		"version":  appVersion(),
	})
}
