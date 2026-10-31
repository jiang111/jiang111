package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// epPrefix 是推流分离域名的代理路径：/__ep/{签名}/{scheme}/{host}/{port}/{path}
// 上游 302 跳转到别的推流域名时，Location 会被改写成这个格式，流量继续经过本机。
// 签名用集群共享密钥计算，多台服务器部署同一个域名时，任何一台都能验证并放行，
// 不依赖「是不是这台机器学到的推流域名」。旧的无签名格式 /__ep/{scheme}/... 仍按白名单放行。
const epPrefix = "/__ep/"

type SiteRT struct {
	m        *Manager
	cfg      Site
	upstream *url.URL // 已规范化：默认端口去掉，Path 为不带尾斜杠的 base path
	upHost   string
	upPort   string
	upBase   string // 上游 base path（已转义），如 /emby-base
	stats    *Stats
	epKey    []byte    // 推流地址签名密钥（集群共享）
	lines    []*upLine // 主线路 + 备用线路（见 lines.go），lines[0] 即 upstream

	hostsMu    sync.Mutex
	hosts      map[string]bool // 学习到的推流源（scheme://host:port），允许经 /__ep/ 代理
	routeHosts map[string]bool // 额外转发规则命中过的节点，同样允许经 /__ep/ 代理
}

func defaultPort(scheme string) string {
	if scheme == "https" {
		return "443"
	}
	return "80"
}

func splitHP(hostport, scheme string) (string, string) {
	if h, p, err := net.SplitHostPort(hostport); err == nil {
		return strings.ToLower(h), p
	}
	return strings.ToLower(strings.Trim(hostport, "[]")), defaultPort(scheme)
}

func joinHP(scheme, host, port string) string {
	if port == defaultPort(scheme) {
		if strings.Contains(host, ":") {
			return "[" + host + "]"
		}
		return host
	}
	return net.JoinHostPort(host, port)
}

func hostKey(scheme, host, port string) string {
	return scheme + "://" + host + ":" + port
}

func parseUpstream(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return nil, errors.New("上游地址格式不正确，应为 http(s)://域名或IP[:端口]")
	}
	u.Scheme = strings.ToLower(u.Scheme)
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errors.New("上游地址只支持 http / https")
	}
	h, p := splitHP(u.Host, u.Scheme)
	if h == "" {
		return nil, errors.New("上游地址缺少主机名")
	}
	if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
		return nil, errors.New("上游端口不正确")
	}
	u.Host = joinHP(u.Scheme, h, p)
	base := strings.TrimRight(u.EscapedPath(), "/")
	// 用户常把 /emby 或 /web/index.html 带进来，这些都不是 base path
	lb := strings.ToLower(base)
	if lb == "/emby" || lb == "/web" || strings.HasPrefix(lb, "/web/") || lb == "/emby/web" || strings.HasPrefix(lb, "/emby/web/") {
		base = ""
	}
	u.Path, u.RawPath = "", ""
	if base != "" {
		u.Path, _ = url.PathUnescape(base)
		u.RawPath = base
	}
	u.RawQuery, u.Fragment, u.User = "", "", nil
	return u, nil
}

func newSiteRT(m *Manager, s Site, st *Stats) (*SiteRT, error) {
	u, err := parseUpstream(s.Upstream)
	if err != nil {
		return nil, err
	}
	lines, err := siteLines(s)
	if err != nil {
		return nil, err
	}
	rt := &SiteRT{m: m, cfg: s, upstream: u, stats: st, lines: lines, hosts: map[string]bool{}, routeHosts: map[string]bool{}}
	rt.upHost, rt.upPort = splitHP(u.Host, u.Scheme)
	rt.upBase = u.EscapedPath()
	for _, h := range s.StreamHosts {
		rt.hosts[h] = true
	}
	return rt, nil
}

func (s *SiteRT) roundTripper(rc *reqCtx) http.RoundTripper {
	var base http.RoundTripper = &lineTransport{base: s.m.transport(s.cfg.InsecureTLS, s.cfg.IgnoreExpired), s: s, rc: rc}
	if s.cfg.StreamMode == "proxy" {
		return &followTransport{base: base, s: s, rc: rc}
	}
	return base
}

