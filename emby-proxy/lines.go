package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// 多线路智能选路：一个站点除了主线路（Upstream）外可以填几条备用线路（Backups），
// 都是同一台 Emby 服务器的不同入口（如 CDN 线路、直连 IP、另一个域名）。
//
//   - 主动探测：每台服务器各自定时请求每条线路的 /System/Info/Public，记录延迟（指数平滑）和连续失败次数；
//     连续 2 次失败判为不可用，连续 2 次成功恢复。
//   - 选路：正在用的线路不可用时立即换到可用线路里最快的一条；都可用时，别的线路要明显更快
//     （比当前快 30% 且至少 30ms）才换，避免来回切换。全部不可用时仍用当前线路（总比直接报错好）。
//   - 请求失败当场换线：连不上上游，或上游返回 502 / 504 时，没有请求体的请求（GET / HEAD，绝大多数）
//     立即用下一条可用线路重试，客户端无感；连续失败 2 次的线路同样判为不可用，不用等下一轮探测。
//   - 切换、全部不可用、恢复会记在面板上，配置了保号的 Telegram 时同时推送（只写线路序号，不写地址）。
//
// 多台服务器时每台各自探测、各自选路（不同机房到各线路的快慢不同），线路状态不同步。

const (
	lineFailDown  = 2   // 连续失败几次判为不可用
	lineOKUp      = 2   // 不可用的线路连续成功几次恢复
	lineSwitchPct = 1.3 // 别的线路要比当前快这么多倍……
	lineSwitchMs  = 30  // ……且至少快这么多毫秒才切换
	lineProbeMany = 30 * time.Second
	lineProbeOne  = 2 * time.Minute // 只有一条线路时只为面板显示延迟和推送故障，探测得慢一些
	lineTimeout   = 8 * time.Second
)

type lineState struct {
	ms      float64 // 延迟（指数平滑），0 = 还没测过
	okN     int     // 连续成功次数
	failN   int     // 连续失败次数
	down    bool
	checked time.Time
	lastErr string
}

type upLine struct {
	key  string   // 规范化后的地址，状态表的 key
	u    *url.URL // 只含 scheme 和 host
	host string
	port string
	base string // 已转义的 base path
}

type lineEvent struct {
	Time int64  `json:"time"`
	Text string `json:"text"`
}

type lineHub struct {
	mu       sync.Mutex
	states   map[string]*lineState // 站点 ID + "|" + 线路 key
	active   map[string]string     // 站点 ID → 正在用的线路 key
	allDown  map[string]bool       // 站点 ID → 已推送过「全部不可用」
	events   map[string][]lineEvent
	lastSent map[string]time.Time // 站点 ID → 上次推送切换通知
	next     map[string]time.Time // 站点 ID → 下次探测时间
	inflight map[string]bool

	probe  func(s Site, ln *upLine) (time.Duration, error) // 测试可替换
	notify func(text string)                               // 测试可替换，nil 用 Telegram
}

func (h *lineHub) init() {
	if h.states == nil {
		h.states = map[string]*lineState{}
		h.active = map[string]string{}
		h.allDown = map[string]bool{}
		h.events = map[string][]lineEvent{}
		h.lastSent = map[string]time.Time{}
		h.next = map[string]time.Time{}
		h.inflight = map[string]bool{}
	}
}

// state 调用方需持有 h.mu。
func (h *lineHub) state(site, key string) *lineState {
	h.init()
	k := site + "|" + key
	st := h.states[k]
	if st == nil {
		st = &lineState{}
		h.states[k] = st
	}
	return st
}

func (h *lineHub) event(site, text string) {
	h.init()
	ev := append(h.events[site], lineEvent{time.Now().Unix(), text})
	if len(ev) > 10 {
		ev = ev[len(ev)-10:]
	}
	h.events[site] = ev
}

// siteLines 把主线路和备用线路解析成线路列表（去重，解析失败的备用线路跳过）。
func siteLines(s Site) ([]*upLine, error) {
	var out []*upLine
	seen := map[string]bool{}
	for i, raw := range append([]string{s.Upstream}, s.Backups...) {
		u, err := parseUpstream(raw)
		if err != nil {
			if i == 0 {
				return nil, err
			}
			continue
		}
		base := u.EscapedPath()
		key := u.Scheme + "://" + u.Host + base
		if seen[key] {
			continue
		}
		seen[key] = true
		h, p := splitHP(u.Host, u.Scheme)
		out = append(out, &upLine{key: key, u: &url.URL{Scheme: u.Scheme, Host: u.Host}, host: h, port: p, base: base})
	}
	return out, nil
}

func lineName(i int) string {
	if i == 0 {
		return "主线路"
	}
	return fmt.Sprintf("备用线路 %d", i)
}

