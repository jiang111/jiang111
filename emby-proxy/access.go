package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 访问控制：对所有站点的客户端请求生效（面板不受影响，改错了也能在面板改回来）。
//   - 白名单模式：只放行列表里的 IP / 网段 / 国家；黑名单模式：拦截列表里的。
//   - 国家按 IP 归属判断（各大洲 RIR 公开的分配数据，见 geo.go），不信任客户端自己带的 CF-IPCountry 之类请求头。
//   - 信任 IP 永远放行、永不封禁；本机自己发出的请求（保号等）、各节点之间的转发也不受限制。
//   - 自动封禁：同一 IP 10 分钟内多次登录失败（上游对 /Users/AuthenticateByName 回 401）或多次访问密钥不对，
//     封禁一段时间。撞库会让上游把反代服务器的 IP 整个封掉、连累所有用户，在这里先挡住。
// 设置在主节点修改，同步给从节点；封禁记录每台各自记在内存里。

type AccessConfig struct {
	Mode       string   `json:"mode"` // off | allow（白名单） | deny（黑名单）
	IPs        []string `json:"ips,omitempty"`
	Countries  []string `json:"countries,omitempty"` // ISO 两位国家码
	Trusted    []string `json:"trusted,omitempty"`   // 永远放行
	BlockURL   string   `json:"block_url,omitempty"` // 被拦时 302 到这里，空 = 显示拒绝页
	AutoBan    bool     `json:"auto_ban"`
	BanHits    int      `json:"ban_hits,omitempty"`    // 10 分钟内失败几次封禁，默认 10
	BanMinutes int      `json:"ban_minutes,omitempty"` // 封禁多久，默认 1440
}

const banWindow = 10 * time.Minute

var ccRe = regexp.MustCompile(`^[A-Z]{2}$`)

// normalize 校验并整理访问控制设置。
func (a *AccessConfig) normalize() error {
	a.Mode = strings.ToLower(strings.TrimSpace(a.Mode))
	switch a.Mode {
	case "", "off":
		a.Mode = "off"
	case "allow", "deny":
	default:
		return errors.New("访问控制模式只能是 off / allow / deny")
	}
	clean := func(list []string, what string) ([]string, error) {
		var out []string
		for _, s := range list {
			s = strings.TrimSpace(s)
			if s == "" || slices.Contains(out, s) {
				continue
			}
			if _, err := parseNet(s); err != nil {
				return nil, fmt.Errorf("%s「%s」不是有效的 IP 或网段", what, s)
			}
			out = append(out, s)
		}
		return out, nil
	}
	var err error
	if a.IPs, err = clean(a.IPs, "IP"); err != nil {
		return err
	}
	if a.Trusted, err = clean(a.Trusted, "信任 IP"); err != nil {
		return err
	}
	var cc []string
	for _, c := range a.Countries {
		c = strings.ToUpper(strings.TrimSpace(c))
		if c == "" || slices.Contains(cc, c) {
			continue
		}
		if !ccRe.MatchString(c) {
			return fmt.Errorf("国家码「%s」应为两位字母，如 CN、US", c)
		}
		cc = append(cc, c)
	}
	a.Countries = cc
	a.BlockURL = strings.TrimSpace(a.BlockURL)
	if a.BlockURL != "" {
		u, err := url.Parse(a.BlockURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("跳转网址应为 http(s)://…")
		}
	}
	if a.BanHits <= 0 {
		a.BanHits = 10
	}
	if a.BanMinutes <= 0 {
		a.BanMinutes = 1440
	}
	return nil
}

func parseNet(s string) (*net.IPNet, error) {
	if strings.Contains(s, "/") {
		_, n, err := net.ParseCIDR(s)
		return n, err
	}
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, errors.New("bad ip")
	}
	bits := 128
	if ip4 := ip.To4(); ip4 != nil {
		ip, bits = ip4, 32
	}
	return &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)}, nil
}

func parseNets(list []string) []*net.IPNet {
	var out []*net.IPNet
	for _, s := range list {
		if n, err := parseNet(s); err == nil {
			out = append(out, n)
		}
	}
	return out
}