// isUpstream 判断地址是否指向上游自己（任意一条线路）。
func (s *SiteRT) isUpstream(scheme, host, port string) bool {
	for _, ln := range s.lines {
		if host != ln.host {
			continue
		}
		if scheme == ln.u.Scheme && port == ln.port {
			return true
		}
		// http://a.com 与 https://a.com（都是默认端口）视为同一台
		if port == defaultPort(scheme) && ln.port == defaultPort(ln.u.Scheme) {
			return true
		}
	}
	return false
}

// sameDomain 判断主机名是否和上游（任意一条线路）同属一个域名。
func (s *SiteRT) sameDomain(h string) bool {
	pd := parentDomain(h)
	for _, ln := range s.lines {
		if parentDomain(ln.host) == pd {
			return true
		}
	}
	return false
}

func (s *SiteRT) learnHost(key string) {
	s.hostsMu.Lock()
	known := s.hosts[key]
	if !known && len(s.hosts) < 100 {
		s.hosts[key] = true
	}
	s.hostsMu.Unlock()
	if !known {
		s.m.bg.Add(1)
		go func() {
			defer s.m.bg.Done()
			s.m.persistStreamHost(s.cfg.ID, key)
		}()
	}
}

// learnRouteHost 把路由目标加入内存白名单（不写入配置，避免节点名把配置刷满）。
func (s *SiteRT) learnRouteHost(u *url.URL) {
	h, p := splitHP(u.Host, u.Scheme)
	s.hostsMu.Lock()
	if len(s.routeHosts) < 1000 {
		s.routeHosts[hostKey(u.Scheme, h, p)] = true
	}
	s.hostsMu.Unlock()
}

// allowedEP 判断 /__ep/ 目标是否允许：只允许上游自己跳转/返回过的推流域名，或手动添加的域名，避免成为开放代理。
func (s *SiteRT) allowedEP(scheme, host, port string) bool {
	if s.isUpstream(scheme, host, port) {
		return true
	}
	key := hostKey(scheme, host, port)
	s.hostsMu.Lock()
	ok := s.hosts[key] || s.routeHosts[key]
	s.hostsMu.Unlock()
	if ok {
		return true
	}
	for _, e := range s.cfg.StreamHosts {
		if strings.Contains(e, "://") {
			u, err := url.Parse(e)
			if err != nil {
				continue
			}
			h, p := splitHP(u.Host, u.Scheme)
			if h == host && p == port && u.Scheme == scheme {
				return true
			}
			continue
		}
		// 不带协议：只比较主机名（可带端口）
		if h, p, err := net.SplitHostPort(e); err == nil {
			if h == host && p == port {
				return true
			}
		} else if e == host || (strings.HasPrefix(e, "*.") && strings.HasSuffix(host, e[1:])) {
			return true
		}
	}
	return false
}

type reqCtx struct {
	s          *SiteRT
	prefix     string // 路径模式的前缀，如 /hk
	publicBase string // 客户端看到的地址，如 https://proxy.com/hk
	target     *url.URL
	ep         bool
	line       *upLine // 这个请求用的上游线路（换线重试后更新）
	rest       string  // 去掉路径前缀后的请求路径（已转义），换线重试时拼到新线路的 base 后面
	matchPath  string  // 小写、已解码、去掉前缀和 base 的路径，用于判断接口
	acceptGzip bool
	cw         *countWriter
	ip         string // 客户端真实 IP（转来的请求为原始 IP）
	play       string // 视频响应的播放去重 key（见 playKey）
	report     string // 播放上报（/Sessions/Playing…）的「IP|片子」key，上游接受后记一次
	reportStop bool   // 是 /Sessions/Playing/Stopped
	imgKey     string // 图片缓存 key（不能缓存的请求为空）
	errMsg     string
}

// imageCacheKey 返回请求的图片缓存 key，不能走缓存时为空。
func (s *SiteRT) imageCacheKey(r *http.Request, prefix string) string {
	esc := r.URL.EscapedPath()
	if len(prefix) > 0 && len(esc) >= len(prefix) {
		esc = esc[len(prefix):]
	}
	p, err := url.PathUnescape(esc)
	if err != nil {
		return ""
	}
	return imageKey(s.cfg.ID, r, strings.ToLower(p))
}

