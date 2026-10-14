package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cookieName = "emby_proxy_session"

const testUA = "SenPlayer/6.2.1 (iPhone; iOS 18.0; Scale/3.00)"

type Admin struct {
	m        *Manager
	failMu   sync.Mutex
	failures map[string][]time.Time
}

func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(indexHTML)
	})
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/state", a.auth(a.state))
	mux.HandleFunc("GET /api/logs", a.auth(a.logsAPI))
	mux.HandleFunc("POST /api/sites", a.auth(a.saveSite))
	mux.HandleFunc("PUT /api/sites/{id}", a.auth(a.saveSite))
	mux.HandleFunc("DELETE /api/sites/{id}", a.auth(a.deleteSite))
	mux.HandleFunc("POST /api/sites/{id}/clear-stats", a.auth(a.clearStats))
	mux.HandleFunc("POST /api/test", a.auth(a.test))
	mux.HandleFunc("PUT /api/settings", a.auth(a.settings))
	return securityHeaders(mux)
}

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func (a *Admin) sign(exp int64) string {
	a.m.mu.RLock()
	key := a.m.cfg.Secret + "|" + a.m.cfg.AdminPassword
	a.m.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return strconv.FormatInt(exp, 10) + "." + hex.EncodeToString(mac.Sum(nil))
}

func (a *Admin) valid(tok string) bool {
	es, _, ok := strings.Cut(tok, ".")
	if !ok {
		return false
	}
	exp, err := strconv.ParseInt(es, 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return false
	}
	return hmac.Equal([]byte(tok), []byte(a.sign(exp)))
}

func (a *Admin) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(cookieName)
		if err != nil || !a.valid(c.Value) {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		h(w, r)
	}
}

func remoteIP(r *http.Request) string {
	return clientIP(r)
}

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	ip := remoteIP(r)
	a.failMu.Lock()
	var recent []time.Time
	for _, t := range a.failures[ip] {
		if time.Since(t) < 10*time.Minute {
			recent = append(recent, t)
		}
	}
	a.failures[ip] = recent
	a.failMu.Unlock()
	if len(recent) >= 5 {
		writeErr(w, http.StatusTooManyRequests, "失败次数过多，请 10 分钟后再试")
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	a.m.mu.RLock()
	pw := a.m.cfg.AdminPassword
	a.m.mu.RUnlock()
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(pw)) != 1 {
		a.failMu.Lock()
		a.failures[ip] = append(a.failures[ip], time.Now())
		a.failMu.Unlock()
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: a.sign(exp), Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 3600,
		Secure: r.TLS != nil || strings.EqualFold(firstHeader(r, "X-Forwarded-Proto"), "https"),
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

type siteView struct {
	*Site
	Stats  StatsView `json:"stats"`
	Status string    `json:"status"`
	Error  string    `json:"error,omitempty"`
}

func (a *Admin) state(w http.ResponseWriter, r *http.Request) {
	m := a.m
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := []siteView{}
	for _, s := range m.cfg.Sites {
		v := siteView{Site: s, Status: "disabled"}
		if st := m.stats[s.ID]; st != nil {
			v.Stats = st.view()
		}
		if e := m.siteErr[s.ID]; e != "" {
			v.Status, v.Error = "error", e
		} else if m.sites[s.ID] != nil {
			v.Status = "running"
		}
		views = append(views, v)
	}
	writeJSON(w, 200, map[string]any{
		"settings": map[string]any{
			"listen": m.cfg.Listen, "admin_listen": m.cfg.AdminListen,
			"entry_url": m.cfg.EntryURL, "public_host": m.cfg.PublicHost,
		},
		"sites":   views,
		"uptime":  int64(time.Since(m.started).Seconds()),
		"version": version,
	})
}

func (a *Admin) logsAPI(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	writeJSON(w, 200, a.m.logs.list(r.URL.Query().Get("site"), limit))
}

func (a *Admin) saveSite(w http.ResponseWriter, r *http.Request) {
	var s Site
	if err := readJSON(r, &s); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	m := a.m
	m.mu.Lock()
	id := r.PathValue("id")
	idx := -1
	if id != "" {
		for i, o := range m.cfg.Sites {
			if o.ID == id {
				idx = i
			}
		}
		if idx < 0 {
			m.mu.Unlock()
			writeErr(w, 404, "站点不存在")
			return
		}
		s.ID = id
	} else {
		s.ID = randHex(4)
	}
	if err := validateSite(m.cfg, &s); err != nil {
		m.mu.Unlock()
		writeErr(w, 400, err.Error())
		return
	}
	if idx >= 0 {
		m.cfg.Sites[idx] = &s
	} else {
		m.cfg.Sites = append(m.cfg.Sites, &s)
	}
	m.reload()
	siteErr := m.siteErr[s.ID]
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "id": s.ID, "error": siteErr})
}