// activeLine 返回这个请求要用的线路。正在用的线路被请求失败判为不可用时，当场换到最快的可用线路。
func (s *SiteRT) activeLine() *upLine {
	if len(s.lines) == 1 {
		return s.lines[0]
	}
	h := &s.m.lines
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	cur := 0
	if k := h.active[s.cfg.ID]; k != "" {
		for i, ln := range s.lines {
			if ln.key == k {
				cur = i
			}
		}
	}
	if !h.state(s.cfg.ID, s.lines[cur].key).down {
		return s.lines[cur]
	}
	if best := h.best(s.cfg.ID, s.lines, nil); best >= 0 && best != cur {
		h.switchLocked(s.cfg.ID, s.cfg.Name, s.lines, cur, best, lineName(cur)+"请求连续失败")
		return s.lines[best]
	}
	return s.lines[cur]
}

// best 返回可用线路里延迟最低的（没测过延迟的排在测过的后面，同样快取靠前的），没有可用线路返回 -1。调用方需持有 h.mu。
func (h *lineHub) best(site string, lines []*upLine, skip map[*upLine]bool) int {
	best, bestMs := -1, math.MaxFloat64
	for i, ln := range lines {
		st := h.state(site, ln.key)
		if st.down || skip[ln] {
			continue
		}
		ms := st.ms
		if ms == 0 {
			ms = math.MaxFloat64 / 2
		}
		if ms < bestMs {
			best, bestMs = i, ms
		}
	}
	return best
}

// switchLocked 切换正在用的线路并记录、推送。调用方需持有 h.mu。
func (h *lineHub) switchLocked(site, name string, lines []*upLine, from, to int, why string) {
	h.active[site] = lines[to].key
	text := fmt.Sprintf("切换到%s（%s）", lineName(to), why)
	h.event(site, text)
	log.Printf("站点 %s %s", name, text)
	if time.Since(h.lastSent[site]) >= 5*time.Minute {
		h.lastSent[site] = time.Now()
		h.send("Emby 反代：站点「" + name + "」" + text)
	}
}

func (h *lineHub) send(text string) {
	if h.notify != nil {
		h.notify(text)
	}
}

// reportFail 记一次请求失败（连不上或 502/504）。
func (h *lineHub) reportFail(site string, ln *upLine, msg string) {
	h.mu.Lock()
	st := h.state(site, ln.key)
	st.okN = 0
	st.failN++
	st.lastErr = msg
	if st.failN >= lineFailDown {
		st.down = true
	}
	h.mu.Unlock()
}

// reportOK 记一次请求成功：清掉连续失败计数（不可用的线路只靠探测恢复，避免偶尔成功一次就切回去）。
func (h *lineHub) reportOK(site string, ln *upLine) {
	h.mu.Lock()
	if st := h.state(site, ln.key); !st.down {
		st.failN = 0
	}
	h.mu.Unlock()
}

// nextLine 返回还没试过的线路里最合适的一条：先挑可用的，没有可用的就按顺序挑没试过的。
func (s *SiteRT) nextLine(tried map[*upLine]bool) *upLine {
	h := &s.m.lines
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := h.best(s.cfg.ID, s.lines, tried); i >= 0 {
		return s.lines[i]
	}
	for _, ln := range s.lines {
		if !tried[ln] {
			return ln
		}
	}
	return nil
}

// lineTransport 在连不上上游或上游返回 502 / 504 时换一条线路重试（只对第一跳、没有请求体的请求）。
type lineTransport struct {
	base http.RoundTripper
	s    *SiteRT
	rc   *reqCtx
	used bool // 只处理 ReverseProxy 发出的第一跳；跟随跳转的后续请求原样转发
}

func (t *lineTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	ln := t.rc.line
	if t.used || ln == nil || t.rc.ep {
		return t.base.RoundTrip(req)
	}
	t.used = true
	replay := (req.Body == nil || req.Body == http.NoBody) && req.Header.Get("Upgrade") == ""
	retry := len(t.s.lines) > 1 && replay
	rest := t.rc.rest
	if t.s.embyLearned.Load() && !hasEmbyPrefix(rest) {
		rest = "/emby" + rest
		req = lineReq(req, ln, rest)
	}
	tried := map[*upLine]bool{}
	for {
		tried[ln] = true
		resp, err := t.base.RoundTrip(req)
		if req.Context().Err() != nil { // 客户端已断开，不算线路的错
			return resp, err
		}
		msg := ""
		switch {
		case err != nil:
			msg = simplifyNetErr(err)
		case resp.StatusCode == http.StatusBadGateway || resp.StatusCode == http.StatusGatewayTimeout:
			msg = "HTTP " + resp.Status
		}
		if msg == "" {
			t.s.m.lines.reportOK(t.s.cfg.ID, ln)
			if resp.StatusCode == http.StatusNotFound && replay && t.s.tryEmby(req.Method, rest) {
				return t.embyRetry(req, ln, rest, resp), nil
			}
			return resp, nil
		}
		t.s.m.lines.reportFail(t.s.cfg.ID, ln, msg)
		if !retry {
			return resp, err
		}
		next := t.s.nextLine(tried)
		if next == nil {
			return resp, err
		}
		if resp != nil {
			resp.Body.Close()
		}
		req, ln = lineReq(req, next, rest), next
		t.rc.line = next
		t.rc.target = next.u
	}
}