func firstHeader(r *http.Request, k string) string {
	v := r.Header.Get(k)
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func (s *SiteRT) publicBase(r *http.Request, prefix string) string {
	if s.cfg.PublicURL != "" {
		return s.cfg.PublicURL
	}
	scheme := strings.ToLower(firstHeader(r, "X-Forwarded-Proto"))
	if scheme != "http" && scheme != "https" {
		scheme = "http"
		if r.TLS != nil {
			scheme = "https"
		}
	}
	host := firstHeader(r, "X-Forwarded-Host")
	if host == "" {
		host = r.Host
	}
	if p := firstHeader(r, "X-Forwarded-Port"); p != "" && p != defaultPort(scheme) {
		if _, _, err := net.SplitHostPort(host); err != nil {
			host = net.JoinHostPort(strings.Trim(host, "[]"), p)
		}
	}
	if h, p, err := net.SplitHostPort(host); err == nil && p == defaultPort(scheme) {
		host = joinHP(scheme, strings.ToLower(h), p)
	}
	return scheme + "://" + host + prefix
}

func clientIP(r *http.Request) string {
	h, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		h = r.RemoteAddr
	}
	ip := net.ParseIP(h)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if v := firstHeader(r, "X-Real-Ip"); v != "" {
			return v
		}
		if v := firstHeader(r, "X-Forwarded-For"); v != "" {
			return v
		}
	}
	return h
}

func (s *SiteRT) parseEP(rest string) (*url.URL, string, bool) {
	body := rest[len(epPrefix):]
	sig := ""
	if first, after, ok := strings.Cut(body, "/"); ok && first != "http" && first != "https" {
		sig, body = first, after
	}
	parts := strings.SplitN(body, "/", 4)
	if len(parts) < 3 {
		return nil, "", false
	}
	scheme, host, port := strings.ToLower(parts[0]), strings.ToLower(parts[1]), parts[2]
	if scheme != "http" && scheme != "https" {
		return nil, "", false
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, "", false
	}
	host = strings.Trim(host, "[]")
	if host == "" {
		return nil, "", false
	}
	if sig != "" {
		if !hmac.Equal([]byte(sig), []byte(s.epSig(scheme, host, port))) {
			return nil, "", false
		}
	} else if !s.allowedEP(scheme, host, port) {
		return nil, "", false
	}
	remainder := "/"
	if len(parts) == 4 {
		remainder += parts[3]
	}
	return &url.URL{Scheme: scheme, Host: joinHP(scheme, host, port)}, remainder, true
}

// epSig 是推流目标的签名（站点 ID + 目标），16 个字符。
func (s *SiteRT) epSig(scheme, host, port string) string {
	mac := hmac.New(sha256.New, s.epKey)
	mac.Write([]byte(s.cfg.ID + "|" + scheme + "|" + host + "|" + port))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:16]
}

// epPath 生成带签名的推流代理路径 /__ep/{签名}/{scheme}/{host}/{port}
func (s *SiteRT) epPath(scheme, host, port string) string {
	return epPrefix + s.epSig(scheme, host, port) + "/" + scheme + "/" + host + "/" + port
}

func (s *SiteRT) epSegment(u *url.URL) string {
	h, p := splitHP(u.Host, u.Scheme)
	return s.epPath(u.Scheme, h, p)
}

var stripReqHeaders = []string{
	"X-Real-Ip", "Forwarded", "Via", "Cf-Connecting-Ip", "True-Client-Ip", "X-Client-Ip",
	"Cdn-Loop", "Cf-Ipcountry", "Cf-Ray", "Cf-Visitor", "Cf-Worker", "X-Forwarded-Port", "X-Forwarded-Prefix", hopHeader,
}

