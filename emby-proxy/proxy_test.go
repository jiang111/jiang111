package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	t.Cleanup(func() { entry.Close(); up.Close(); st.Close(); m.bg.Wait() })
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
	m, entry, up, st := setup(t, Site{Mode: "path", Path: "hk", StreamMode: "proxy"})
	rt := m.sites["s1"]
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
	epBase := base + rt.epPath("http", h, p)
	epURL := epBase + "/v/1.mkv"
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
	_, body = get(t, epBase+"/v/master.m3u8")
	if !strings.Contains(body, epBase+"/seg/0.ts") || !strings.Contains(body, "\nseg/1.ts") {
		t.Fatalf("m3u8 not rewritten: %s", body)
	}

	// 非播放请求跳到无关域名（如被拦截跳错误页）：保持原样，不学习
	resp, _ = get(t, base+"/blocked")
	if loc := resp.Header.Get("Location"); loc != "https://errors.example.net/" {
		t.Fatalf("blocked redirect rewritten: %q", loc)
	}

	// 未学习过的域名不允许经 /__ep/ 代理（无签名 / 伪造签名都不行）
	for _, bad := range []string{"/__ep/https/example.com/443/", "/__ep/AAAAAAAAAAAAAAAA/https/example.com/443/",
		strings.Replace(rt.epPath("http", h, p), "/http/", "/https/", 1) + "/"} {
		if resp, _ = get(t, base+bad); resp.StatusCode != 403 {
			t.Fatalf("open proxy via %s! status %d", bad, resp.StatusCode)
		}
	}
	// 旧格式（无签名）对已学习的域名仍然可用，兼容升级前客户端缓存的地址
	if resp, body = get(t, base+"/__ep/http/"+h+"/"+p+"/v/1.mkv", "Range", "bytes=0-3"); resp.StatusCode != 206 || body != "0123" {
		t.Fatalf("legacy ep: %d %q", resp.StatusCode, body)
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
	checkWS(t, entry.URL, "/ws")
}