// lineReq 把请求改发到另一条线路（或同一条线路的另一个路径）。
func lineReq(req *http.Request, ln *upLine, rest string) *http.Request {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = ln.u.Scheme, ln.u.Host, ln.u.Host
	out.URL.RawPath = ln.base + rest
	out.URL.Path, _ = url.PathUnescape(out.URL.RawPath)
	origin := ln.u.Scheme + "://" + ln.u.Host
	if out.Header.Get("Origin") != "" {
		out.Header.Set("Origin", origin)
	}
	if out.Header.Get("Referer") != "" {
		out.Header.Set("Referer", origin+"/")
	}
	return out
}

// ---- 自动补 /emby 前缀 ----
// 有的上游只认 /emby/xxx（客户端却按 /xxx 请求，或上游地址里的路径填错了）。
// 不带 /emby 的请求返回 404 时补上 /emby 再试一次；补上后成功就记住，之后的请求直接补；
// 连续 20 次补了也没用就不再尝试（避免对真正不存在的地址多发一倍请求）。

func hasEmbyPrefix(rest string) bool {
	l := strings.ToLower(rest)
	return l == "/emby" || strings.HasPrefix(l, "/emby/")
}

func (s *SiteRT) tryEmby(method, rest string) bool {
	if method != http.MethodGet && method != http.MethodHead {
		return false
	}
	l := strings.ToLower(rest)
	return !hasEmbyPrefix(rest) && !strings.Contains(l, "/images/") && !s.embyLearned.Load() && s.embyMiss.Load() < 20
}

func (t *lineTransport) embyRetry(req *http.Request, ln *upLine, rest string, orig *http.Response) *http.Response {
	r2, err := t.base.RoundTrip(lineReq(req, ln, "/emby"+rest))
	if err == nil && r2.StatusCode < 400 {
		orig.Body.Close()
		if !t.s.embyLearned.Swap(true) {
			log.Printf("站点 %s：上游需要 /emby 前缀，之后自动补上", t.s.cfg.Name)
		}
		t.s.embyMiss.Store(0)
		return r2
	}
	if r2 != nil {
		r2.Body.Close()
	}
	t.s.embyMiss.Add(1)
	return orig
}

// ---- 主动探测 ----

// probeLine 请求线路的 /System/Info/Public，返回响应头到达的耗时。
func (m *Manager) probeLine(s Site, ln *upLine) (time.Duration, error) {
	client := &http.Client{
		Transport:     m.transportPref(s.InsecureTLS, s.IgnoreExpired, m.sitePref(&s)),
		Timeout:       lineTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	ctx, cancel := context.WithTimeout(context.Background(), lineTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", ln.u.String()+ln.base+"/System/Info/Public", nil)
	if err != nil {
		return 0, err
	}
	ua := s.UserAgent
	if ua == "" {
		ua = testUA
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "application/json")
	for k, v := range s.Headers { // 只带不含占位符的自定义请求头（占位符要有客户端请求才能展开）
		if v != "" && !strings.Contains(v, "{") {
			req.Header.Set(k, v)
		}
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	d := time.Since(start)
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return d, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return d, nil
}

// lineLoop 定时探测各站点的线路。
func (m *Manager) lineLoop() {
	for range time.Tick(5 * time.Second) {
		m.probeDue(time.Now(), false)
	}
}

// probeDue 探测到期的站点；wait 为 true 时等探测结束（测试用）。
func (m *Manager) probeDue(now time.Time, wait bool) {
	m.mu.RLock()
	var due []*SiteRT
	for _, rt := range m.sites {
		due = append(due, rt)
	}
	m.mu.RUnlock()
	h := &m.lines
	var wg sync.WaitGroup
	for _, rt := range due {
		id := rt.cfg.ID
		h.mu.Lock()
		h.init()
		if h.inflight[id] || now.Before(h.next[id]) {
			h.mu.Unlock()
			continue
		}
		iv := lineProbeMany
		if len(rt.lines) == 1 {
			iv = lineProbeOne
		}
		h.inflight[id], h.next[id] = true, now.Add(iv)
		h.mu.Unlock()
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.probeSite(rt)
		}()
	}
	if wait {
		wg.Wait()
	}
}

// probeSite 并行探测一个站点的所有线路，然后重新选路。
func (m *Manager) probeSite(rt *SiteRT) {
	h := &m.lines
	probe := h.probe
	if probe == nil {
		probe = m.probeLine
	}
	type res struct {
		d   time.Duration
		err error
	}
	out := make([]res, len(rt.lines))
	var wg sync.WaitGroup
	for i, ln := range rt.lines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := probe(rt.cfg, ln)
			out[i] = res{d, err}
		}()
	}
	wg.Wait()

	h.mu.Lock()
	defer h.mu.Unlock()
	h.inflight[rt.cfg.ID] = false
	for i, ln := range rt.lines {
		st := h.state(rt.cfg.ID, ln.key)
		st.checked = time.Now()
		if out[i].err != nil {
			st.okN, st.failN, st.lastErr = 0, st.failN+1, simplifyNetErr(out[i].err)
			if st.failN >= lineFailDown {
				st.down = true
			}
			continue
		}
		ms := float64(out[i].d.Microseconds())/1000 + 0.01
		if st.ms == 0 {
			st.ms = ms
		} else {
			st.ms = st.ms*0.7 + ms*0.3
		}
		st.okN, st.failN, st.lastErr = st.okN+1, 0, ""
		if st.down && st.okN >= lineOKUp {
			st.down = false
		}
	}
	h.chooseLocked(rt)
}