func (s *SiteRT) serve(w http.ResponseWriter, r *http.Request, prefix string) {
	if r.URL.Path == prefix+peerHealthPath { // 其它节点的健康探测（经 nginx），不访问上游
		w.Write([]byte("ok"))
		return
	}
	defer clearWriteDeadline(w)
	start := time.Now()
	st := s.stats
	ip := clientIP(r)
	node := ""
	logReq := func(cw *countWriter, errMsg string) {
		status := cw.status
		if status == 0 && r.Header.Get("Upgrade") != "" {
			status = 101
		}
		s.m.logs.add(LogEntry{
			Time: start.Unix(), Site: s.cfg.ID, SiteName: s.cfg.Name, IP: ip, Method: r.Method,
			Path: maskURI(r.URL.RequestURI()), Status: status, Bytes: cw.n,
			Ms: time.Since(start).Milliseconds(), UA: shortUA(r.UserAgent()), Err: errMsg, Node: node,
		})
	}

	// 同一用户固定同一台：由其它节点转来的请求直接处理；否则不是自己负责的用户就转给负责节点。
	// 转发出去的请求只计入「转发」（RelayRequests/RelayBytes），不计入本站请求/连接/播放/流量，
	// 由负责节点计数，避免两台相加时重复。
	hopped := false
	if hip, from, ok := s.m.verifyHop(r.Header.Get(hopHeader), s.cfg.ID); ok {
		ip, node, hopped = hip, "← "+from, true
	}
	// 图片缓存命中：本机直接返回，不访问上游，也不用转给负责节点
	imgKey := s.imageCacheKey(r, prefix)
	if e := s.m.imgs.get(imgKey); e != nil {
		st.begin(ip)
		cw := &countWriter{ResponseWriter: w, st: st, tr: &s.m.traffic}
		e.serve(cw, r)
		st.end(ip, "")
		s.m.imgs.saved.Add(int64(len(e.body)))
		if node == "" {
			node = "缓存"
		} else {
			node += " · 缓存"
		}
		logReq(cw, "")
		return
	}
	if hopped {
	} else if owner, ok := s.m.owner(ip, s.cfg.ExcludeNodes); ok && owner.Name != s.m.selfName() && s.cfg.PublicURL != "" {
		if loc := s.directURL(r, prefix, owner); loc != "" {
			// 视频直连：推流请求让客户端直接连负责节点，本机不转发视频
			rw := &countWriter{ResponseWriter: w, st: st, relay: true, tr: &s.m.traffic}
			w.Header().Set("Location", loc)
			w.Header().Set("Cache-Control", "no-store")
			rw.WriteHeader(http.StatusFound)
			st.LastAccess.Store(st.clock().Unix())
			node = "⇢ " + owner.Name + " 直连"
			logReq(rw, "")
			return
		}
		rw := &countWriter{ResponseWriter: w, st: st, relay: true, tr: &s.m.traffic}
		st.RelayRequests.Add(1)
		if s.forward(w, r, owner, ip, rw) {
			st.LastAccess.Store(st.clock().Unix())
			node = "→ " + owner.Name
			logReq(rw, "")
			return
		}
		st.RelayRequests.Add(-1) // 负责节点连不上，下面按本机请求正常计数
		node = "→ " + owner.Name + " 失联，本机处理"
	}

	st.begin(ip)
	cw := &countWriter{ResponseWriter: w, st: st, tr: &s.m.traffic}
	rc := &reqCtx{s: s, prefix: prefix, cw: cw, ip: ip, imgKey: imgKey}
	defer func() {
		st.end(rc.ip, rc.play)
		if cw.status >= 500 {
			st.Errors.Add(1)
		}
		logReq(cw, rc.errMsg)
	}()

	rc.publicBase = s.publicBase(r, prefix)
	esc := r.URL.EscapedPath()
	rest := esc
	if len(prefix) > 0 && len(esc) >= len(prefix) {
		rest = esc[len(prefix):]
	}
	if rest == "" || rest[0] != '/' {
		rest = "/" + rest
	}

	ln := s.activeLine()
	rc.line, rc.rest = ln, rest
	target := ln.u
	outPath := ln.base + rest
	if strings.HasPrefix(rest, epPrefix) {
		t, remainder, ok := s.parseEP(rest)
		if !ok {
			cw.status = http.StatusForbidden
			http.Error(w, "stream host not allowed", http.StatusForbidden)
			return
		}
		target, outPath, rest, rc.ep = t, remainder, remainder, true
	} else if t, matched, err := s.matchRoute(rest, r); matched {
		if err != nil {
			cw.status = http.StatusBadRequest
			http.Error(w, "bad route", http.StatusBadRequest)
			return
		}
		// 路由目标当作推流域名处理：路径原样转发，响应里指向它的地址改写回 /__ep/
		s.learnRouteHost(t)
		target, outPath, rc.ep = t, rest, true
	}
	rc.target = target
	unescOut, err := url.PathUnescape(outPath)
	if err != nil {
		cw.status = http.StatusBadRequest
		http.Error(w, "bad path", http.StatusBadRequest)
		return
	}
	unescRest, _ := url.PathUnescape(rest)
	rc.matchPath = strings.ToLower(unescRest)
	if stop, ok := playReportPath(r.Method, rc.matchPath); ok {
		rc.report, rc.reportStop = ip+"|"+playReportItem(r), stop
	}
	rc.acceptGzip = strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
	targetOrigin := target.Scheme + "://" + target.Host

	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL.Scheme = target.Scheme
			out.URL.Host = target.Host
			out.URL.Path = unescOut
			out.URL.RawPath = outPath
			out.Host = target.Host
			h := out.Header
			for _, k := range stripReqHeaders {
				h.Del(k)
			}
			stripPanelCookie(h)
			if s.cfg.ForwardIP {
				h.Set("X-Forwarded-For", ip)
				h.Set("X-Real-Ip", ip)
			}
			if h.Get("Origin") != "" {
				h.Set("Origin", targetOrigin)
			}
			if h.Get("Referer") != "" {
				h.Set("Referer", targetOrigin+"/")
			}
			// 只允许 gzip（需要改写的响应要能解压），客户端不支持或 Range 请求时不压缩。
			// 显式设置后 Transport 不会自动解压，其它类型（如网页版 JS）保持压缩原样转给客户端。
			if rc.acceptGzip && h.Get("Range") == "" {
				h.Set("Accept-Encoding", "gzip")
			} else {
				h.Set("Accept-Encoding", "identity")
			}
			if s.cfg.UserAgent != "" {
				h.Set("User-Agent", s.cfg.UserAgent)
			}
			for k, v := range s.cfg.Headers {
				if v == "" {
					h.Del(k)
				} else {
					h.Set(k, rc.expandHeader(v, ip))
				}
			}
		},
		Transport:      s.roundTripper(rc),
		FlushInterval:  -1,
		ModifyResponse: rc.modifyResponse,
		ErrorHandler:   rc.errorHandler,
	}
	rp.ServeHTTP(cw, r)
}