func inNets(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// aclRT 是编译好的访问控制（随配置 reload 重建）。
type aclRT struct {
	cfg       AccessConfig
	nets      []*net.IPNet
	trusted   []*net.IPNet
	countries map[string]bool
}

func newACL(c *AccessConfig) *aclRT {
	if c == nil {
		return &aclRT{cfg: AccessConfig{Mode: "off"}}
	}
	a := &aclRT{cfg: *c, nets: parseNets(c.IPs), trusted: parseNets(c.Trusted), countries: map[string]bool{}}
	for _, cc := range c.Countries {
		a.countries[cc] = true
	}
	return a
}

// ---- 自动封禁 ----

type banEntry struct {
	IP     string `json:"ip"`
	Until  int64  `json:"until"`
	Reason string `json:"reason"`
	CC     string `json:"cc,omitempty"`
}

type banList struct {
	mu    sync.Mutex
	hits  map[string][]time.Time
	until map[string]banEntry
}

func (b *banList) init() {
	if b.hits == nil {
		b.hits, b.until = map[string][]time.Time{}, map[string]banEntry{}
	}
}

func (b *banList) banned(ip string, now time.Time) (banEntry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	e, ok := b.until[ip]
	if ok && now.Unix() >= e.Until {
		delete(b.until, ip)
		return e, false
	}
	return e, ok
}

// hit 记一次失败，达到次数时封禁，返回是否刚刚被封。
func (b *banList) hit(ip, reason string, limit int, dur time.Duration, now time.Time) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	if _, ok := b.until[ip]; ok {
		return false
	}
	h := b.hits[ip][:0:0]
	for _, t := range b.hits[ip] {
		if now.Sub(t) < banWindow {
			h = append(h, t)
		}
	}
	h = append(h, now)
	if len(h) >= limit {
		delete(b.hits, ip)
		b.until[ip] = banEntry{IP: ip, Until: now.Add(dur).Unix(), Reason: reason}
		return true
	}
	b.hits[ip] = h
	if len(b.hits) > 100000 { // 被大量不同 IP 扫描时不让表无限增长
		for k, v := range b.hits {
			if now.Sub(v[len(v)-1]) >= banWindow {
				delete(b.hits, k)
			}
		}
	}
	return false
}

func (b *banList) unban(ip string) {
	b.mu.Lock()
	b.init()
	delete(b.until, ip)
	delete(b.hits, ip)
	b.mu.Unlock()
}

func (b *banList) list(now time.Time) []banEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.init()
	out := []banEntry{}
	for ip, e := range b.until {
		if now.Unix() >= e.Until {
			delete(b.until, ip)
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Until > out[j].Until })
	return out
}

// ---- 本机自己发出的请求 ----

const selfHeader = "X-Emby-Proxy-Self"

// selfToken 是本机（或同一集群的节点）发出的请求带的签名，访问控制见到它就放行（例如保号经客户端地址访问自己）。
func (m *Manager) selfToken() string {
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	m.mu.RLock()
	key := m.cfg.ClusterSecret
	m.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("self|" + ts))
	return ts + "." + hex.EncodeToString(mac.Sum(nil))[:32]
}

