package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestEmbyPrefixFallback(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if strings.HasPrefix(r.URL.Path, "/emby/") {
			w.Write([]byte("ok:" + r.URL.Path))
			return
		}
		http.NotFound(w, r)
	}))
	defer up.Close()
	site := Site{ID: "s1", Name: "t", Enabled: true, Upstream: up.URL, Mode: "path", Path: "hk"}
	cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{&site}}
	validateSite(cfg, &site)
	m := NewManager(t.TempDir()+"/c.json", cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()
	entry := httptest.NewServer(m)
	defer entry.Close()
	if resp, body := get(t, entry.URL+"/hk/System/Info/Public"); resp.StatusCode != 200 || body != "ok:/emby/System/Info/Public" {
		t.Fatalf("fallback: %d %s", resp.StatusCode, body)
	}
	hits.Store(0)
	if _, body := get(t, entry.URL+"/hk/Users/Me"); body != "ok:/emby/Users/Me" || hits.Load() != 1 {
		t.Fatalf("learned prefix should be applied directly: %s hits=%d", body, hits.Load())
	}
}

func TestEPCredentialIsolation(t *testing.T) {
	var gotTok atomic.Value
	gotTok.Store("")
	stream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTok.Store(r.Header.Get("X-Emby-Token") + "|" + r.URL.Query().Get("api_key"))
		w.Write([]byte("v"))
	}))
	defer stream.Close()
	m, entry, _, _ := setup(t, Site{Mode: "path", Path: "hk"})
	// 推流域名用 localhost（和上游 127.0.0.1 不是同一个「域名」）
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(stream.URL, "http://"))
	m.mu.RLock()
	rt := m.sites["s1"]
	m.mu.RUnlock()
	ep := rt.epPath("http", "localhost", port)
	get(t, entry.URL+"/hk"+ep+"/v/1.mkv?api_key=abc", "X-Emby-Token", "secret")
	if v := gotTok.Load().(string); v != "|abc" {
		t.Fatalf("token header should be stripped for foreign stream host, query kept: %q", v)
	}
	m.mu.Lock()
	m.cfg.Sites[0].EPCredentials = true
	m.reload()
	rt = m.sites["s1"]
	m.mu.Unlock()
	get(t, entry.URL+"/hk"+rt.epPath("http", "localhost", port)+"/v/1.mkv?api_key=abc", "X-Emby-Token", "secret")
	if v := gotTok.Load().(string); v != "secret|abc" {
		t.Fatalf("ep_credentials should keep token: %q", v)
	}
}

func TestForceOrigin(t *testing.T) {
	var origin atomic.Value
	origin.Store("")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin.Store(r.Header.Get("Origin") + "|" + r.Header.Get("Referer"))
	}))
	defer up.Close()
	site := Site{ID: "s1", Name: "t", Enabled: true, Upstream: up.URL, Mode: "path", Path: "hk", ForceOrigin: true}
	cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{&site}}
	validateSite(cfg, &site)
	m := NewManager(t.TempDir()+"/c.json", cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()
	entry := httptest.NewServer(m)
	defer entry.Close()
	get(t, entry.URL+"/hk/x")
	if v := origin.Load().(string); v != up.URL+"|"+up.URL+"/" {
		t.Fatalf("force origin: %q", v)
	}
}

func TestPrefDialFallback(t *testing.T) {
	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	d := &net.Dialer{Timeout: 2 * time.Second}
	// 优先 v6，但目标只有 v4：应回退成功
	c, err := prefDial(d, "v6")(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c, err = prefDial(d, "v4")(context.Background(), "tcp", "localhost:"+strings.Split(ln.Addr().String(), ":")[1])
	if err != nil || !strings.HasPrefix(c.RemoteAddr().String(), "127.") {
		t.Fatalf("v4 pref should dial IPv4: %v", err)
	}
	c.Close()
}

func TestReorderSites(t *testing.T) {
	cfg := &Config{Sites: []*Site{{ID: "a", Name: "a", Upstream: "https://a.x", Mode: "path", Path: "a"},
		{ID: "b", Name: "b", Upstream: "https://b.x", Mode: "path", Path: "b"}, {ID: "c", Name: "c", Upstream: "https://c.x", Mode: "path", Path: "c"}}}
	m, srv, c := newAdmin(t, cfg)
	if code, out := adminDo(t, srv, c, "PUT", "/api/sites/order", `{"ids":["c","a"]}`, csrfHeader, "1"); code != 200 {
		t.Fatal(out)
	}
	var ids []string
	for _, s := range m.cfg.Sites {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, ",") != "c,a,b" {
		t.Fatalf("order: %v", ids)
	}
	if code, out := adminDo(t, srv, nil, "GET", "/icons.json", ""); code != 200 || !strings.Contains(out, "emby-icon") {
		t.Fatal("icons.json")
	}
}