// stripPanelCookie 去掉面板的登录 Cookie：浏览器会把同一主机任意端口的 Cookie 都带上，不能转发给上游。
func stripPanelCookie(h http.Header) {
	cs := h.Values("Cookie")
	if len(cs) == 0 {
		return
	}
	h.Del("Cookie")
	for _, c := range cs {
		var keep []string
		for _, part := range strings.Split(c, ";") {
			name, _, _ := strings.Cut(strings.TrimSpace(part), "=")
			if name != cookieName && strings.TrimSpace(part) != "" {
				keep = append(keep, strings.TrimSpace(part))
			}
		}
		if len(keep) > 0 {
			h.Add("Cookie", strings.Join(keep, "; "))
		}
	}
}

// clearWriteDeadline 清掉 countWriter 设置的写超时，保持连接（keep-alive）的下一个请求不受影响。
func clearWriteDeadline(w http.ResponseWriter) {
	http.NewResponseController(w).SetWriteDeadline(time.Time{})
}

func (rc *reqCtx) errorHandler(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, context.Canceled) || r.Context().Err() != nil {
		rc.cw.status = 499 // 客户端主动断开（拖动进度条、切换视频时很常见）
		return
	}
	rc.errMsg = err.Error()
	w.WriteHeader(http.StatusBadGateway)
	fmt.Fprintf(w, "emby-proxy: 上游连接失败: %v\n", err)
}

func (rc *reqCtx) modifyResponse(resp *http.Response) error {
	h := resp.Header
	h.Del("Server")
	h.Del("X-Powered-By")
	h.Del("Alt-Svc")
	h.Del("Strict-Transport-Security") // HTTPS 策略由你自己的前置反代决定，不继承上游的
	for _, k := range []string{"Location", "Content-Location"} {
		if v := h.Get(k); v != "" {
			h.Set(k, rc.rewriteLocation(v))
		}
	}
	if rc.report != "" && resp.StatusCode < 300 {
		rc.s.stats.reportPlay(rc.report, rc.reportStop)
	}
	if rc.imgKey != "" && cacheableImage(resp) {
		rc.s.m.imgs.tee(rc.imgKey, resp)
	}
	ct := strings.ToLower(h.Get("Content-Type"))
	if (strings.HasPrefix(ct, "video/") || strings.HasPrefix(ct, "audio/")) && !strings.Contains(ct, "mpegurl") {
		if rc.play == "" {
			rc.play = playKey(rc.ip, rc.matchPath)
			rc.s.stats.startPlay(rc.play)
		}
	}
	return rc.rewriteBody(resp, ct)
}