func (a *Admin) deleteSite(w http.ResponseWriter, r *http.Request) {
	m := a.m
	id := r.PathValue("id")
	m.mu.Lock()
	sites := m.cfg.Sites[:0]
	for _, s := range m.cfg.Sites {
		if s.ID != id {
			sites = append(sites, s)
		}
	}
	m.cfg.Sites = sites
	delete(m.stats, id)
	m.reload()
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) clearStats(w http.ResponseWriter, r *http.Request) {
	m := a.m
	m.mu.RLock()
	st := m.stats[r.PathValue("id")]
	m.mu.RUnlock()
	if st != nil {
		st.Requests.Store(0)
		st.Bytes.Store(0)
		st.Errors.Store(0)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) settings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EntryURL    *string `json:"entry_url"`
		PublicHost  *string `json:"public_host"`
		OldPassword string  `json:"old_password"`
		NewPassword string  `json:"new_password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	m := a.m
	m.mu.Lock()
	if body.NewPassword != "" {
		if subtle.ConstantTimeCompare([]byte(body.OldPassword), []byte(m.cfg.AdminPassword)) != 1 {
			m.mu.Unlock()
			writeErr(w, 400, "原密码错误")
			return
		}
		if len(body.NewPassword) < 6 {
			m.mu.Unlock()
			writeErr(w, 400, "新密码至少 6 位")
			return
		}
		m.cfg.AdminPassword = body.NewPassword
	}
	if body.EntryURL != nil {
		m.cfg.EntryURL = strings.TrimRight(strings.TrimSpace(*body.EntryURL), "/")
	}
	if body.PublicHost != nil {
		m.cfg.PublicHost = strings.TrimSpace(*body.PublicHost)
	}
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "relogin": body.NewPassword != ""})
}

// test 直接请求上游的 /System/Info/Public，检查连通性。
func (a *Admin) test(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Upstream    string `json:"upstream"`
		InsecureTLS bool   `json:"insecure_tls"`
		UserAgent   string `json:"user_agent"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	up := addScheme(body.Upstream)
	u, err := parseUpstream(up)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	client := &http.Client{
		Transport: a.m.transport(body.InsecureTLS),
		Timeout:   15 * time.Second,
		// 不跟随跳转：很多服务器把被拦截的请求 302 到错误页
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	base := u.Scheme + "://" + u.Host + u.EscapedPath()
	var lastErr string
	for _, p := range []string{"/emby/System/Info/Public", "/System/Info/Public"} {
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		req, _ := http.NewRequestWithContext(ctx, "GET", base+p, nil)
		req.Header.Set("Accept", "application/json")
		// 不少服务器会拦截浏览器 / curl / Go 默认 UA，测试时模拟播放器
		ua := body.UserAgent
		if ua == "" {
			ua = testUA
		}
		req.Header.Set("User-Agent", ua)
		start := time.Now()
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = simplifyNetErr(err)
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		cancel()
		ms := time.Since(start).Milliseconds()
		var info map[string]any
		if resp.StatusCode == 200 && json.Unmarshal(data, &info) == nil && info["Id"] != nil {
			writeJSON(w, 200, map[string]any{
				"ok": true, "ms": ms, "server_name": info["ServerName"], "version": info["Version"],
				"id": info["Id"], "final_url": resp.Request.URL.String(),
			})
			return
		}
		if loc := resp.Header.Get("Location"); resp.StatusCode >= 300 && resp.StatusCode < 400 && loc != "" {
			lastErr = "被重定向到 " + loc + "（服务器可能拦截了本机 IP 或 User-Agent，或地址填错）"
			continue
		}
		lastErr = "HTTP " + strconv.Itoa(resp.StatusCode) + "（不是 Emby 的响应？）"
	}
	writeJSON(w, 200, map[string]any{"ok": false, "error": lastErr})
}

func simplifyNetErr(err error) string {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return "连接超时"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "no such host"):
		return "域名解析失败"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝（端口没开？）"
	case strings.Contains(s, "certificate"):
		return "证书校验失败（可勾选「不校验证书」）: " + s
	}
	return s
}
