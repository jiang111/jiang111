package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// 各播放器的导入链接：参数名、顺序、编码（空格 %20、& # ? 中文都要编码）、默认端口、带路径时跳过没有路径参数的播放器。
func TestImportLinks(t *testing.T) {
	links := importLinks("A 服#1", "https://emby.example.com:8001", "user@x", "p&w d#?好")
	got := map[string]string{}
	for _, l := range links {
		got[l.ID] = l.URL
	}
	want := map[string]string{
		"senplayer": "senplayer://importserver?type=emby&name=A%20%E6%9C%8D%231&address=https%3A%2F%2Femby.example.com%3A8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD",
		"rex":       "rex://import?type=emby&scheme=https&host=emby.example.com&port=8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD&title=A%20%E6%9C%8D%231",
		"forward":   "forward://import?type=emby&scheme=https&host=emby.example.com&port=8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD&title=A%20%E6%9C%8D%231",
		"hills":     "hills://import?type=emby&scheme=https&host=emby.example.com&port=8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD",
		"rodel":     "rodelplayer://import?type=emby&title=A%20%E6%9C%8D%231&scheme=https&host=emby.example.com&port=8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD",
		"bmh":       "https://bmh.163.com/la/?import=emby&name=A%20%E6%9C%8D%231&scheme=https&host=emby.example.com&port=8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD",
		"eplayerx":  "eplayerx://add-or-update?type=emby&href=https%3A%2F%2Femby.example.com%3A8001&username=user%40x&password=p%26w%20d%23%3F%E5%A5%BD",
	}
	if len(got) != len(want) {
		t.Fatalf("links: %v", got)
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("%s:\n got %s\nwant %s", id, got[id], w)
		}
	}
	if links[0].ID != "senplayer" {
		t.Fatal("SenPlayer first")
	}

	// 没写端口按协议补；带路径时 SenPlayer 地址带路径，小幻影视用 path，Forward / Rex / Hills 不生成
	ids := []string{}
	for _, l := range importLinks("B", "http://1.2.3.4/emby-a/", "u", "") {
		ids = append(ids, l.ID)
		switch l.ID {
		case "senplayer":
			if !strings.Contains(l.URL, "address=http%3A%2F%2F1.2.3.4%2Femby-a&username=u") || strings.Contains(l.URL, "password") {
				t.Errorf("senplayer with path: %s", l.URL)
			}
		case "rodel":
			if !strings.Contains(l.URL, "port=80&path=%2Femby-a&username=u") {
				t.Errorf("rodel with path: %s", l.URL)
			}
		}
	}
	if strings.Join(ids, ",") != "senplayer,rodel,bmh,eplayerx" {
		t.Fatalf("with path: %v", ids)
	}
	if importLinks("C", "ftp://x", "u", "p") != nil || importLinks("C", "not a url", "u", "p") != nil {
		t.Fatal("bad address")
	}
}