func (rc *reqCtx) rewriteLocation(v string) string {
	lv := strings.ToLower(v)
	switch {
	case strings.HasPrefix(lv, "http://"), strings.HasPrefix(lv, "https://"):
		return rc.rewriteAbs(v, rc.externalRedirectOK(v))
	case strings.HasPrefix(v, "//"):
		v = rc.target.Scheme + ":" + v
		return rc.rewriteAbs(v, rc.externalRedirectOK(v))
	case strings.HasPrefix(v, "/"):
		if rc.ep {
			return rc.prefix + rc.s.epSegment(rc.target) + v
		}
		return rc.prefix + rc.s.stripBase(v)
	}
	return v
}

// externalRedirectOK 判断跳到外部域名的 Location 是否当作推流分离处理：
// 只有播放请求的跳转，或跳到与上游同一主域名的地址才算；
// 否则（如被拦截时跳到错误页）保持原样，不记为推流域名。
func (rc *reqCtx) externalRedirectOK(loc string) bool {
	if rc.ep || isMediaPath(rc.matchPath) {
		return true
	}
	u, err := url.Parse(loc)
	if err != nil {
		return false
	}
	h, _ := splitHP(u.Host, u.Scheme)
	return rc.s.sameDomain(h)
}

func (s *SiteRT) stripBase(tail string) string {
	for _, ln := range s.lines {
		if r, ok := stripOne(ln.base, tail); ok {
			return r
		}
	}
	return tail
}

func stripOne(b, tail string) (string, bool) {
	if b == "" || !strings.HasPrefix(strings.ToLower(tail), strings.ToLower(b)) {
		return tail, false
	}
	if len(tail) == len(b) {
		return "/", true
	}
	switch tail[len(b)] {
	case '/', '?', '#':
		r := tail[len(b):]
		if r[0] != '/' {
			r = "/" + r
		}
		return r, true
	}
	return tail, false
}

// rewriteAbs 改写一个绝对 URL：
//   - 指向上游自身 → 改成客户端访问的地址
//   - 指向其它域名（推流分离）且 allowExternal 且推流模式为 proxy → 改成 /__ep/ 代理地址并记住该域名
//   - 其它保持不变
func (rc *reqCtx) rewriteAbs(raw string, allowExternal bool) string {
	i := strings.Index(raw, "://")
	if i < 0 {
		return raw
	}
	scheme := strings.ToLower(raw[:i])
	rest := raw[i+3:]
	end := strings.IndexAny(rest, "/?#")
	if end < 0 {
		end = len(rest)
	}
	hostport, tail := rest[:end], rest[end:]
	if hostport == "" || strings.Contains(hostport, "@") {
		return raw
	}
	host, port := splitHP(hostport, scheme)
	s := rc.s
	if rc.isSelf(scheme, host, port) {
		return raw // 已经是反代自己的地址（如上游按 MagpieCDN / X-Forwarded-Host 生成的）
	}
	if s.isUpstream(scheme, host, port) {
		return rc.publicBase + s.stripBase(tail)
	}
	if rc.ep {
		th, tp := splitHP(rc.target.Host, rc.target.Scheme)
		if host == th && port == tp && scheme == rc.target.Scheme {
			return rc.publicBase + s.epSegment(rc.target) + tail
		}
	}
	if !allowExternal || s.cfg.StreamMode != "proxy" {
		return raw
	}
	// 上游返回的本机回环地址不能代理（否则会访问到反代服务器自己），除非上游本身就在本机
	if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified()) {
		if up := net.ParseIP(s.upHost); up == nil || !up.IsLoopback() {
			return raw
		}
	}
	if host == "localhost" {
		return raw
	}
	s.learnHost(hostKey(scheme, host, port))
	return rc.publicBase + s.epPath(scheme, host, port) + tail
}

func (rc *reqCtx) isSelf(scheme, host, port string) bool {
	pb := rc.publicBase
	i := strings.Index(pb, "://")
	if i < 0 {
		return false
	}
	ps := pb[:i]
	hp := pb[i+3:]
	if j := strings.IndexByte(hp, '/'); j >= 0 {
		hp = hp[:j]
	}
	h, p := splitHP(hp, ps)
	return h == host && p == port && ps == scheme
}

