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
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const cookieName = "emby_proxy_session"

const testUA = "SenPlayer/6.2.1 (iPhone; iOS 18.0; Scale/3.00)"

type Admin struct {
	m        *Manager
	cluster  *Cluster
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
	mux.HandleFunc("GET /export", a.serveExportPage)  // 一键导出账号页（导出密码在链接里，不用登录）
	mux.HandleFunc("POST /export/data", a.exportData) // 导出页脚本取数据（校验导出密码）
	mux.HandleFunc("POST /api/export-key", a.auth(a.nodeReadOnly(a.resetExportKey)))
	mux.HandleFunc("DELETE /api/export-key", a.auth(a.nodeReadOnly(a.resetExportKey)))
	mux.HandleFunc("POST /api/logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1})
		writeJSON(w, 200, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /api/state", a.authView(a.state))
	mux.HandleFunc("GET /api/logs", a.auth(a.logsAPI))
	mux.HandleFunc("PUT /api/viewers", a.auth(a.saveViewers))
	mux.HandleFunc("POST /api/sites", a.auth(a.nodeReadOnly(a.saveSite)))
	mux.HandleFunc("PUT /api/sites/{id}", a.auth(a.nodeReadOnly(a.saveSite)))
	mux.HandleFunc("DELETE /api/sites/{id}", a.auth(a.nodeReadOnly(a.deleteSite)))
	mux.HandleFunc("PUT /api/sites/{id}/account", a.auth(a.nodeReadOnly(a.saveAccount)))
	mux.HandleFunc("DELETE /api/sites/{id}/account", a.auth(a.nodeReadOnly(a.deleteAccount)))
	mux.HandleFunc("POST /api/sites/{id}/clear-stats", a.auth(a.clearStats))
	mux.HandleFunc("POST /api/test", a.auth(a.test))
	mux.HandleFunc("GET /icons.json", serveIcons)
	mux.HandleFunc("GET /api/update", a.auth(a.updateInfo))
	mux.HandleFunc("POST /api/update", a.auth(a.startUpdate))
	mux.HandleFunc("GET /api/stats", a.authView(a.statsAPI))
	mux.HandleFunc("PUT /api/sites/order", a.auth(a.nodeReadOnly(a.reorderSites)))
	mux.HandleFunc("POST /api/lines/probe", a.auth(a.probeAll))
	mux.HandleFunc("PUT /api/ip-pref", a.auth(a.nodeReadOnly(a.saveIPPref)))
	mux.HandleFunc("GET /api/access", a.auth(a.accessState))
	mux.HandleFunc("PUT /api/access", a.auth(a.nodeReadOnly(a.saveAccess)))
	mux.HandleFunc("POST /api/access/unban", a.auth(a.unban))
	mux.HandleFunc("DELETE /api/access/catches", a.auth(a.clearCatches))
	mux.HandleFunc("POST /api/image-cache/clear", a.auth(func(w http.ResponseWriter, r *http.Request) {
		a.m.imgs.clear()
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("PUT /api/settings", a.auth(a.settings))
	mux.HandleFunc("POST /api/cert", a.auth(a.nodeReadOnly(a.uploadCert)))
	mux.HandleFunc("GET /api/export", a.auth(a.exportConfig))
	mux.HandleFunc("POST /api/import", a.auth(a.nodeReadOnly(a.importConfig)))
	mux.HandleFunc("GET /api/cluster/sync", a.cluster.serveSync)      // 令牌鉴权，供从节点拉取
	mux.HandleFunc("POST /api/cluster/report", a.cluster.serveReport) // 令牌鉴权，从节点上报统计
	mux.HandleFunc("PUT /api/keepalive", a.auth(a.nodeReadOnly(a.saveKeepalive)))
	mux.HandleFunc("POST /api/keepalive/run", a.auth(a.nodeReadOnly(a.runKeepalive)))
	mux.HandleFunc("PUT /api/sites/{id}/keepalive", a.auth(a.nodeReadOnly(a.setSiteKeepalive)))
	mux.HandleFunc("PUT /api/cluster", a.auth(a.clusterSettings))
	mux.HandleFunc("POST /api/cluster/rebalance", a.auth(a.rebalanceNow))
	mux.HandleFunc("POST /api/cluster/sync-now", a.auth(func(w http.ResponseWriter, r *http.Request) {
		a.cluster.syncNow()
		writeJSON(w, 200, map[string]any{"ok": true})
	}))
	mux.HandleFunc("GET /api/nginx", a.auth(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write([]byte(a.m.nginxConf()))
	}))
	return securityHeaders(mux)
}

// csrfHeader 是面板前端在所有写请求里带的自定义头。
// 浏览器的 SameSite 不区分端口：反代出来的上游页面（同一主机的其它端口）也算同站，会带上面板 Cookie。
// 自定义头会触发 CORS 预检，而面板不响应 CORS，因此跨源页面无法伪造写请求。
const csrfHeader = "X-Emby-Proxy"

func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get(csrfHeader) != "1" || !sameOrigin(r) {
				writeErr(w, http.StatusForbidden, "请求来源不正确")
				return
			}
		}
		h.ServeHTTP(w, r)
	})
}

