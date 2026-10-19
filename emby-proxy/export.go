package main

import (
	"crypto/subtle"
	_ "embed"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 一键导出账号：GET /export?key=导出密码 打开导出页，每个站点一行，提供导入各 Emby 播放器的链接（一条链接导入一台），
// 底部是文本清单。隐私：
//   - 导出密码和面板登录密码分开，面板上可以重新生成（旧链接立即失效）；只在主节点 / 单机，不同步、不导出。
//   - 导出页 HTML 里没有任何账号，打开后由页面脚本 POST /export/data 取数据：Telegram、微信等自动抓取链接做预览时
//     不执行脚本，拿不到账号。没有直接 GET 下载数据的格式，txt / json 由页面生成。
//   - 导出密码错误和面板登录共用失败次数限额；生成的 nginx 面板配置不记访问日志（链接里有导出密码）。
// 各播放器的链接格式见 importLinks 的注释。

//go:embed web/export.html
var exportHTML []byte

type playerLink struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	URL  string `json:"url"`
	Note string `json:"note,omitempty"`
}

type exportSite struct {
	Name     string       `json:"name"`
	URL      string       `json:"url"`
	Username string       `json:"username"`
	Password string       `json:"password"`
	Disabled bool         `json:"disabled,omitempty"`
	Links    []playerLink `json:"links"`
}

// enc 按 RFC 3986 编码参数值（空格编码为 %20，不是 +）。
func enc(s string) string { return strings.ReplaceAll(url.QueryEscape(s), "+", "%20") }

// query 按给定顺序拼参数，值为空的跳过。
func query(kv ...string) string {
	var parts []string
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] != "" {
			parts = append(parts, kv[i]+"="+enc(kv[i+1]))
		}
	}
	return strings.Join(parts, "&")
}

// importLinks 生成各播放器「导入服务器」的链接（都是一条链接导入一台）：
//   - SenPlayer：senplayer://importserver（官方 URLSchemes.md，5.7.0+），address 是完整地址
//   - Rex：rex://import，参数和 Forward 相同，只有第三方代码，未实测；免费版只能加 1 个资源库
//   - Forward：forward://import（官方文档；App 已下架，只对已安装的有用）
//   - Hills（Android）：hills://import（安装包注册了该 scheme，参数名来自第三方代码）
//   - 小幻影视（Windows）：rodelplayer://import（官方文档）
//   - 网易爆米花：https://bmh.163.com/la/?import=emby（官方文档；没装 App 时会打开网易网页，账号随链接发给网易）
//   - EplayerX：eplayerx://add-or-update（只有第三方代码，未实测）
//
// Forward / Rex / Hills 没有路径参数，地址带路径时不生成。
func importLinks(name, addr, user, pw string) []playerLink {
	u, err := url.Parse(addr)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil
	}
	scheme, host, port := u.Scheme, u.Hostname(), u.Port()
	if port == "" {
		port = map[string]string{"http": "80", "https": "443"}[scheme]
	}
	path := strings.TrimRight(u.Path, "/")
	base := scheme + "://" + u.Host + path
	var out []playerLink
	add := func(id, n, link, note string) { out = append(out, playerLink{id, n, link, note}) }
	add("senplayer", "SenPlayer", "senplayer://importserver?type=emby&"+query("name", name, "address", base, "username", user, "password", pw), "")
	if path == "" {
		hp := query("scheme", scheme, "host", host, "port", port, "username", user, "password", pw)
		add("rex", "Rex", "rex://import?type=emby&"+hp+"&"+query("title", name), "未实测；免费版只能加 1 个资源库")
		add("forward", "Forward", "forward://import?type=emby&"+hp+"&"+query("title", name), "App 已下架，只对已安装的有用")
		add("hills", "Hills", "hills://import?type=emby&"+hp, "Android")
	}
	add("rodel", "小幻影视", "rodelplayer://import?type=emby&"+query("title", name, "scheme", scheme, "host", host, "port", port,
		"path", path, "username", user, "password", pw), "Windows")
	add("bmh", "网易爆米花", "https://bmh.163.com/la/?import=emby&"+query("name", name, "scheme", scheme, "host", host, "port", port,
		"path", path, "username", user, "password", pw), "没装 App 时会打开网易网页，账号会随链接发给网易")
	add("eplayerx", "EplayerX", "eplayerx://add-or-update?type=emby&"+query("href", base, "username", user, "password", pw), "未实测")
	return out
}

// serveExportPage：GET /export，页面本身不含账号（导出密码在链接里，由页面脚本带去取数据）。
func (a *Admin) serveExportPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Robots-Tag", "noindex, nofollow")
	w.Write(exportHTML)
}

// exportData：POST /export/data {"key": 导出密码}，返回各站点的账号和导入链接。
func (a *Admin) exportData(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var body struct {
		Key string `json:"key"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	m := a.m
	if m.isNode() {
		writeErr(w, http.StatusConflict, "这台是从节点，没有保存账号，请打开主节点的导出链接")
		return
	}
	ip := clientIP(r)
	if !a.reserveLogin(ip) {
		writeErr(w, http.StatusTooManyRequests, "失败次数过多，请 10 分钟后再试")
		return
	}
	m.mu.RLock()
	want := m.cfg.ExportKey
	m.mu.RUnlock()
	if want == "" || subtle.ConstantTimeCompare([]byte(body.Key), []byte(want)) != 1 {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusForbidden, "导出密码不正确或已重新生成，请到面板复制新的导出链接")
		return
	}
	a.releaseLogin(ip)

	key, keyErr := m.accountKey(false)
	m.mu.RLock()
	sites := []exportSite{}
	skipped := []string{}
	for _, s := range m.cfg.Sites {
		encd := m.cfg.Accounts[s.ID]
		if encd == "" {
			continue
		}
		if keyErr != nil {
			skipped = append(skipped, s.Name+"（"+keyErr.Error()+"）")
			continue
		}
		acc, err := openAccount(key, s.ID, encd)
		if err != nil {
			skipped = append(skipped, s.Name+"（"+err.Error()+"）")
			continue
		}
		addr := strings.TrimRight(s.PublicURL, "/")
		if addr == "" {
			skipped = append(skipped, s.Name+"（没有客户端访问地址）")
			continue
		}
		sites = append(sites, exportSite{Name: s.Name, URL: addr, Username: acc.Username, Password: acc.Password,
			Disabled: !s.Enabled, Links: importLinks(s.Name, addr, acc.Username, acc.Password)})
	}
	m.mu.RUnlock()
	writeJSON(w, 200, map[string]any{"sites": sites, "skipped": skipped})
}

// resetExportKey：POST /api/export-key（需要登录）生成新的导出密码，旧链接立即失效；DELETE 关闭导出链接。
func (a *Admin) resetExportKey(w http.ResponseWriter, r *http.Request) {
	m := a.m
	key := ""
	if r.Method == http.MethodPost {
		key = randHex(16)
	}
	m.mu.Lock()
	m.cfg.ExportKey = key
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "key": key})
}