// 导出：页面不含账号；数据要导出密码（和面板密码分开、可重新生成、输错计入限额）；没有客户端地址的跳过；
// 从节点不导出；导出密码不在配置导出和同步里。
func TestExportAPI(t *testing.T) {
	m, srv, ck := newAdmin(t, &Config{})
	c := newCluster(m)
	do := func(method, path, body string) (int, string) {
		return adminDo(t, srv, ck, method, path, body, csrfHeader, "1")
	}
	anon := func(method, path, body string) (int, string) {
		return adminDo(t, srv, nil, method, path, body, csrfHeader, "1")
	}
	for _, b := range []string{
		`{"name":"A服","upstream":"https://a.example.net","mode":"port","port":18001,"public_url":"https://emby.example.com:8001","enabled":true}`,
		`{"name":"B","upstream":"https://b.example.net","mode":"path","path":"b","enabled":true}`,
	} {
		if code, out := do("POST", "/api/sites", b); code != 200 {
			t.Fatal(out)
		}
	}
	for _, s := range m.cfg.Sites {
		do("PUT", "/api/sites/"+s.ID+"/account", `{"username":"alice","password":"s3cret&x"}`)
	}

	// 页面：不用登录，不含账号
	code, page := anon("GET", "/export?key=whatever", "")
	if code != 200 || strings.Contains(page, "s3cret") || strings.Contains(page, "alice") || !strings.Contains(page, "/export/data") {
		t.Fatalf("page: %d", code)
	}

	// 还没生成导出密码：拒绝
	if code, _ := anon("POST", "/export/data", `{"key":""}`); code != 403 {
		t.Fatalf("no key configured: %d", code)
	}
	if code, _ := anon("POST", "/api/export-key", ""); code != 401 {
		t.Fatalf("reset needs login: %d", code)
	}
	_, out := do("POST", "/api/export-key", "")
	var r struct{ Key string }
	json.Unmarshal([]byte(out), &r)
	if len(r.Key) != 32 || m.cfg.ExportKey != r.Key {
		t.Fatalf("key: %s", out)
	}
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, `"export_key":"`+r.Key+`"`) {
		t.Fatal("state should show export key")
	}
	// 面板登录密码不能当导出密码用
	if code, _ := anon("POST", "/export/data", `{"key":"pw123456"}`); code != 403 {
		t.Fatalf("panel password must not work: %d", code)
	}
	// 没带自定义头（跨站页面伪造）拒绝
	if code, _ := adminDo(t, srv, nil, "POST", "/export/data", `{"key":"`+r.Key+`"}`); code != 403 {
		t.Fatalf("csrf: %d", code)
	}
	code, data := anon("POST", "/export/data", `{"key":"`+r.Key+`"}`)
	var d struct {
		Sites   []exportSite
		Skipped []string
	}
	json.Unmarshal([]byte(data), &d)
	if code != 200 || len(d.Sites) != 1 || d.Sites[0].Name != "A服" || d.Sites[0].Password != "s3cret&x" || d.Sites[0].URL != "https://emby.example.com:8001" ||
		len(d.Sites[0].Links) != 7 || len(d.Skipped) != 1 || !strings.Contains(d.Skipped[0], "没有客户端访问地址") {
		t.Fatalf("data: %d %s", code, data)
	}

	// 导出密码不在配置导出、同步里
	_, exp := do("GET", "/api/export", "")
	payload, _, _ := c.payload()
	if strings.Contains(exp, r.Key) || strings.Contains(string(payload), r.Key) {
		t.Fatal("export key leaked into config export / sync payload")
	}

	// 重新生成：旧的失效；关闭：都不能用
	_, out = do("POST", "/api/export-key", "")
	var r2 struct{ Key string }
	json.Unmarshal([]byte(out), &r2)
	if code, _ := anon("POST", "/export/data", `{"key":"`+r.Key+`"}`); code != 403 || r2.Key == r.Key {
		t.Fatalf("old key should stop working: %d", code)
	}
	if code, _ := anon("POST", "/export/data", `{"key":"`+r2.Key+`"}`); code != 200 {
		t.Fatalf("new key: %d", code)
	}
	do("DELETE", "/api/export-key", "")
	if code, _ := anon("POST", "/export/data", `{"key":"`+r2.Key+`"}`); code != 403 || m.cfg.ExportKey != "" {
		t.Fatalf("disabled: %d", code)
	}

	// 输错和登录共用失败次数限额（上面已经错了 4 次）
	for i := 0; i < 3; i++ {
		anon("POST", "/export/data", `{"key":"wrong"}`)
	}
	if code, _ := anon("POST", "/export/data", `{"key":"wrong"}`); code != 429 {
		t.Fatalf("rate limit: %d", code)
	}

	m.mu.Lock()
	m.cfg.Cluster.Role, m.cfg.ExportKey = "node", "k"
	m.mu.Unlock()
	if code, _ := anon("POST", "/export/data", `{"key":"k"}`); code != 409 {
		t.Fatalf("node: %d", code)
	}
}

// nginx 面板配置不记访问日志（导出链接里有导出密码）。
func TestPanelNginxNoAccessLog(t *testing.T) {
	m, _, _ := newAdmin(t, &Config{})
	m.cfg.PanelURL = "https://master.example.com:8443"
	if conf := m.nginxConf(); !strings.Contains(conf, "access_log off;") {
		t.Fatal("panel server should not log access")
	}
}