// sameOrigin：带了 Origin 头时，必须与面板自己的地址一致（主机名和端口，默认端口按 Origin 的协议补全）。
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, err := url.Parse(o)
	if err != nil || u.Host == "" {
		return false
	}
	host := firstHeader(r, "X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	oh, op := splitHP(u.Host, u.Scheme)
	hh, hp := splitHP(host, u.Scheme)
	return oh == hh && op == hp
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
			if err == nil && a.role(c.Value) == "viewer" {
				writeErr(w, http.StatusForbidden, errViewer.Error())
				return
			}
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		h(w, r.WithContext(context.WithValue(r.Context(), roleKey, "admin")))
	}
}

const (
	loginWindow    = 10 * time.Minute
	loginPerIP     = 5  // 单个 IP 10 分钟内最多失败次数
	loginGlobalMax = 30 // 所有 IP 合计（防止伪造来源 IP 绕过）
)

// reserveLogin 在校验密码前先登记一次尝试；返回 false 表示已超限。
func (a *Admin) reserveLogin(ip string) bool {
	a.failMu.Lock()
	defer a.failMu.Unlock()
	now := time.Now()
	total := 0
	for k, ts := range a.failures {
		recent := ts[:0]
		for _, t := range ts {
			if now.Sub(t) < loginWindow {
				recent = append(recent, t)
			}
		}
		if len(recent) == 0 {
			delete(a.failures, k)
			continue
		}
		a.failures[k] = recent
		total += len(recent)
	}
	if len(a.failures[ip]) >= loginPerIP || total >= loginGlobalMax {
		return false
	}
	a.failures[ip] = append(a.failures[ip], now)
	return true
}

// releaseLogin 登录成功后撤销本次登记。
func (a *Admin) releaseLogin(ip string) {
	a.failMu.Lock()
	if ts := a.failures[ip]; len(ts) > 0 {
		a.failures[ip] = ts[:len(ts)-1]
	}
	a.failMu.Unlock()
}

func (a *Admin) login(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if !a.reserveLogin(ip) {
		writeErr(w, http.StatusTooManyRequests, "失败次数过多，请 10 分钟后再试")
		return
	}
	a.m.mu.RLock()
	pw := a.m.cfg.AdminPassword
	a.m.mu.RUnlock()
	exp := time.Now().Add(7 * 24 * time.Hour).Unix()
	tok, role := "", "admin"
	switch {
	case subtle.ConstantTimeCompare([]byte(body.Password), []byte(pw)) == 1:
		tok = a.sign(exp)
	case a.matchViewer(body.Password):
		tok, role = a.signViewer(exp), "viewer"
	default:
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "密码错误")
		return
	}
	a.releaseLogin(ip)
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: tok, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 7 * 24 * 3600,
		Secure: r.TLS != nil || strings.EqualFold(firstHeader(r, "X-Forwarded-Proto"), "https"),
	})
	writeJSON(w, 200, map[string]any{"ok": true, "role": role})
}

type siteView struct {
	*Site
	Stats  StatsView `json:"stats"` // 本机
	Status string    `json:"status"`
	Error  string    `json:"error,omitempty"`
	// 仅主节点：本机 + 各在线节点之和，及各节点拆分
	ClusterStats *StatsView   `json:"cluster_stats,omitempty"`
	NodeStats    []nodeStats  `json:"node_stats,omitempty"`
	Account      *accountView `json:"account,omitempty"` // 站点账号（登录后才能看到）
	Lines        []lineView   `json:"lines,omitempty"`   // 各线路的延迟和状态（本机探测）
	LineEvents   []lineEvent  `json:"line_events,omitempty"`
}

