package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const geoSample = `2|apnic|20260101|1|19830613|20260101|+0800
apnic|*|ipv4|*|1|summary
apnic|JP|ipv4|1.0.16.0|4096|20110412|allocated
apnic|CN|ipv4|1.0.1.0|256|20110414|allocated
ripencc|DE|ipv6|2a00:1450::|32|20090101|allocated
arin|US|ipv4|8.8.8.0|256|19920101|assigned
`

func TestGeoLookup(t *testing.T) {
	db, err := parseGeo(strings.NewReader(geoSample))
	if err != nil {
		t.Fatal(err)
	}
	for ip, want := range map[string]string{
		"1.0.1.5": "CN", "1.0.16.1": "JP", "1.0.31.255": "JP", "1.0.32.0": "", "8.8.8.8": "US",
		"2a00:1450:4001::1": "DE", "2a00:1451::1": "", "9.9.9.9": "",
	} {
		if got := db.lookup(net.ParseIP(ip)); got != want {
			t.Errorf("%s: got %q want %q", ip, got, want)
		}
	}
}

// accessSetup：站点上游是假 Emby，客户端 IP 用 X-Real-Ip 模拟（来自本机回环时才认这个头）。
func accessSetup(t *testing.T, ac *AccessConfig, site Site) (*Manager, *httptest.Server) {
	m, entry, _, _ := setup(t, site)
	if ac != nil {
		if err := ac.normalize(); err != nil {
			t.Fatal(err)
		}
	}
	m.mu.Lock()
	m.cfg.Access = ac
	m.reload()
	m.mu.Unlock()
	db, _ := parseGeo(strings.NewReader(geoSample))
	m.geo.Store(db)
	return m, entry
}

func TestAccessAllowDeny(t *testing.T) {
	_, entry := accessSetup(t, &AccessConfig{Mode: "allow", IPs: []string{"5.5.5.0/24"}, Countries: []string{"jp"}, Trusted: []string{"7.7.7.7"}}, Site{Mode: "path", Path: "hk"})
	u := entry.URL + "/hk/emby/System/Info/Public"
	for ip, want := range map[string]int{"5.5.5.9": 200, "1.0.16.1": 200, "7.7.7.7": 200, "1.0.1.5": 403, "8.8.8.8": 403} {
		if resp, _ := get(t, u, "X-Real-Ip", ip); resp.StatusCode != want {
			t.Errorf("allow mode %s: got %d want %d", ip, resp.StatusCode, want)
		}
	}
	m2, entry2 := accessSetup(t, &AccessConfig{Mode: "deny", Countries: []string{"CN"}, BlockURL: "https://example.com/no"}, Site{Mode: "path", Path: "hk"})
	u2 := entry2.URL + "/hk/emby/System/Info/Public"
	if resp, _ := get(t, u2, "X-Real-Ip", "1.0.1.5"); resp.StatusCode != 302 || resp.Header.Get("Location") != "https://example.com/no" {
		t.Fatalf("deny mode CN should redirect: %d", resp.StatusCode)
	}
	if resp, _ := get(t, u2, "X-Real-Ip", "8.8.8.8"); resp.StatusCode != 200 {
		t.Fatalf("deny mode US should pass: %d", resp.StatusCode)
	}
	// 本机签名的请求（保号）不受限制
	if resp, _ := get(t, u2, "X-Real-Ip", "1.0.1.5", selfHeader, m2.selfToken()); resp.StatusCode != 200 {
		t.Fatalf("self request should pass: %d", resp.StatusCode)
	}
	if resp, _ := get(t, u2, "X-Real-Ip", "1.0.1.5", selfHeader, "1.bad"); resp.StatusCode != 302 {
		t.Fatal("forged self header must not pass")
	}
	logs := m2.logs.list(func(e *LogEntry) bool { return e.IP == "1.0.1.5" }, 10)
	if len(logs) == 0 || logs[len(logs)-1].CC != "CN" || !strings.Contains(logs[len(logs)-1].Node, "拦截") {
		t.Fatalf("blocked request should be logged with country: %+v", logs)
	}
}

func TestAutoBanLoginBruteForce(t *testing.T) {
	m, entry := accessSetup(t, &AccessConfig{Mode: "off", AutoBan: true, BanHits: 3, BanMinutes: 5}, Site{Mode: "path", Path: "hk"})
	var sent []string
	m.lines.notify = func(s string) { sent = append(sent, s) }
	// 假上游对登录接口返回 404，这里换成一个总是 401 的上游
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/AuthenticateByName") {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte("{}"))
	}))
	defer up.Close()
	m.mu.Lock()
	m.cfg.Sites[0].Upstream = up.URL
	m.reload()
	m.mu.Unlock()
	login := func(ip string) int {
		req, _ := http.NewRequest("POST", entry.URL+"/hk/emby/Users/AuthenticateByName", strings.NewReader(`{"Username":"a","Pw":"b"}`))
		req.Header.Set("X-Real-Ip", ip)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 3; i++ {
		if c := login("9.9.9.9"); c != 401 {
			t.Fatalf("attempt %d: %d", i, c)
		}
	}
	if c := login("9.9.9.9"); c != 403 {
		t.Fatalf("should be banned after 3 failures, got %d", c)
	}
	if c := login("9.9.9.10"); c != 401 {
		t.Fatal("other IPs not affected")
	}
	if len(sent) != 1 || !strings.Contains(sent[0], "9.9.9.9") {
		t.Fatalf("ban notification: %v", sent)
	}
	if bl := m.bans.list(time.Now()); len(bl) != 1 {
		t.Fatalf("ban list: %+v", bl)
	}
	m.bans.unban("9.9.9.9")
	if c := login("9.9.9.9"); c != 401 {
		t.Fatal("unban should restore access")
	}
}

func TestAccessKeyGate(t *testing.T) {
	m, entry := accessSetup(t, nil, Site{Mode: "path", Path: "hk", AccessKey: "s3cret"})
	if resp, _ := get(t, entry.URL+"/hk/emby/System/Info/Public"); resp.StatusCode != 404 {
		t.Fatalf("missing key should 404, got %d", resp.StatusCode)
	}
	if resp, _ := get(t, entry.URL+"/hk/s3cretx/emby/System/Info/Public"); resp.StatusCode != 404 {
		t.Fatal("key prefix must match a whole segment")
	}
	resp, body := get(t, entry.URL+"/hk/s3cret/emby/Items/1/PlaybackInfo")
	if resp.StatusCode != 200 || !strings.Contains(body, entry.URL+"/hk/s3cret/emby/videos/1/stream") {
		t.Fatalf("with key: %d %s", resp.StatusCode, body)
	}
	// 健康探测不需要密钥
	if resp, _ := get(t, entry.URL+"/hk"+peerHealthPath); resp.StatusCode != 200 {
		t.Fatal("health path must bypass the key")
	}
	if c := m.catches.all(); len(c) != 2 {
		t.Fatalf("catch log: %+v", c)
	}
	if cb := clientBase(&Site{PublicURL: "https://e.com:8001/", AccessKey: "k1k1"}); cb != "https://e.com:8001/k1k1" {
		t.Fatal(cb)
	}
}