// resolveSelf 把指向本站自己对外地址的跳转（如上游按 MagpieCDN 头生成的 {public_base}/magpiecdn?load=节点）
// 解析成本机收到这个请求时实际要访问的地址：额外转发规则、/__ep/ 推流地址或上游。
// 服务端跟随跳转时用它在本机直接解析，不经公网绕回自己（DNS 可能落到另一台，视频多走一圈，访问者里还会多出服务器自己的 IP）。
func (rc *reqCtx) resolveSelf(loc *url.URL) (*url.URL, bool) {
	h, p := splitHP(loc.Host, loc.Scheme)
	pb, err := url.Parse(rc.publicBase)
	if err != nil || !rc.isSelf(loc.Scheme, h, p) {
		return nil, false
	}
	base := strings.TrimRight(pb.EscapedPath(), "/")
	esc := loc.EscapedPath()
	if !strings.HasPrefix(esc, base) {
		return nil, false
	}
	rest := esc[len(base):]
	if rest == "" {
		rest = "/"
	}
	if rest[0] != '/' {
		return nil, false
	}
	s := rc.s
	ln := s.activeLine()
	target, out := ln.u, ln.base+rest
	if strings.HasPrefix(rest, epPrefix) {
		t, remainder, ok := s.parseEP(rest)
		if !ok {
			return nil, false
		}
		target, out = t, remainder
	} else if t, matched, err := s.matchRoute(rest, &http.Request{URL: loc}); matched {
		if err != nil {
			return nil, false
		}
		s.learnRouteHost(t)
		target, out = t, rest
	}
	unesc, err := url.PathUnescape(out)
	if err != nil {
		return nil, false
	}
	return &url.URL{Scheme: target.Scheme, Host: target.Host, Path: unesc, RawPath: out, RawQuery: loc.RawQuery}, true
}

// countWriter 统计下行流量；实现 Unwrap 以便 ReverseProxy 能 Flush / Hijack（WebSocket）。
// relay 为 true 时（转发给其它节点的请求）只计入转发流量。
type countWriter struct {
	http.ResponseWriter
	st     *Stats
	tr     *monthTraffic // 本机本月流量（本机处理和转发都算）
	relay  bool
	n      int64
	status int
	rc     *http.ResponseController
}

func (c *countWriter) WriteHeader(code int) {
	if c.status == 0 || c.status < 200 {
		c.status = code
	}
	c.ResponseWriter.WriteHeader(code)
}

// writeStall：客户端多久读不走数据就断开（播放器卡死、网络半断时连接和上游拉流会一直挂着）。
// 每次写之前重新设置写超时，请求结束时清掉（见 serve），所以正常的暂停、慢速播放不受影响。
// 前面有 nginx 时 nginx 自己的 send_timeout 会先断开，这里主要保护直接对外的独立端口。
var writeStall = 3 * time.Minute

func (c *countWriter) Write(b []byte) (int, error) {
	if c.status == 0 {
		c.status = http.StatusOK
	}
	if c.rc == nil {
		c.rc = http.NewResponseController(c.ResponseWriter)
	}
	c.rc.SetWriteDeadline(time.Now().Add(writeStall))
	n, err := c.ResponseWriter.Write(b)
	c.n += int64(n)
	c.tr.add(int64(n))
	if c.relay {
		c.st.RelayBytes.Add(int64(n))
	} else {
		c.st.addBytes(int64(n))
	}
	return n, err
}

func (c *countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }

var sensitiveKeys = []string{"api_key", "x-emby-token", "x-emby-authorization", "token", "pw", "password", "x-mediabrowser-token"}

func maskURI(uri string) string {
	path, q, ok := strings.Cut(uri, "?")
	if ok {
		parts := strings.Split(q, "&")
		for i, kv := range parts {
			k, _, _ := strings.Cut(kv, "=")
			if uk, err := url.QueryUnescape(k); err == nil {
				k = uk
			}
			for _, s := range sensitiveKeys {
				if strings.EqualFold(k, s) {
					parts[i] = k + "=***"
				}
			}
		}
		path += "?" + strings.Join(parts, "&")
	}
	if len(path) > 300 {
		path = path[:300] + "…"
	}
	return path
}

func shortUA(ua string) string {
	if len(ua) > 80 {
		return ua[:80]
	}
	return ua
}
