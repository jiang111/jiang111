package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEmby 模拟一台做了推流分离的 Emby：/stream 302 到另一台推流服务器。
func fakeEmby(t *testing.T, streamURL *string) *httptest.Server {
	var self *httptest.Server
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/System/Info/Public"):
			json.NewEncoder(w).Encode(map[string]any{
				"Id": "abc", "ServerName": "Fake", "Version": "4.8.0.0",
				"LocalAddress": "http://192.168.1.2:8096", "WanAddress": self.URL,
			})
		case r.URL.Path == "/emby/Items/1/PlaybackInfo":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"MediaSources":[{"DirectStreamUrl":"%s/emby/videos/1/stream?api_key=x","Path":"%s/v/1.mkv"}],"Host":%q,"XFF":%q,"OriginOK":%v}`,
				self.URL, *streamURL, r.Host, r.Header.Get("X-Forwarded-For"), r.Header.Get("Origin") == self.URL)
		case r.URL.Path == "/blocked":
			http.Redirect(w, r, "https://errors.example.net/", http.StatusFound)
		case r.URL.Path == "/emby/videos/1/stream", r.URL.Path == "/emby/videos/1/redir":
			// 模拟 NoxEmby 等推流分离：播放请求 307 到推流节点
			http.Redirect(w, r, *streamURL+"/v/1.mkv?sign=1", http.StatusTemporaryRedirect)
		case r.URL.Path == "/emby/web/":
			http.Redirect(w, r, "/emby/web/index.html", http.StatusFound)
		case r.URL.Path == "/embywebsocket":
			c, brw, _ := http.NewResponseController(w).Hijack()
			defer c.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			brw.Flush()
			line, _ := brw.ReadString('\n')
			brw.WriteString("echo:" + line)
			brw.Flush()
		default:
			http.NotFound(w, r)
		}
	}))
	return self
}

func fakeStream(t *testing.T) *httptest.Server {
	var self *httptest.Server
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".m3u8") {
			w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
			fmt.Fprintf(w, "#EXTM3U\n%s/seg/0.ts\nseg/1.ts\n", self.URL)
			return
		}
		w.Header().Set("Content-Type", "video/x-matroska")
		http.ServeContent(w, r, "1.mkv", time.Time{}, bytes.NewReader(bytes.Repeat([]byte("0123456789"), 1000)))
	}))
	return self
}

func setup(t *testing.T, site Site) (*Manager, *httptest.Server, *httptest.Server, *httptest.Server) {
	streamURL := ""
	st := fakeStream(t)
	streamURL = st.URL
	up := fakeEmby(t, &streamURL)
	site.Upstream = up.URL
	site.ID, site.Enabled = "s1", true
	if site.Name == "" {
		site.Name = "test"
	}
	cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{&site}}
	if err := validateSite(cfg, &site); err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(t.TempDir(), "config.json"), cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()
	entry := httptest.NewServer(m)
	t.Cleanup(func() { entry.Close(); up.Close(); st.Close() })
	return m, entry, up, st
}

var noRedirect = &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

func get(t *testing.T, url string, hdr ...string) (*http.Response, string) {
	req, _ := http.NewRequest("GET", url, nil)
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

func TestPathModeFlow(t *testing.T) {
	_, entry, up, st := setup(t, Site{Mode: "path", Path: "hk", StreamMode: "proxy"})
	base := entry.URL + "/hk"

	// System/Info/Public：内外网地址改成反代地址
	_, body := get(t, base+"/emby/System/Info/Public")
	var info map[string]any
	json.Unmarshal([]byte(body), &info)
	if info["LocalAddress"] != base || info["WanAddress"] != base {
		t.Fatalf("system info not rewritten: %s", body)
	}

	// PlaybackInfo：上游地址和推流地址都改写，Host/Origin 改成上游，默认不传 XFF
	_, body = get(t, base+"/emby/Items/1/PlaybackInfo", "Origin", "http://evil", "X-Forwarded-For", "9.9.9.9")
	if strings.Contains(body, up.URL) || strings.Contains(body, st.URL) {
		t.Fatalf("upstream url leaked: %s", body)
	}
	if !strings.Contains(body, base+"/emby/videos/1/stream") {
		t.Fatalf("direct stream url not rewritten: %s", body)
	}
	stHost := strings.TrimPrefix(st.URL, "http://")
	h, p, _ := net.SplitHostPort(stHost)
	epURL := base + "/__ep/http/" + h + "/" + p + "/v/1.mkv"
	if !strings.Contains(body, epURL) {
		t.Fatalf("stream host not rewritten: %s", body)
	}
	if !strings.Contains(body, `"Host":"`+strings.TrimPrefix(up.URL, "http://")+`"`) || !strings.Contains(body, `"XFF":""`) || !strings.Contains(body, `"OriginOK":true`) {
		t.Fatalf("headers wrong: %s", body)
	}

	// 播放请求带 Range 被 307 到推流节点：服务端直接跟随，客户端拿到 206
	resp, body := get(t, base+"/emby/videos/1/stream?api_key=x&static=true", "Range", "bytes=0-9")
	if resp.StatusCode != 206 || body != "0123456789" || resp.Header.Get("Content-Range") != "bytes 0-9/10000" {
		t.Fatalf("follow 307 failed: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	// 不带 Range 的直链播放请求同样跟随
	resp, body = get(t, base+"/emby/videos/1/stream?api_key=x")
	if resp.StatusCode != 200 || len(body) != 10000 {
		t.Fatalf("follow 307 (no range) failed: %d %d", resp.StatusCode, len(body))
	}

	// 非播放请求的跳转 → Location 改写到 /__ep/
	resp, _ = get(t, base+"/emby/videos/1/redir")
	if loc := resp.Header.Get("Location"); loc != epURL+"?sign=1" {
		t.Fatalf("location = %q, want %q", loc, epURL+"?sign=1")
	}
	// 相对 Location 加前缀
	resp, _ = get(t, base+"/emby/web/")
	if loc := resp.Header.Get("Location"); loc != "/hk/emby/web/index.html" {
		t.Fatalf("relative location = %q", loc)
	}

	// 经 /__ep/ 拉流，支持 Range
	resp, body = get(t, epURL+"?sign=1", "Range", "bytes=10-19")
	_ = body
	if resp.StatusCode != 206 || body != "0123456789" {
		t.Fatalf("range via ep failed: %d %q", resp.StatusCode, body)
	}

	// m3u8 里的绝对地址改写
	_, body = get(t, base+"/__ep/http/"+h+"/"+p+"/v/master.m3u8")
	if !strings.Contains(body, base+"/__ep/http/"+h+"/"+p+"/seg/0.ts") || !strings.Contains(body, "\nseg/1.ts") {
		t.Fatalf("m3u8 not rewritten: %s", body)
	}

	// 非播放请求跳到无关域名（如被拦截跳错误页）：保持原样，不学习
	resp, _ = get(t, base+"/blocked")
	if loc := resp.Header.Get("Location"); loc != "https://errors.example.net/" {
		t.Fatalf("blocked redirect rewritten: %q", loc)
	}

	// 未学习过的域名不允许经 /__ep/ 代理
	resp, _ = get(t, base+"/__ep/https/example.com/443/")
	if resp.StatusCode != 403 {
		t.Fatalf("open proxy! status %d", resp.StatusCode)
	}

	// 未匹配路径
	resp, _ = get(t, entry.URL+"/nope/emby/System/Info/Public")
	if resp.StatusCode != 404 {
		t.Fatalf("unmatched status %d", resp.StatusCode)
	}
}

func TestDirectStreamMode(t *testing.T) {
	_, entry, _, st := setup(t, Site{Mode: "path", Path: "jp", StreamMode: "direct"})
	resp, _ := get(t, entry.URL+"/jp/emby/videos/1/stream", "Range", "bytes=0-")
	if loc := resp.Header.Get("Location"); loc != st.URL+"/v/1.mkv?sign=1" {
		t.Fatalf("direct mode should keep location, got %q", loc)
	}
}

func TestForwardedHeadersAndForwardIP(t *testing.T) {
	_, entry, _, _ := setup(t, Site{Mode: "host", Host: "hk.example.com", StreamMode: "proxy", ForwardIP: true})
	_, body := get(t, entry.URL+"/emby/Items/1/PlaybackInfo",
		"X-Forwarded-Host", "hk.example.com", "X-Forwarded-Proto", "https", "X-Real-IP", "1.2.3.4")
	if !strings.Contains(body, "https://hk.example.com/emby/videos/1/stream") {
		t.Fatalf("public base from forwarded headers wrong: %s", body)
	}
	if !strings.Contains(body, `"XFF":"1.2.3.4"`) {
		t.Fatalf("forward ip failed: %s", body)
	}
}

func TestGzipRewrite(t *testing.T) {
	_, entry, up, _ := setup(t, Site{Mode: "path", Path: "gz", StreamMode: "proxy"})
	resp, err := http.Get(entry.URL + "/gz/emby/Items/1/PlaybackInfo") // Go 自动带 gzip 并解压
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if strings.Contains(string(b), up.URL) || !strings.Contains(string(b), "MediaSources") {
		t.Fatalf("gzip body wrong: %s", b)
	}
}

func TestWebSocket(t *testing.T) {
	_, entry, _, _ := setup(t, Site{Mode: "path", Path: "ws", StreamMode: "proxy"})
	c, err := net.Dial("tcp", strings.TrimPrefix(entry.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET /ws/embywebsocket?api_key=x HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade failed: %v %v", err, resp)
	}
	fmt.Fprintf(c, "hello\n")
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, _ := br.ReadString('\n')
	if line != "echo:hello\n" {
		t.Fatalf("ws echo = %q", line)
	}
}

func TestPortMode(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	m, _, _, _ := setup(t, Site{Mode: "port", Port: port, StreamMode: "proxy"})
	t.Cleanup(func() {
		m.mu.Lock()
		m.cfg.Sites = nil
		m.reload()
		m.mu.Unlock()
	})
	_, body := get(t, fmt.Sprintf("http://127.0.0.1:%d/emby/System/Info/Public", port))
	if !strings.Contains(body, fmt.Sprintf(`"LocalAddress":"http://127.0.0.1:%d"`, port)) {
		t.Fatalf("port mode: %s", body)
	}
}

func TestMaskURI(t *testing.T) {
	got := maskURI("/emby/Items?api_key=secret&X-Emby-Token=t&Fields=a")
	if strings.Contains(got, "secret") || strings.Contains(got, "=t&") {
		t.Fatalf("not masked: %s", got)
	}
}

// 模拟 023168 这类服务器：上游根据 MagpieCDN 头生成推流地址 {MagpieCDN}/magpiecdn?load=节点，
// 反代需要把 /magpiecdn 转发到 https://{load}.域名。
func TestMagpieStyleRoute(t *testing.T) {
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/magpiecdn" || r.URL.Query().Get("id") != "1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "a.mp4", time.Time{}, bytes.NewReader(bytes.Repeat([]byte("ab"), 500)))
	}))
	defer node.Close()
	_, nodePort, _ := net.SplitHostPort(strings.TrimPrefix(node.URL, "http://"))

	var gotHeader string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("MagpieCDN")
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"DirectStreamUrl":"%s/magpiecdn?load=%s&id=1"}`, gotHeader, nodePort)
	}))
	defer up.Close()

	site := &Site{ID: "m", Name: "magpie", Enabled: true, Upstream: up.URL, Mode: "path", Path: "mp", StreamMode: "proxy",
		Headers: map[string]string{"MagpieCDN": "{public_base}"},
		Routes:  []Route{{Path: "/magpiecdn", Target: "http://127.0.0.1:{arg.load}"}}}
	cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{site}}
	if err := validateSite(cfg, site); err != nil {
		t.Fatal(err)
	}
	m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()
	entry := httptest.NewServer(m)
	defer entry.Close()

	_, body := get(t, entry.URL+"/mp/emby/Items/1/PlaybackInfo", "X-Forwarded-Proto", "https", "X-Forwarded-Host", "p.example.com")
	if gotHeader != "https://p.example.com/mp" || !strings.Contains(body, "https://p.example.com/mp/magpiecdn?load=") {
		t.Fatalf("MagpieCDN header %q body %s", gotHeader, body)
	}
	resp, b := get(t, entry.URL+"/mp/magpiecdn?load="+nodePort+"&id=1", "Range", "bytes=0-3")
	if resp.StatusCode != 206 || b != "abab" {
		t.Fatalf("route failed: %d %q", resp.StatusCode, b)
	}
	resp, _ = get(t, entry.URL+"/mp/magpiecdn?load=evil.com&id=1")
	if resp.StatusCode != 400 {
		t.Fatalf("route arg injection not blocked: %d", resp.StatusCode)
	}
}

func TestAddScheme(t *testing.T) {
	for in, want := range map[string]string{
		"lite.example.uk": "https://lite.example.uk",
		"a.com:443":       "https://a.com:443",
		"a.com:8096":      "http://a.com:8096",
		"1.2.3.4:8096":    "http://1.2.3.4:8096",
		"1.2.3.4":         "http://1.2.3.4",
		"http://a.com":    "http://a.com",
		"a.com/emby":      "https://a.com/emby",
	} {
		if got := addScheme(in); got != want {
			t.Errorf("addScheme(%q) = %q, want %q", in, got, want)
		}
	}
}