func (a *Admin) state(w http.ResponseWriter, r *http.Request) {
	if roleOf(r) == "viewer" {
		a.viewerState(w)
		return
	}
	m := a.m
	// 这两项内部会自己加锁，必须在持有读锁之前算好（读锁嵌套在有写者等待时会死锁）
	cluster := a.cluster.view()
	ngx := m.nginxStatus()
	// 各节点上报的统计同样先取快照（c.mu），持有 m.mu 时不能再拿 c.mu
	var snap *statsSnapshot
	if cluster.Role == "master" {
		snap = a.cluster.statsSnapshot()
	}
	var ka *kaView
	exportKey := ""
	if cluster.Role != "node" {
		ka = m.ka.view(time.Now())
		m.mu.RLock()
		exportKey = m.cfg.ExportKey
		m.mu.RUnlock()
	}
	var key []byte
	var keyErr error
	m.mu.RLock()
	hasAccounts := len(m.cfg.Accounts) > 0
	m.mu.RUnlock()
	if hasAccounts {
		key, keyErr = m.accountKey(false)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := []siteView{}
	for _, s := range m.cfg.Sites {
		v := siteView{Site: s, Status: "disabled", Account: m.accountViewOf(s.ID, key, keyErr)}
		if st := m.stats[s.ID]; st != nil {
			v.Stats = st.view()
		}
		if snap != nil {
			// 只做控制的主节点（没填本机公网地址）不在各节点拆分里列出本机
			cs, ns := snap.site(s.ID, cluster.SelfName, v.Stats, cluster.AdvertiseAddr != "")
			v.ClusterStats, v.NodeStats = &cs, ns
		}
		v.Lines, v.LineEvents = m.lineViews(s.ID)
		if e := m.siteErr[s.ID]; e != "" {
			v.Status, v.Error = "error", e
		} else if m.sites[s.ID] != nil {
			v.Status = "running"
		}
		views = append(views, v)
	}
	out := map[string]any{
		"settings": map[string]any{
			"listen": m.cfg.Listen, "admin_listen": m.cfg.AdminListen,
			"entry_url": m.cfg.EntryURL, "public_host": m.cfg.PublicHost,
			"port_bind": m.cfg.PortBind, "nginx_cert": m.cfg.NginxCert, "nginx_key": m.cfg.NginxKey, "nginx_ipv6": m.cfg.NginxIPv6,
			"cert": m.certInfo(),
		},
		"sites":       views,
		"nginx":       ngx,
		"cluster":     cluster,
		"keepalive":   ka,        // 自动保号（从节点为 null）
		"export_key":  exportKey, // 一键导出的导出密码（从节点为空）
		"image_cache": m.imgs.view(),
		"role":        "admin",
		"viewers":     append([]string{}, m.cfg.ViewerPasswords...),
		"ip_pref":     m.cfg.IPPref,
		"uptime":      int64(time.Since(m.started).Seconds()),
		"version":     appVersion(),
	}
	if snap != nil {
		out["cluster_stats_nodes"] = snap.nodes // 上报过统计的从节点及最后上报时间
	}
	writeJSON(w, 200, out)
}

// logsAPI：GET /api/logs?site=&server=&limit=。主节点合并本机和各从节点上报的日志（按时间，最新在前）。
func (a *Admin) logsAPI(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	site, server := q.Get("site"), q.Get("server")
	if !a.m.isMaster() {
		writeJSON(w, 200, a.m.logs.list(func(e *LogEntry) bool { return site == "" || e.Site == site }, limit))
		return
	}
	self := a.m.selfName()
	// 「同一用户固定同一台」时，收到请求的服务器转给负责节点（记「→ 负责节点」），负责节点处理（记「← 来源」），
	// 同一个请求两台各有一条。看全部服务器时只留负责节点那条；选了具体服务器照常全部显示。
	// 负责节点的转发失联、本机兜底处理（「失联」）和视频直连跳转（「⇢」）只有一条，不隐藏。
	relayDup := func(e *LogEntry) bool {
		return server == "" && strings.HasPrefix(e.Node, "→ ") && !strings.Contains(e.Node, "失联")
	}
	match := func(e *LogEntry) bool {
		return (site == "" || e.Site == site) && (server == "" || e.Server == server) && !relayDup(e)
	}
	var local []LogEntry
	if server == "" || server == self {
		local = a.m.logs.list(func(e *LogEntry) bool { return (site == "" || e.Site == site) && !relayDup(e) }, limit)
	}
	for i := range local {
		local[i].Server = self
	}
	remote := a.cluster.remoteLogs.list(match, limit)
	out := make([]LogEntry, 0, min(limit, len(local)+len(remote)))
	for i, j := 0, 0; len(out) < limit && (i < len(local) || j < len(remote)); {
		if j >= len(remote) || (i < len(local) && local[i].Time >= remote[j].Time) {
			out, i = append(out, local[i]), i+1
		} else {
			out, j = append(out, remote[j]), j+1
		}
	}
	writeJSON(w, 200, out)
}

func (a *Admin) saveSite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Site
		// 面板打开表单时看到的推流域名。之后自动学习到的域名不在其中，保存时保留，
		// 避免用旧快照覆盖掉新学到的域名；用户在表单里删掉的域名（在 base 里但不在提交里）照常删除。
		StreamHostsBase []string `json:"stream_hosts_base"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	s := req.Site
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
		s.StreamHosts = mergeHosts(s.StreamHosts, req.StreamHostsBase, m.cfg.Sites[idx].StreamHosts)
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
	m.pruneSiteData()
	delete(m.stats, id)
	m.daily.dropSite(id)
	m.reload()
	m.mu.Unlock()
	a.cluster.dropSite(id)
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) clearStats(w http.ResponseWriter, r *http.Request) {
	m := a.m
	id := r.PathValue("id")
	m.mu.RLock()
	st := m.stats[id]
	m.mu.RUnlock()
	if st != nil {
		st.clear()
	}
	a.cluster.markCleared(id) // 主节点：各节点的累计值从现在算起（m.mu 已释放）
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) settings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EntryURL    *string `json:"entry_url"`
		PublicHost  *string `json:"public_host"`
		PortBind    *string `json:"port_bind"`
		NginxCert   *string `json:"nginx_cert"`
		NginxKey    *string `json:"nginx_key"`
		NginxIPv6   *bool   `json:"nginx_ipv6"`
		OldPassword string  `json:"old_password"`
		NewPassword string  `json:"new_password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	var bind string
	if body.PortBind != nil {
		bind = strings.Trim(strings.TrimSpace(*body.PortBind), "[]")
		if !validBind(bind) {
			writeErr(w, 400, "监听地址必须是 IP，例如 127.0.0.1，留空表示所有网卡")
			return
		}
	}
	if a.m.isNode() && (body.EntryURL != nil || body.PublicHost != nil || body.PortBind != nil || body.NginxCert != nil || body.NginxKey != nil || body.NginxIPv6 != nil) {
		writeErr(w, http.StatusConflict, "这台服务器是从节点，这些设置由主节点同步，请到主节点上修改")
		return
	}
	if body.NewPassword != "" && len(body.NewPassword) < 6 {
		writeErr(w, 400, "新密码至少 6 位")
		return
	}
	m := a.m
	m.mu.Lock()
	if body.NewPassword != "" && subtle.ConstantTimeCompare([]byte(body.OldPassword), []byte(m.cfg.AdminPassword)) != 1 {
		m.mu.Unlock()
		writeErr(w, 400, "原密码错误")
		return
	}
	// 以下不会再失败，统一应用
	if body.NewPassword != "" {
		m.cfg.AdminPassword = body.NewPassword
	}
	if body.EntryURL != nil {
		m.cfg.EntryURL = strings.TrimRight(strings.TrimSpace(*body.EntryURL), "/")
	}
	if body.PublicHost != nil {
		m.cfg.PublicHost = strings.TrimSpace(*body.PublicHost)
	}
	if body.NginxCert != nil {
		m.cfg.NginxCert = strings.TrimSpace(*body.NginxCert)
	}
	if body.NginxKey != nil {
		m.cfg.NginxKey = strings.TrimSpace(*body.NginxKey)
	}
	if body.NginxIPv6 != nil {
		m.cfg.NginxIPv6 = *body.NginxIPv6
	}
	if body.PortBind != nil && bind != m.cfg.PortBind {
		m.cfg.PortBind = bind
		m.rebindAll()
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
		Upstream      string `json:"upstream"`
		InsecureTLS   bool   `json:"insecure_tls"`
		IgnoreExpired bool   `json:"ignore_expired"`
		UserAgent     string `json:"user_agent"`
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
		Transport: a.m.transport(body.InsecureTLS, body.IgnoreExpired),
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
	case strings.Contains(s, "expired") || strings.Contains(s, "not yet valid"):
		return "上游证书已过期（可勾选「上游证书过期仍连接」）: " + s
	case strings.Contains(s, "certificate"):
		return "证书校验失败（可勾选「不校验证书」）: " + s
	}
	return s
}

// mergeHosts = submitted ∪ (current − base)
func mergeHosts(submitted, base, current []string) []string {
	inBase := map[string]bool{}
	for _, h := range base {
		inBase[strings.ToLower(strings.TrimSpace(h))] = true
	}
	out := append([]string{}, submitted...)
	seen := map[string]bool{}
	for _, h := range out {
		seen[h] = true
	}
	for _, h := range current {
		if !inBase[h] && !seen[h] {
			out = append(out, h)
			seen[h] = true
		}
	}
	return out
}