func (m *Manager) verifySelf(v string) bool {
	ts, sig, ok := strings.Cut(v, ".")
	if !ok {
		return false
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || time.Since(time.Unix(t, 0)).Abs() > 5*time.Minute {
		return false
	}
	m.mu.RLock()
	key := m.cfg.ClusterSecret
	m.mu.RUnlock()
	mac := hmac.New(sha256.New, []byte(key))
	mac.Write([]byte("self|" + ts))
	return hmac.Equal([]byte(sig), []byte(hex.EncodeToString(mac.Sum(nil))[:32]))
}

// selfTransport 给本机发出的请求带上 selfHeader。
type selfTransport struct {
	base http.RoundTripper
	m    *Manager
}

func (t selfTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set(selfHeader, t.m.selfToken())
	return t.base.RoundTrip(req)
}

// ---- 判断 ----

// isPeerIP 判断 IP 是不是集群里的某台服务器（节点之间互相探测、转发不能被拦）。
func (m *Manager) isPeerIP(ip string) bool {
	for _, mb := range m.members() {
		if hostIP(mb.Addr) == ip {
			return true
		}
	}
	return false
}

// checkAccess 判断客户端 IP 能否访问，不能时返回原因。
func (m *Manager) checkAccess(r *http.Request, ipStr string) (string, bool) {
	a := m.acl.Load()
	ip := net.ParseIP(ipStr)
	if a == nil || ip == nil || ip.IsLoopback() || ip.IsPrivate() {
		return "", true
	}
	if inNets(ip, a.trusted) {
		return "", true
	}
	if v := r.Header.Get(selfHeader); v != "" && m.verifySelf(v) {
		return "", true
	}
	if e, ok := m.bans.banned(ipStr, time.Now()); ok {
		return "自动封禁：" + e.Reason, false
	}
	if a.cfg.Mode == "off" {
		return "", true
	}
	hit := inNets(ip, a.nets)
	if !hit && len(a.countries) > 0 {
		if cc := m.geoLookup(ip); cc != "" {
			hit = a.countries[cc]
		} else if a.cfg.Mode == "allow" && m.geo.Load() == nil {
			hit = true // 国家数据还没下载好时白名单不拦（避免刚开启就把所有人挡在外面）
		}
	}
	if a.cfg.Mode == "allow" && !hit {
		if m.isPeerIP(ipStr) {
			return "", true
		}
		return "不在白名单", false
	}
	if a.cfg.Mode == "deny" && hit {
		if m.isPeerIP(ipStr) {
			return "", true
		}
		return "黑名单", false
	}
	return "", true
}

// noteFailure 记一次可疑行为（登录失败、访问密钥不对），开启了自动封禁时累计到次数就封。
func (m *Manager) noteFailure(ipStr, reason string) {
	a := m.acl.Load()
	if a == nil || !a.cfg.AutoBan {
		return
	}
	ip := net.ParseIP(ipStr)
	if ip == nil || ip.IsLoopback() || ip.IsPrivate() || inNets(ip, a.trusted) || m.isPeerIP(ipStr) {
		return
	}
	if m.bans.hit(ipStr, reason, a.cfg.BanHits, time.Duration(a.cfg.BanMinutes)*time.Minute, time.Now()) {
		m.lines.send(fmt.Sprintf("Emby 反代：已自动封禁 %s %d 分钟（%s）", ipStr, a.cfg.BanMinutes, reason))
	}
}

func (m *Manager) denyPage(w http.ResponseWriter, ip, reason string) int {
	a := m.acl.Load()
	w.Header().Set("Cache-Control", "no-store")
	if a != nil && a.cfg.BlockURL != "" {
		w.Header().Set("Location", a.cfg.BlockURL)
		w.WriteHeader(http.StatusFound)
		return http.StatusFound
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>访问被拒绝</title>
<body style="font-family:-apple-system,sans-serif;display:grid;place-items:center;min-height:90vh;color:#333"><div style="text-align:center">
<h2>你的 IP 已被禁止访问</h2><p>%s</p><p style="color:#888">%s</p></div></body>`, html.EscapeString(ip), html.EscapeString(reason))
	return http.StatusForbidden
}

// isLoginPath 判断是不是 Emby 的用户名密码登录接口。
func isLoginPath(p string) bool {
	return strings.HasSuffix(p, "/users/authenticatebyname") || strings.HasSuffix(p, "/users/authenticate")
}

// ---- 访问密钥（站点级） ----

var accessKeyRe = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)

// catchEntry 是一次访问密钥不对的请求（多半是扫描器），面板「门禁记录」里查看。
type catchEntry struct {
	Time  int64  `json:"t"`
	Site  string `json:"site"`
	IP    string `json:"ip"`
	CC    string `json:"cc,omitempty"`
	Path  string `json:"path"`
	UA    string `json:"ua"`
	Times int    `json:"times"`
}

type catchLog struct {
	mu   sync.Mutex
	list []catchEntry // 最新的在前
}

func (c *catchLog) add(e catchEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i := range c.list {
		if c.list[i].IP == e.IP && c.list[i].Path == e.Path && c.list[i].Site == e.Site {
			old := c.list[i]
			old.Times++
			old.Time = e.Time
			c.list = append(append([]catchEntry{old}, c.list[:i]...), c.list[i+1:]...)
			return
		}
	}
	e.Times = 1
	c.list = append([]catchEntry{e}, c.list...)
	if len(c.list) > 500 {
		c.list = c.list[:500]
	}
}

func (c *catchLog) all() []catchEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]catchEntry{}, c.list...)
}

func (c *catchLog) clear() {
	c.mu.Lock()
	c.list = nil
	c.mu.Unlock()
}

// keyGate 检查访问密钥：开启了密钥的站点，路径必须以 /密钥 开头。通过时返回加上密钥后的前缀。
func (s *SiteRT) keyGate(w http.ResponseWriter, r *http.Request, prefix, ip string) (string, bool) {
	key := s.cfg.AccessKey
	if key == "" {
		return prefix, true
	}
	esc := r.URL.EscapedPath()
	want := prefix + "/" + key
	if esc == want || strings.HasPrefix(esc, want+"/") {
		return want, true
	}
	s.m.catches.add(catchEntry{Time: time.Now().Unix(), Site: s.cfg.Name, IP: ip, CC: s.m.geoLookupStr(ip),
		Path: maskURI(r.URL.RequestURI()), UA: shortUA(r.UserAgent())})
	s.m.noteFailure(ip, "访问密钥不对")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	fmt.Fprintf(w, `<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>404</title>
<body style="font-family:-apple-system,sans-serif;display:grid;place-items:center;min-height:90vh;color:#333"><div style="text-align:center">
<h2>抓到你了 👀</h2><p>%s</p><p style="color:#888;font-size:13px">%s</p></div></body>`, html.EscapeString(ip), html.EscapeString(r.UserAgent()))
	return "", false
}

// keyPath 返回站点访问密钥对应的路径段（没开启时为空）。
func keyPath(s *Site) string {
	if s.AccessKey == "" {
		return ""
	}
	return "/" + s.AccessKey
}

// clientBase 是客户端填写的地址：客户端访问地址 + 访问密钥。
func clientBase(s *Site) string {
	if s.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(s.PublicURL, "/") + keyPath(s)
}