// chooseLocked 根据最新的探测结果选路。调用方需持有 h.mu。
func (h *lineHub) chooseLocked(rt *SiteRT) {
	id, name, lines := rt.cfg.ID, rt.cfg.Name, rt.lines
	cur := 0
	for i, ln := range lines {
		if ln.key == h.active[id] {
			cur = i
		}
	}
	best := h.best(id, lines, nil)
	if best < 0 {
		if !h.allDown[id] {
			h.allDown[id] = true
			what := "上游不可用"
			if len(lines) > 1 {
				what = "全部线路不可用"
			}
			st := h.state(id, lines[cur].key)
			h.event(id, what+"："+st.lastErr)
			log.Printf("站点 %s %s：%s", name, what, st.lastErr)
			h.send("Emby 反代：站点「" + name + "」" + what + "（" + st.lastErr + "）")
		}
		return
	}
	if h.allDown[id] {
		h.allDown[id] = false
		h.event(id, lineName(best)+"已恢复")
		log.Printf("站点 %s %s已恢复", name, lineName(best))
		h.send("Emby 反代：站点「" + name + "」已恢复")
	}
	h.active[id] = lines[cur].key
	if best == cur {
		return
	}
	cs, bs := h.state(id, lines[cur].key), h.state(id, lines[best].key)
	switch {
	case cs.down:
		h.switchLocked(id, name, lines, cur, best, lineName(cur)+"不可用："+cs.lastErr)
	case cs.ms > 0 && bs.ms*lineSwitchPct < cs.ms && cs.ms-bs.ms >= lineSwitchMs:
		h.switchLocked(id, name, lines, cur, best, fmt.Sprintf("延迟 %.0fms，%s %.0fms", bs.ms, lineName(cur), cs.ms))
	}
}

// ---- 面板 ----

type lineView struct {
	URL     string `json:"url"`
	Name    string `json:"name"`
	Active  bool   `json:"active"`
	Down    bool   `json:"down"`
	Ms      int    `json:"ms,omitempty"`
	Checked int64  `json:"checked,omitempty"`
	Err     string `json:"err,omitempty"`
}

// lineViews 返回站点各线路的状态（站点没在运行时返回 nil）。调用方需持有 m.mu 读锁。
func (m *Manager) lineViews(id string) ([]lineView, []lineEvent) {
	rt := m.sites[id]
	if rt == nil {
		return nil, nil
	}
	h := &m.lines
	h.mu.Lock()
	defer h.mu.Unlock()
	h.init()
	act := h.active[id]
	if act == "" {
		act = rt.lines[0].key
	}
	out := make([]lineView, len(rt.lines))
	for i, ln := range rt.lines {
		st := h.state(id, ln.key)
		out[i] = lineView{URL: ln.key, Name: lineName(i), Active: ln.key == act, Down: st.down,
			Ms: int(math.Round(st.ms)), Err: st.lastErr}
		if !st.checked.IsZero() {
			out[i].Checked = st.checked.Unix()
		}
	}
	return out, append([]lineEvent(nil), h.events[id]...)
}

// activeUpstreamLocked 返回站点正在用的线路地址（保号直连上游时用），站点没在运行时返回主线路。调用方需持有 m.mu 读锁。
func (m *Manager) activeUpstreamLocked(s *Site) string {
	rt := m.sites[s.ID]
	if rt == nil {
		return s.Upstream
	}
	ln := rt.activeLine()
	return ln.u.String() + ln.base
}