func checkWS(t *testing.T, entryURL, prefix string) {
	c, err := net.Dial("tcp", strings.TrimPrefix(entryURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET "+prefix+"/embywebsocket?api_key=x HTTP/1.1\r\nHost: x\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
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

// 推流请求被上游 302 到本站自己的地址（{MagpieCDN}/magpiecdn?load=节点）时，服务端跟随要在本机解析，
// 不能经公网绕回自己：这里的 p.example.com 解析不到，绕出去就会失败；访问者里也只能有客户端一个 IP。
func TestFollowSelfRedirect(t *testing.T) {
	var nodeHits atomic.Int32
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nodeHits.Add(1)
		if r.URL.Path != "/magpiecdn" || r.URL.Query().Get("id") != "1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "video/mp4")
		http.ServeContent(w, r, "a.mp4", time.Time{}, bytes.NewReader(bytes.Repeat([]byte("ab"), 500)))
	}))
	defer node.Close()
	_, nodePort, _ := net.SplitHostPort(strings.TrimPrefix(node.URL, "http://"))

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/Videos/1/stream":
			http.Redirect(w, r, r.Header.Get("MagpieCDN")+"/magpiecdn?load="+nodePort+"&id=1", http.StatusFound)
		case "/Videos/2/stream": // 跳回本站的普通路径：直接访问上游
			http.Redirect(w, r, r.Header.Get("MagpieCDN")+"/Videos/2/original?x=1", http.StatusFound)
		case "/Videos/3/stream": // 参数不合法，解析不了：把跳转交给客户端
			http.Redirect(w, r, r.Header.Get("MagpieCDN")+"/magpiecdn?load=evil.com&id=1", http.StatusFound)
		case "/Videos/2/original":
			w.Header().Set("Content-Type", "video/mp4")
			http.ServeContent(w, r, "b.mp4", time.Time{}, bytes.NewReader(bytes.Repeat([]byte("cd"), 500)))
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()

	site := &Site{ID: "m", Name: "magpie", Enabled: true, Upstream: up.URL, Mode: "path", Path: "mp", StreamMode: "proxy",
		PublicURL: "https://p.example.com/mp",
		Headers:   map[string]string{"MagpieCDN": "{public_base}"},
		Routes:    []Route{{Path: "/magpiecdn", Target: "http://127.0.0.1:{arg.load}"}}}
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
	defer m.bg.Wait()

	resp, b := get(t, entry.URL+"/mp/Videos/1/stream", "Range", "bytes=0-3")
	if resp.StatusCode != 206 || b != "abab" || nodeHits.Load() != 1 {
		t.Fatalf("route via self redirect: %d %q hits=%d", resp.StatusCode, b, nodeHits.Load())
	}
	resp, b = get(t, entry.URL+"/mp/Videos/2/stream", "Range", "bytes=0-3")
	if resp.StatusCode != 206 || b != "cdcd" {
		t.Fatalf("upstream via self redirect: %d %q", resp.StatusCode, b)
	}
	resp, _ = get(t, entry.URL+"/mp/Videos/3/stream", "Range", "bytes=0-3")
	if resp.StatusCode != 302 || resp.Header.Get("Location") != "https://p.example.com/mp/magpiecdn?load=evil.com&id=1" {
		t.Fatalf("unresolvable self redirect: %d %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	rt := m.sites["m"]
	rt.hostsMu.Lock()
	learned := len(rt.hosts)
	rt.hostsMu.Unlock()
	if learned != 0 {
		t.Fatalf("self address learned as stream host: %v", rt.hosts)
	}
	if v := rt.stats.view(); len(v.IPs) != 1 || v.Requests != 3 {
		t.Fatalf("ips %+v requests %d", v.IPs, v.Requests)
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

func TestNginxConfig(t *testing.T) {
	cfg := &Config{AdminListen: ":55660", PortBind: "127.0.0.1", NginxCert: "/c.pem", NginxKey: "/k.pem", Sites: []*Site{
		{Name: "a", Mode: "port", Port: 18001, PublicURL: "https://e.example.com:8001", Upstream: "https://u", Enabled: true},
		{Name: "b", Mode: "port", Port: 18002, Upstream: "https://u"},
		{Name: "c", Mode: "port", Port: 8003, PublicURL: "https://e.example.com:8003", Upstream: "https://u"},
		{Name: "d", Mode: "path", Path: "x", Upstream: "https://u"},
	}}
	out := nginxConfig(cfg, "", "", nil)
	for _, want := range []string{"listen 8001 ssl;", "server_name e.example.com;", "proxy_pass http://127.0.0.1:18001;", "ssl_certificate     /c.pem;"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "\nserver {") != 1 {
		t.Fatalf("expected exactly one server block:\n%s", out)
	}
	cfg.Cluster.DirectHost = "n1.example.com"
	if out = nginxConfig(cfg, "", "", nil); !strings.Contains(out, "server_name e.example.com n1.example.com;") {
		t.Fatalf("direct host missing in server_name:\n%s", out)
	}
}

func TestCleanHost(t *testing.T) {
	for in, want := range map[string]string{
		"Tokyo-1.Example.com": "tokyo-1.example.com", " a.b.c ": "a.b.c",
		"": "", "a": "", "1.2.3.4": "", "a.example.com:443": "", "https://a.example.com": "", "a_b.example.com": "", "a..example.com": "",
	} {
		if got := cleanHost(in); got != want {
			t.Errorf("cleanHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func genCert(t *testing.T, host string, notAfter time.Time) (certPEM, keyPEM []byte) {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: host}, DNSNames: []string{host},
		NotBefore: time.Now().Add(-48 * time.Hour), NotAfter: notAfter}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	kb, _ := x509.MarshalPKCS8PrivateKey(key)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kb})
}

func TestCertUpload(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOST_DATA_DIR", "/opt/emby-proxy/data")
	cfg := &Config{AdminPassword: "pw123456", Secret: "s", AdminListen: ":0", Sites: []*Site{
		{Name: "a", Mode: "port", Port: 18001, PublicURL: "https://other.example.com:8001", Upstream: "https://u", Enabled: true}}}
	m := NewManager(filepath.Join(dir, "config.json"), cfg)
	a := &Admin{m: m, cluster: newCluster(m), failures: map[string][]time.Time{}}
	srv := httptest.NewServer(a.Handler())
	defer srv.Close()
	cookie := adminLogin(t, srv.URL, "pw123456")

	allowExpired := false
	upload := func(cert, key []byte) (int, string) {
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		if allowExpired {
			mw.WriteField("allow_expired", "1")
		}
		fw, _ := mw.CreateFormFile("cert", "fullchain.pem")
		fw.Write(cert)
		fw, _ = mw.CreateFormFile("key", "privkey.pem")
		fw.Write(key)
		mw.Close()
		req, _ := http.NewRequest("POST", srv.URL+"/api/cert", &buf)
		req.Header.Set("Content-Type", mw.FormDataContentType())
		req.Header.Set(csrfHeader, "1")
		req.AddCookie(cookie)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, string(b)
	}

	c1, k1 := genCert(t, "emby.example.com", time.Now().Add(60*24*time.Hour))
	_, k2 := genCert(t, "emby.example.com", time.Now().Add(60*24*time.Hour))
	cOld, kOld := genCert(t, "emby.example.com", time.Now().Add(-time.Hour))

	if code, body := upload(c1, k2); code != 400 || !strings.Contains(body, "不匹配") {
		t.Fatalf("mismatch: %d %s", code, body)
	}
	if code, body := upload(k1, c1); code != 400 {
		t.Fatalf("swapped files: %d %s", code, body)
	}
	if code, body := upload(cOld, kOld); code != 400 || !strings.Contains(body, "过期") {
		t.Fatalf("expired: %d %s", code, body)
	}
	allowExpired = true
	if code, body := upload(cOld, kOld); code != 200 {
		t.Fatalf("expired with allow_expired: %d %s", code, body)
	}
	allowExpired = false
	if code, body := upload(c1, k1); code != 200 || !strings.Contains(body, "emby.example.com") {
		t.Fatalf("upload: %d %s", code, body)
	}
	if fi, err := os.Stat(filepath.Join(dir, "ssl", "privkey.pem")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", fi, err)
	}
	if m.cfg.NginxCert != "/opt/emby-proxy/data/ssl/fullchain.pem" {
		t.Fatalf("nginx cert path = %q", m.cfg.NginxCert)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/nginx", nil)
	req.AddCookie(cookie)
	resp, _ := http.DefaultClient.Do(req)
	b, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(b), "ssl_certificate     /opt/emby-proxy/data/ssl/fullchain.pem;") || !strings.Contains(string(b), "证书不包含域名 other.example.com") {
		t.Fatalf("nginx conf:\n%s", b)
	}
}

func adminLogin(t *testing.T, base, pw string) *http.Cookie {
	req, _ := http.NewRequest("POST", base+"/api/login", strings.NewReader(`{"password":"`+pw+`"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || len(resp.Cookies()) == 0 {
		t.Fatalf("login failed: %v %v", err, resp)
	}
	return resp.Cookies()[0]
}

func newAdmin(t *testing.T, cfg *Config) (*Manager, *httptest.Server, *http.Cookie) {
	if cfg.AdminPassword == "" {
		cfg.AdminPassword = "pw123456"
	}
	cfg.Secret, cfg.AdminListen, cfg.Listen = "s", ":1", ":2"
	if cfg.ClusterSecret == "" {
		cfg.ClusterSecret = randHex(16)
	}
	m := NewManager(filepath.Join(t.TempDir(), "config.json"), cfg)
	a := &Admin{m: m, cluster: newCluster(m), failures: map[string][]time.Time{}}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return m, srv, adminLogin(t, srv.URL, cfg.AdminPassword)
}

func adminDo(t *testing.T, srv *httptest.Server, c *http.Cookie, method, path, body string, hdr ...string) (int, string) {
	req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	if c != nil {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp.StatusCode, string(b)
}

func TestAdminCSRF(t *testing.T) {
	_, srv, c := newAdmin(t, &Config{})
	site := `{"name":"x","upstream":"https://a.example.com","mode":"path","path":"x"}`
	// 跨站页面能发的「简单请求」：text/plain、无自定义头
	if code, _ := adminDo(t, srv, c, "POST", "/api/sites", site, "Content-Type", "text/plain"); code != 403 {
		t.Fatalf("csrf without header: %d", code)
	}
	// 带了自定义头但 Origin 是别的端口
	if code, _ := adminDo(t, srv, c, "POST", "/api/sites", site, csrfHeader, "1", "Origin", "http://"+strings.TrimPrefix(srv.URL, "http://")[:9]+":8001"); code != 403 {
		t.Fatalf("csrf cross origin: %d", code)
	}
	if code, b := adminDo(t, srv, c, "POST", "/api/sites", site, csrfHeader, "1", "Origin", srv.URL); code != 200 {
		t.Fatalf("same origin should work: %d %s", code, b)
	}
}

func TestSettingsAtomic(t *testing.T) {
	m, srv, c := newAdmin(t, &Config{})
	code, _ := adminDo(t, srv, c, "PUT", "/api/settings", `{"old_password":"pw123456","new_password":"newpass123","port_bind":"abc","entry_url":"https://x"}`, csrfHeader, "1")
	if code != 400 || m.cfg.AdminPassword != "pw123456" || m.cfg.EntryURL != "" {
		t.Fatalf("partial apply: code=%d pw=%q entry=%q", code, m.cfg.AdminPassword, m.cfg.EntryURL)
	}
}

func TestLoginRateLimit(t *testing.T) {
	_, srv, _ := newAdmin(t, &Config{})
	var wg sync.WaitGroup
	var ok429 atomic.Int32
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, _ := adminDo(t, srv, nil, "POST", "/api/login", `{"password":"wrong"}`, csrfHeader, "1"); code == 429 {
				ok429.Add(1)
			}
		}()
	}
	wg.Wait()
	if ok429.Load() < 7 { // 并发 12 次，最多 5 次进入校验
		t.Fatalf("rate limit bypassed by concurrency: only %d got 429", ok429.Load())
	}
}

func TestMergeHosts(t *testing.T) {
	got := mergeHosts([]string{"https://a:443"}, []string{"https://a:443", "https://b:443"}, []string{"https://a:443", "https://b:443", "https://c:443"})
	// b 被用户删掉，c 是之后学到的要保留
	if strings.Join(got, ",") != "https://a:443,https://c:443" {
		t.Fatalf("merge = %v", got)
	}
}

func TestParentDomain(t *testing.T) {
	for in, want := range map[string]string{
		"emby.foo.com.cn": "foo.com.cn", "evil.bar.com.cn": "bar.com.cn", "a.b.co.uk": "b.co.uk",
		"node1.example.com": "example.com", "example.com": "example.com", "1.2.3.4": "1.2.3.4", "x.y.com.tw": "y.com.tw",
	} {
		if got := parentDomain(in); got != want {
			t.Errorf("parentDomain(%q)=%q want %q", in, got, want)
		}
	}
}

func TestStripPanelCookie(t *testing.T) {
	h := http.Header{}
	h.Add("Cookie", cookieName+"=secret; other=1")
	stripPanelCookie(h)
	if h.Get("Cookie") != "other=1" {
		t.Fatalf("cookie = %q", h.Get("Cookie"))
	}
	h = http.Header{"Cookie": {cookieName + "=secret"}}
	stripPanelCookie(h)
	if _, ok := h["Cookie"]; ok {
		t.Fatalf("cookie header should be removed: %v", h)
	}
}

// 上游返回 gzip：需要改写的 JSON 能解压改写；不需要改写的 JS 保持压缩原样透传；面板 Cookie 不转发；
// 路由节点返回的相对跳转仍指向该节点。
func TestGzipPassthroughAndRoutes(t *testing.T) {
	var gotCookie string
	var self *httptest.Server
	gz := func(s string) []byte {
		var b bytes.Buffer
		zw := gzip.NewWriter(&b)
		zw.Write([]byte(s))
		zw.Close()
		return b.Bytes()
	}
	node := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cdn" {
			http.Redirect(w, r, "/stream/abc", http.StatusFound)
			return
		}
		w.Write([]byte("node:" + r.URL.Path))
	}))
	defer node.Close()
	_, nodePort, _ := net.SplitHostPort(strings.TrimPrefix(node.URL, "http://"))
	self = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		ae := r.Header.Get("Accept-Encoding")
		switch r.URL.Path {
		case "/emby/Items":
			w.Header().Set("Content-Type", "application/json")
			body := strings.Repeat(`{"Url":"`+self.URL+`/emby/x"},`, 100)
			if ae == "gzip" {
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(gz("[" + body + "{}]"))
			} else {
				w.Write([]byte("[" + body + "{}]"))
			}
		case "/web/app.js":
			w.Header().Set("Content-Type", "application/javascript")
			if ae == "gzip" {
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(gz(strings.Repeat("console.log(1);", 500)))
			} else {
				w.Write([]byte(strings.Repeat("console.log(1);", 500)))
			}
		}
	}))
	defer self.Close()
	site := &Site{ID: "g", Name: "g", Enabled: true, Upstream: self.URL, Mode: "path", Path: "g", StreamMode: "proxy",
		Routes: []Route{{Path: "/cdn", Target: "http://127.0.0.1:{arg.p}"}}}
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
	rtG := m.sites["g"]
	tr := &http.Transport{DisableCompression: true}
	cl := &http.Client{Transport: tr, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	do := func(path string, gzipOK bool) (*http.Response, []byte) {
		req, _ := http.NewRequest("GET", entry.URL+path, nil)
		if gzipOK {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		req.Header.Set("Cookie", cookieName+"=secret; emby=1")
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp, b
	}

	resp, b := do("/g/emby/Items", true)
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if resp.Header.Get("Content-Encoding") != "gzip" || err != nil {
		t.Fatalf("json not gzipped: %v %v", resp.Header, err)
	}
	plain, _ := io.ReadAll(zr)
	if strings.Contains(string(plain), self.URL) || !strings.Contains(string(plain), entry.URL+"/g/emby/x") {
		t.Fatalf("gzip json not rewritten: %.200s", plain)
	}
	if gotCookie != "emby=1" {
		t.Fatalf("panel cookie leaked upstream: %q", gotCookie)
	}
	resp, b = do("/g/emby/Items", false)
	if resp.Header.Get("Content-Encoding") != "" || !strings.Contains(string(b), entry.URL+"/g/emby/x") {
		t.Fatalf("identity json: %v %.100s", resp.Header, b)
	}
	resp, b = do("/g/web/app.js", true)
	if resp.Header.Get("Content-Encoding") != "gzip" || len(b) > 1000 {
		t.Fatalf("js should stay compressed: enc=%q len=%d", resp.Header.Get("Content-Encoding"), len(b))
	}
	resp, _ = do("/g/cdn?p="+nodePort, false)
	want := "/g" + rtG.epPath("http", "127.0.0.1", nodePort) + "/stream/abc"
	if loc := resp.Header.Get("Location"); loc != want {
		t.Fatalf("route relative redirect = %q want %q", loc, want)
	}
	resp, b = do(want, false)
	if string(b) != "node:/stream/abc" {
		t.Fatalf("follow route redirect: %d %s", resp.StatusCode, b)
	}
}

// 用测试 CA 签一张给 127.0.0.1 / 指定域名的证书
func tlsServerWithCert(t *testing.T, notAfter time.Time, withIP bool, names ...string) (*httptest.Server, *x509.CertPool) {
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(10), Subject: pkix.Name{CommonName: "test ca"}, IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-1000 * time.Hour), NotAfter: time.Now().Add(1000 * time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tpl := &x509.Certificate{SerialNumber: big.NewInt(11), Subject: pkix.Name{CommonName: names[0]}, DNSNames: names,
		NotBefore: time.Now().Add(-500 * time.Hour), NotAfter: notAfter,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if withIP {
		tpl.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	}
	der, _ := x509.CreateCertificate(rand.Reader, tpl, ca, &key.PublicKey, caKey)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embywebsocket" {
			c, brw, _ := http.NewResponseController(w).Hijack()
			defer c.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			brw.Flush()
			line, _ := brw.ReadString('\n')
			brw.WriteString("echo:" + line)
			brw.Flush()
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"Id": "x", "ServerName": "TLS", "Version": "4.8"})
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	srv.StartTLS()
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	return srv, pool
}

func TestIgnoreExpiredUpstream(t *testing.T) {
	expired, pool := tlsServerWithCert(t, time.Now().Add(-time.Hour), true, "emby.test")
	testRoots = pool
	defer func() { testRoots = nil }()
	get := func(mode int, url string) error {
		tr := newTransport(mode)
		tr.Proxy = nil
		if mode == tlsVerify {
			tr.TLSClientConfig.RootCAs = pool
		}
		resp, err := (&http.Client{Transport: tr}).Get(url)
		if err == nil {
			resp.Body.Close()
		}
		return err
	}
	if err := get(tlsVerify, expired.URL); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("normal verify should fail on expired cert: %v", err)
	}
	if err := get(tlsIgnoreExpired, expired.URL); err != nil {
		t.Fatalf("ignore expired should connect: %v", err)
	}
	// 只忽略过期：域名不对仍然拒绝
	other, pool2 := tlsServerWithCert(t, time.Now().Add(-time.Hour), false, "emby.test") // 证书只含 emby.test，不含 127.0.0.1
	testRoots = pool2
	if err := get(tlsIgnoreExpired, other.URL); err == nil || !strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("ignore expired must still verify hostname: %v", err)
	}
	// 签发机构不受信任仍然拒绝
	testRoots = x509.NewCertPool()
	if err := get(tlsIgnoreExpired, expired.URL); err == nil {
		t.Fatalf("ignore expired must still verify issuer")
	}
}

func TestExportImport(t *testing.T) {
	src, srvA, cA := newAdmin(t, &Config{EntryURL: "https://a.example.com", PortBind: "127.0.0.1"})
	for _, b := range []string{
		`{"name":"一","upstream":"https://one.example.com","mode":"port","port":18001,"public_url":"https://a.example.com:8001","stream_hosts":["https://cdn.one.example.com:443"]}`,
		`{"name":"二","upstream":"two.example.com","mode":"path","path":"two","headers":{"MagpieCDN":"{public_base}"},"routes":[{"path":"/magpiecdn","target":"https://{arg.load}.two.example.com"}]}`,
	} {
		if code, out := adminDo(t, srvA, cA, "POST", "/api/sites", b, csrfHeader, "1"); code != 200 {
			t.Fatalf("add: %s", out)
		}
	}
	code, exported := adminDo(t, srvA, cA, "GET", "/api/export", "")
	if code != 200 || strings.Contains(exported, "pw123456") || strings.Contains(exported, `"secret"`) {
		t.Fatalf("export leaked secrets or failed: %d %s", code, exported)
	}

	dst, srvB, cB := newAdmin(t, &Config{})
	if code, out := adminDo(t, srvB, cB, "POST", "/api/sites", `{"name":"旧","upstream":"https://old.example.com","mode":"path","path":"old"}`, csrfHeader, "1"); code != 200 {
		t.Fatal(out)
	}
	// 替换
	code, out := adminDo(t, srvB, cB, "POST", "/api/import", `{"mode":"replace","data":`+exported+`}`, csrfHeader, "1")
	if code != 200 || len(dst.cfg.Sites) != 2 || dst.cfg.EntryURL != "https://a.example.com" || dst.cfg.PortBind != "127.0.0.1" {
		t.Fatalf("replace import: %d %s sites=%d entry=%q", code, out, len(dst.cfg.Sites), dst.cfg.EntryURL)
	}
	if dst.cfg.Sites[1].Routes[0].Target != "https://{arg.load}.two.example.com" || dst.cfg.Sites[0].StreamHosts[0] != "https://cdn.one.example.com:443" {
		t.Fatalf("fields lost: %+v", dst.cfg.Sites)
	}
	// 合并：同 ID 覆盖，不重复
	if code, out := adminDo(t, srvB, cB, "POST", "/api/import", `{"mode":"merge","data":`+exported+`}`, csrfHeader, "1"); code != 200 || len(dst.cfg.Sites) != 2 {
		t.Fatalf("merge same ids: %d %s %d", code, out, len(dst.cfg.Sites))
	}
	// 冲突：新 ID 但端口重复 → 整体拒绝，不改动
	bad := `{"mode":"merge","data":{"sites":[{"id":"new1","name":"冲突","upstream":"https://x.example.com","mode":"port","port":18001},{"id":"new2","name":"好的","upstream":"https://y.example.com","mode":"path","path":"y"}]}}`
	code, out = adminDo(t, srvB, cB, "POST", "/api/import", bad, csrfHeader, "1")
	if code != 400 || !strings.Contains(out, "18001") || len(dst.cfg.Sites) != 2 {
		t.Fatalf("conflict import should be rejected atomically: %d %s %d", code, out, len(dst.cfg.Sites))
	}
	// 服务器上的 config.json 原文件也能导入，且不会改密码
	raw, _ := os.ReadFile(src.path)
	if code, out := adminDo(t, srvB, cB, "POST", "/api/import", `{"mode":"replace","data":`+string(raw)+`}`, csrfHeader, "1"); code != 200 || dst.cfg.AdminPassword != "pw123456" {
		t.Fatalf("config.json import: %d %s", code, out)
	}
	if code, _ := adminDo(t, srvB, cB, "POST", "/api/import", `{"mode":"merge","data":{"foo":1}}`, csrfHeader, "1"); code != 400 {
		t.Fatalf("non-config file should be rejected")
	}
}

func TestIgnoreExpiredThroughProxy(t *testing.T) {
	if err := verifyIgnoringExpiry(tls.ConnectionState{}, ""); err == nil {
		t.Fatal("empty host must be rejected")
	}
	up, pool := tlsServerWithCert(t, time.Now().Add(-time.Hour), true, "emby.test")
	testRoots = pool
	defer func() { testRoots = nil }()
	mk := func(ignore bool) string {
		site := &Site{ID: "e", Name: "e", Enabled: true, Upstream: up.URL, Mode: "path", Path: "e", StreamMode: "proxy", IgnoreExpired: ignore}
		cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{site}}
		if err := validateSite(cfg, site); err != nil {
			t.Fatal(err)
		}
		m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
		for _, tr := range m.trs {
			tr.Proxy = nil
		}
		m.trs[tlsVerify].TLSClientConfig.RootCAs = pool
		m.mu.Lock()
		m.reload()
		m.mu.Unlock()
		entry := httptest.NewServer(m)
		t.Cleanup(entry.Close)
		return entry.URL
	}
	if resp, _ := get(t, mk(false)+"/e/emby/System/Info/Public"); resp.StatusCode != 502 {
		t.Fatalf("expired upstream without option should be 502, got %d", resp.StatusCode)
	}
	base := mk(true)
	if resp, body := get(t, base+"/e/emby/System/Info/Public"); resp.StatusCode != 200 || !strings.Contains(body, `"TLS"`) {
		t.Fatalf("ignore_expired through proxy: %d %s", resp.StatusCode, body)
	}
	checkWS(t, base, "/e")
}

// 多台服务器部署同一个域名：A 机生成的推流地址，在从没学习过该推流域名的 B 机上也能用（共享集群密钥）；
// 密钥不同则拒绝。
func TestSignedEPAcrossNodes(t *testing.T) {
	stream := fakeStream(t)
	defer stream.Close()
	h, p, _ := net.SplitHostPort(strings.TrimPrefix(stream.URL, "http://"))
	node := func(secret string) (*Manager, string) {
		site := &Site{ID: "s1", Name: "n", Enabled: true, Upstream: "https://up.example.com", Mode: "path", Path: "hk", StreamMode: "proxy"}
		cfg := &Config{Listen: ":0", AdminListen: ":0", ClusterSecret: secret, Sites: []*Site{site}}
		if err := validateSite(cfg, site); err != nil {
			t.Fatal(err)
		}
		m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
		m.mu.Lock()
		m.reload()
		m.mu.Unlock()
		e := httptest.NewServer(m)
		t.Cleanup(e.Close)
		return m, e.URL
	}
	mA, _ := node("shared-secret")
	_, urlB := node("shared-secret")
	_, urlC := node("other-secret")
	path := "/hk" + mA.sites["s1"].epPath("http", h, p) + "/v/1.mkv"
	if resp, body := get(t, urlB+path, "Range", "bytes=0-4"); resp.StatusCode != 206 || body != "01234" {
		t.Fatalf("node B should accept A's signed url: %d %q", resp.StatusCode, body)
	}
	if resp, _ := get(t, urlC+path); resp.StatusCode != 403 {
		t.Fatalf("node with different secret must reject: %d", resp.StatusCode)
	}
}

func TestClusterSync(t *testing.T) {
	// 主节点
	master, srvM, cM := newAdmin(t, &Config{EntryURL: "https://emby.example.com", PortBind: "127.0.0.1"})
	mc := newCluster(master)
	srvM.Config.Handler = (&Admin{m: master, cluster: mc, failures: map[string][]time.Time{}}).Handler()
	if code, out := adminDo(t, srvM, cM, "PUT", "/api/cluster", `{"role":"master","panel_url":"https://master.example.com:8443"}`, csrfHeader, "1"); code != 200 {
		t.Fatalf("set master: %s", out)
	}
	token := master.cfg.Cluster.Token
	if len(token) != 32 {
		t.Fatalf("token = %q", token)
	}
	for _, bad := range []string{"https://m.example.com", "m.example.com:8443", "203.0.113.1", "m", "a b.example.com"} {
		if code, _ := adminDo(t, srvM, cM, "PUT", "/api/cluster", `{"role":"master","direct_host":"`+bad+`"}`, csrfHeader, "1"); code != 400 {
			t.Fatalf("direct_host %q should be rejected: %d", bad, code)
		}
	}
	if code, out := adminDo(t, srvM, cM, "PUT", "/api/cluster", `{"role":"master","panel_url":"https://master.example.com:8443","direct_host":"M.Example.com"}`, csrfHeader, "1"); code != 200 || master.cfg.Cluster.DirectHost != "m.example.com" || master.cfg.Cluster.Token != token {
		t.Fatalf("set direct_host: %s %+v", out, master.cfg.Cluster)
	}
	cert, key := genCert(t, "*.example.com", time.Now().Add(60*24*time.Hour))
	os.MkdirAll(master.sslDir(), 0o700)
	os.WriteFile(filepath.Join(master.sslDir(), "fullchain.pem"), cert, 0o600)
	os.WriteFile(filepath.Join(master.sslDir(), "privkey.pem"), key, 0o600)
	if code, out := adminDo(t, srvM, cM, "POST", "/api/sites", `{"name":"A","upstream":"https://a.example.net","mode":"port","port":18001,"public_url":"https://emby.example.com:8001","enabled":true}`, csrfHeader, "1"); code != 200 {
		t.Fatal(out)
	}

	// 测试里主从在同一台机器上，先关掉主节点的站点端口，让从节点能监听同一个端口
	master.mu.Lock()
	for port, srv := range master.listeners {
		srv.Close()
		delete(master.listeners, port)
	}
	master.mu.Unlock()

	// 从节点（用不同的签名密钥和端口启动）
	node, srvN, cN := newAdmin(t, &Config{})
	nc := newCluster(node)
	srvN.Config.Handler = (&Admin{m: node, cluster: nc, failures: map[string][]time.Time{}}).Handler()
	t.Cleanup(func() { node.mu.Lock(); node.cfg.Sites = nil; node.reload(); node.mu.Unlock() })
	if code, out := adminDo(t, srvN, cN, "PUT", "/api/cluster", `{"role":"node","master_url":"`+srvM.URL+`","token":"`+token+`"}`, csrfHeader, "1"); code != 400 || !strings.Contains(out, "https") {
		t.Fatalf("plain http master must be refused by default: %d %s", code, out)
	}
	node.mu.Lock()
	node.cfg.Cluster = ClusterConfig{Role: "node", MasterURL: srvM.URL, Token: "wrong", NodeName: "tokyo-1", DirectHost: "tokyo-1.example.com"}
	node.mu.Unlock()
	if err := nc.pull(); err == nil || !strings.Contains(err.Error(), "令牌") {
		t.Fatalf("wrong token: %v", err)
	}
	node.mu.Lock()
	node.cfg.Cluster.Token = token
	node.mu.Unlock()
	if err := nc.pull(); err != nil {
		t.Fatal(err)
	}
	if len(node.cfg.Sites) != 1 || node.cfg.Sites[0].Name != "A" || node.cfg.ClusterSecret != master.cfg.ClusterSecret ||
		node.cfg.EntryURL != "https://emby.example.com" || node.cfg.PortBind != "127.0.0.1" {
		t.Fatalf("not synced: %+v", node.cfg)
	}
	if b, _ := os.ReadFile(filepath.Join(node.sslDir(), "privkey.pem")); !bytes.Equal(b, key) {
		t.Fatal("cert not synced")
	}
	if node.sites[node.cfg.Sites[0].ID] == nil {
		t.Fatalf("synced site not running: %v", node.siteErr)
	}
	// 从节点的 nginx 配置已自动生成，引用本机数据目录下的证书
	if conf, err := os.ReadFile(node.nginxConfPath()); err != nil || !strings.Contains(string(conf), "listen 8001 ssl;") {
		t.Fatalf("node nginx conf: %v\n%s", err, conf)
	}
	// 首次同步后本节点加入成员列表，版本会变一次；生产中节点同步后会立刻再拉一次
	if err := nc.pull(); err != nil {
		t.Fatal(err)
	}
	if ms := node.cfg.Members; len(ms) != 1 || ms[0].Name != "tokyo-1" || ms[0].Addr != "127.0.0.1" || ms[0].Host != "tokyo-1.example.com" {
		t.Fatalf("members not synced: %+v", ms)
	}
	// 视频直连：主节点默认开启并同步给从节点；从节点 nginx 的 server_name 带上本机直连域名
	if !master.cfg.Cluster.DirectStream || !node.cfg.Cluster.DirectStream {
		t.Fatalf("direct_stream not synced: master %v node %v", master.cfg.Cluster.DirectStream, node.cfg.Cluster.DirectStream)
	}
	if conf, _ := os.ReadFile(node.nginxConfPath()); !strings.Contains(string(conf), "server_name emby.example.com tokyo-1.example.com;") {
		t.Fatalf("node nginx server_name without direct host:\n%s", conf)
	}
	v1 := nc.status.Version
	if nodes := mc.nodeList(v1); len(nodes) != 1 || nodes[0].Current {
		t.Fatalf("before confirming, node should not be current: %+v", nodes)
	}
	if err := nc.pull(); err != nil || nc.status.Version != v1 { // 304
		t.Fatalf("unchanged pull: %v", err)
	}
	// 主节点上能看到从节点，且是最新
	if nodes := mc.nodeList(v1); len(nodes) != 1 || nodes[0].Name != "tokyo-1" || !nodes[0].Current {
		t.Fatalf("master node list: %+v", nodes)
	}
	// 主节点修改 → 从节点下次同步拿到
	adminDo(t, srvM, cM, "POST", "/api/sites", `{"name":"B","upstream":"https://b.example.net","mode":"port","port":18002,"public_url":"https://emby.example.com:8002"}`, csrfHeader, "1")
	if err := nc.pull(); err != nil || len(node.cfg.Sites) != 2 || nc.status.Version == v1 {
		t.Fatalf("second sync: %v %d", err, len(node.cfg.Sites))
	}
	// 从节点只读
	if code, _ := adminDo(t, srvN, cN, "POST", "/api/sites", `{"name":"x","upstream":"https://x.example.net","mode":"path","path":"x"}`, csrfHeader, "1"); code != 409 {
		t.Fatalf("node should be read-only: %d", code)
	}
	if code, _ := adminDo(t, srvN, cN, "PUT", "/api/settings", `{"port_bind":""}`, csrfHeader, "1"); code != 409 {
		t.Fatalf("node settings should be read-only: %d", code)
	}
	if code, _ := adminDo(t, srvN, cN, "PUT", "/api/settings", `{"old_password":"pw123456","new_password":"nodepass1"}`, csrfHeader, "1"); code != 200 {
		t.Fatalf("node may change its own password: %d", code)
	}
	// 主节点面板 HTTPS 地址写进了 nginx 配置
	if conf, _ := os.ReadFile(master.nginxConfPath()); !strings.Contains(string(conf), "server_name master.example.com;") || !strings.Contains(string(conf), "listen 8443 ssl;") {
		t.Fatalf("master panel block missing:\n%s", conf)
	}
}

func TestNginxIPv6Option(t *testing.T) {
	cfg := &Config{AdminListen: ":55660", Sites: []*Site{{Name: "a", Mode: "port", Port: 18001, PublicURL: "https://e.example.com:8001", Upstream: "https://u", Enabled: true}}}
	if out := nginxConfig(cfg, "", "", nil); !strings.Contains(out, "    # listen [::]:8001 ssl;") {
		t.Fatalf("ipv6 should be commented by default:\n%s", out)
	}
	cfg.NginxIPv6 = true
	if out := nginxConfig(cfg, "", "", nil); !strings.Contains(out, "    listen [::]:8001 ssl;\n") {
		t.Fatalf("ipv6 should be enabled:\n%s", out)
	}
}

// 同一用户固定同一台：两个节点 A、B，各自前面一个 TLS 服务器模拟 nginx。
// 同一个客户端的请求不管落到哪个节点，上游看到的都是同一个节点；不同客户端会分到不同节点；
// 负责节点挂掉时就地处理（POST 请求体不丢）；客户端伪造转发标记无效；WebSocket 能跨节点转发。
func TestStickyAcrossNodes(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/embywebsocket" {
			c, brw, _ := http.NewResponseController(w).Hijack()
			defer c.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			brw.Flush()
			line, _ := brw.ReadString('\n')
			brw.WriteString("echo:" + line)
			brw.Flush()
			return
		}
		b, _ := io.ReadAll(r.Body)
		if r.Header.Get(hopHeader) != "" {
			t.Errorf("hop header leaked upstream")
		}
		fmt.Fprintf(w, "node=%s body=%s", r.Header.Get("X-Node"), b)
	}))
	defer up.Close()

	// 给 emby.test 签证书，两个「nginx」都用它
	caKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTpl := &x509.Certificate{SerialNumber: big.NewInt(20), Subject: pkix.Name{CommonName: "ca"}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	ca, _ := x509.ParseCertificate(caDER)
	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafDER, _ := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(21), DNSNames: []string{"emby.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}, ca, &leafKey.PublicKey, caKey)
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	peerTLS.RootCAs = pool
	defer func() {
		peerTLS.RootCAs = nil
		peerTransports.Range(func(k, _ any) bool { peerTransports.Delete(k); return true })
	}()

	type nodeT struct {
		m     *Manager
		front *httptest.Server
		addr  string
	}
	nodes := map[string]*nodeT{}
	for _, name := range []string{"A", "B"} {
		site := &Site{ID: "s1", Name: "x", Enabled: true, Upstream: up.URL, Mode: "path", Path: "e", StreamMode: "proxy",
			PublicURL: "https://emby.test:8001/e", Headers: map[string]string{"X-Node": name}}
		cfg := &Config{Listen: ":0", AdminListen: ":0", ClusterSecret: "shared", Sites: []*Site{site},
			Cluster: ClusterConfig{Role: "node", NodeName: name, Sticky: true}}
		if err := validateSite(cfg, site); err != nil {
			t.Fatal(err)
		}
		m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
		m.mu.Lock()
		m.reload()
		m.mu.Unlock()
		// 模拟 nginx：TLS、X-Real-IP（测试里用 X-Test-Client 指定客户端 IP）、X-Forwarded-*
		front := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := r.Header.Get("X-Test-Client")
			if ip == "" {
				ip, _, _ = net.SplitHostPort(r.RemoteAddr)
			}
			r.Header.Del("X-Test-Client")
			r.Header.Set("X-Real-Ip", ip)
			r.Header.Set("X-Forwarded-Proto", "https")
			r.Header.Set("X-Forwarded-Host", "emby.test:8001")
			m.ServeHTTP(w, r)
		}))
		front.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{leafDER}, PrivateKey: leafKey}}}
		front.StartTLS()
		t.Cleanup(front.Close)
		nodes[name] = &nodeT{m: m, front: front, addr: strings.TrimPrefix(front.URL, "https://")}
	}
	members := []Member{{Name: "A", Addr: nodes["A"].addr}, {Name: "B", Addr: nodes["B"].addr}}
	for _, n := range nodes {
		n.m.cfg.Members = members
	}
	cl := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "emby.test"}}}
	do := func(node, method, client, body string, hdr ...string) string {
		req, _ := http.NewRequest(method, nodes[node].front.URL+"/e/emby/Items", strings.NewReader(body))
		req.Header.Set("X-Test-Client", client)
		for i := 0; i+1 < len(hdr); i += 2 {
			req.Header.Set(hdr[i], hdr[i+1])
		}
		resp, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return string(b)
	}

	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		client := fmt.Sprintf("203.0.113.%d", i)
		viaA := do("A", "GET", client, "")
		viaB := do("B", "POST", client, "progress")
		ownerA := strings.TrimPrefix(strings.Fields(viaA)[0], "node=")
		ownerB := strings.TrimPrefix(strings.Fields(viaB)[0], "node=")
		if ownerA != ownerB || !strings.Contains(viaB, "body=progress") {
			t.Fatalf("client %s: via A → %q, via B → %q (should be same node, body kept)\nA logs: %+v", client, viaA, viaB, nodes["A"].m.logs.list(nil, 2))
		}
		seen[ownerA] = true
	}
	if !seen["A"] || !seen["B"] {
		t.Fatalf("clients should spread across both nodes: %v", seen)
	}

	// 找一个归 B 负责的客户端
	var clientB string
	for i := 0; i < 50; i++ {
		c := fmt.Sprintf("198.51.100.%d", i)
		if o, _ := nodes["A"].m.owner(c, nil); o.Name == "B" {
			clientB = c
			break
		}
	}
	// 转发出去的请求：A 只计转发，不计本站请求/连接/流量/播放；B 按正常请求计数
	stA, stB := nodes["A"].m.stats["s1"], nodes["B"].m.stats["s1"]
	waitIdle(stA)
	a0, b0 := stA.view(), stB.view()
	do("A", "GET", clientB, "")
	waitIdle(stA)
	a1, b1 := stA.view(), stB.view()
	if a1.Requests != a0.Requests || a1.Bytes != a0.Bytes || a1.Active != 0 || a1.Streams != 0 || a1.Errors != a0.Errors ||
		a1.RelayRequests != a0.RelayRequests+1 || a1.RelayBytes <= a0.RelayBytes {
		t.Fatalf("relay counted on A: before %+v after %+v", a0, a1)
	}
	if b1.Requests != b0.Requests+1 || b1.Bytes <= b0.Bytes || b1.RelayRequests != b0.RelayRequests {
		t.Fatalf("owner B: before %+v after %+v", b0, b1)
	}

	// 客户端伪造转发标记：无效，仍然按负责节点处理
	if got := do("A", "GET", clientB, "", hopHeader, "1|1.2.3.4|s1|A|forged"); !strings.HasPrefix(got, "node=B") {
		t.Fatalf("forged hop should be ignored: %q", got)
	}
	// 访问日志：A 上记录转发给 B，B 上记录由 A 转来，客户端 IP 是原始 IP
	if l := nodes["A"].m.logs.list(nil, 1)[0]; l.Node != "→ B" || l.IP != clientB {
		t.Fatalf("A log: %+v", l)
	}
	if l := nodes["B"].m.logs.list(nil, 1)[0]; l.Node != "← A" || l.IP != clientB {
		t.Fatalf("B log: %+v", l)
	}

	// WebSocket 跨节点：clientB 连到 A，实际由 B 连上游
	conn, err := tls.Dial("tcp", nodes["A"].addr, &tls.Config{RootCAs: pool, ServerName: "emby.test"})
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET /e/embywebsocket?api_key=x HTTP/1.1\r\nHost: emby.test:8001\r\nX-Test-Client: %s\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", clientB)
	br := bufio.NewReader(conn)
	if resp, err := http.ReadResponse(br, nil); err != nil || resp.StatusCode != 101 {
		t.Fatalf("ws upgrade via forward: %v %v", err, resp)
	}
	fmt.Fprintf(conn, "hi\n")
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if line, _ := br.ReadString('\n'); line != "echo:hi\n" {
		t.Fatalf("ws echo via forward = %q", line)
	}
	conn.Close()

	// 视频直连：A、B 各有直连域名并开启后，clientB 落到 A 的视频请求 302 到 B，API 仍转发
	members = []Member{{Name: "A", Addr: nodes["A"].addr, Host: "a.test"}, {Name: "B", Addr: nodes["B"].addr, Host: "b.test"}}
	for name, n := range nodes {
		n.m.mu.Lock()
		n.m.cfg.Members = members
		n.m.cfg.Cluster.DirectStream = true
		n.m.cfg.Cluster.DirectHost = strings.ToLower(name) + ".test"
		n.m.mu.Unlock()
	}
	noFollow := &http.Client{Transport: cl.Transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	raw := func(node, method, path, host string) (int, string, string) {
		req, _ := http.NewRequest(method, nodes[node].front.URL+path, nil)
		req.Header.Set("X-Test-Client", clientB)
		if host != "" {
			req.Host = host
		}
		resp, err := noFollow.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, resp.Header.Get("Location"), string(b)
	}
	waitIdle(stA)
	a0 = stA.view()
	for _, p := range []string{"/e/emby/videos/1/stream.mkv?api_key=x&Static=true", "/e/emby/Videos/1/hls1/main/0.ts", "/e/__ep/sig/https/cdn.example.net/443/v.mp4"} {
		code, loc, _ := raw("A", "GET", p, "")
		if code != http.StatusFound || loc != "https://b.test:8001"+p {
			t.Fatalf("direct stream %s: %d %q", p, code, loc)
		}
	}
	if l := nodes["A"].m.logs.list(nil, 1)[0]; l.Node != "⇢ B 直连" || l.Status != http.StatusFound {
		t.Fatalf("direct log: %+v", l)
	}
	waitIdle(stA)
	if a1 = stA.view(); a1.Requests != a0.Requests || a1.Bytes != a0.Bytes || a1.RelayBytes != a0.RelayBytes || a1.Streams != 0 {
		t.Fatalf("direct redirect should not count traffic on A: before %+v after %+v", a0, a1)
	}
	// API、非 GET 的视频请求：照常转发给 B
	if code, loc, body := raw("A", "GET", "/e/emby/Items", ""); code != 200 || loc != "" || !strings.HasPrefix(body, "node=B") {
		t.Fatalf("api should be forwarded: %d %q %q", code, loc, body)
	}
	if code, _, body := raw("A", "DELETE", "/e/emby/Videos/ActiveEncodings", ""); code != 200 || !strings.HasPrefix(body, "node=B") {
		t.Fatalf("non-GET media request should be forwarded: %d %q", code, body)
	}
	// 客户端已经按 A 的直连域名来访问（成员列表暂时不一致）：不再跳转，转发给 B
	if code, loc, body := raw("A", "GET", "/e/emby/videos/1/stream", "a.test:8001"); code != 200 || loc != "" || !strings.HasPrefix(body, "node=B") {
		t.Fatalf("request on own direct host should not redirect: %d %q %q", code, loc, body)
	}
	// 关闭视频直连：照常转发
	nodes["A"].m.mu.Lock()
	nodes["A"].m.cfg.Cluster.DirectStream = false
	nodes["A"].m.mu.Unlock()
	if code, loc, body := raw("A", "GET", "/e/emby/videos/1/stream", ""); code != 200 || loc != "" || !strings.HasPrefix(body, "node=B") {
		t.Fatalf("direct stream off should forward: %d %q %q", code, loc, body)
	}

	// B 挂掉：A 就地处理，POST 请求体不丢
	nodes["B"].front.Close()
	waitIdle(stA)
	a0 = stA.view()
	if got := do("A", "POST", clientB, "after-down"); got != "node=A body=after-down" {
		t.Fatalf("fallback when owner down: %q", got)
	}
	// 就地处理按本机请求计数，不算转发
	waitIdle(stA)
	if a1 = stA.view(); a1.Requests != a0.Requests+1 || a1.Bytes <= a0.Bytes || a1.RelayRequests != a0.RelayRequests || a1.Active != 0 {
		t.Fatalf("fallback stats: before %+v after %+v", a0, a1)
	}
	if l := nodes["A"].m.logs.list(nil, 1)[0]; !strings.Contains(l.Node, "失联") {
		t.Fatalf("fallback log: %+v", l)
	}
}
