package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUniversalAddress(t *testing.T) {
	m, entry, up, _ := setup(t, Site{Mode: "path", Path: "hk"})
	m.mu.Lock()
	m.cfg.Universal = &UniversalConfig{Enabled: true, Key: "unikey1"}
	m.mu.Unlock()
	base := entry.URL + "/unikey1/" + up.URL
	resp, body := get(t, base+"/emby/Items/1/PlaybackInfo")
	if resp.StatusCode != 200 || !strings.Contains(body, base+"/emby/videos/1/stream") {
		t.Fatalf("universal: %d %s", resp.StatusCode, body)
	}
	// 播放器把 // 合并成 / 也能用
	if resp, _ := get(t, entry.URL+"/unikey1/"+strings.Replace(up.URL, "://", ":/", 1)+"/emby/System/Info/Public"); resp.StatusCode != 200 {
		t.Fatalf("single slash: %d", resp.StatusCode)
	}
	if resp, _ := get(t, entry.URL+"/wrong/"+up.URL+"/emby/System/Info/Public"); resp.StatusCode != 404 {
		t.Fatal("wrong key must 404")
	}
	m.mu.Lock()
	m.cfg.Universal.Enabled = false
	m.mu.Unlock()
	if resp, _ := get(t, base+"/emby/System/Info/Public"); resp.StatusCode != 404 {
		t.Fatal("disabled must 404")
	}
	c := UniversalConfig{Enabled: true}
	if c.normalize() != nil || len(c.Key) < 6 {
		t.Fatal("enable without key should generate one")
	}
}

func TestBarkAndNotifyTest(t *testing.T) {
	var got map[string]string
	var path string
	bk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
	}))
	defer bk.Close()
	old := barkAPI
	barkAPI = bk.URL
	defer func() { barkAPI = old }()
	if err := barkSend("mykey", "标题\n内容"); err != nil || path != "/mykey" || got["title"] != "标题" || got["body"] != "内容" {
		t.Fatalf("bark: %v %s %v", err, path, got)
	}
	if _, err := barkURL("ftp://x"); err == nil {
		t.Fatal("bad bark url")
	}
	if maskBark("https://api.day.app/abcdefgh") != "https://api.day.app/abcd…" {
		t.Fatal(maskBark("https://api.day.app/abcdefgh"))
	}
	_, srv, c := newAdmin(t, &Config{})
	if code, out := adminDo(t, srv, c, "POST", "/api/notify/test", "", csrfHeader, "1"); code != 400 || !strings.Contains(out, "还没有填写") {
		t.Fatal(out)
	}
}

func TestDupUpstreamWarning(t *testing.T) {
	cfg := &Config{Sites: []*Site{{ID: "a", Name: "甲", Upstream: "https://same.example.com", Mode: "path", Path: "a"}}}
	_, srv, c := newAdmin(t, cfg)
	code, out := adminDo(t, srv, c, "POST", "/api/sites", `{"name":"乙","upstream":"same.example.com","mode":"path","path":"b"}`, csrfHeader, "1")
	if code != 200 || !strings.Contains(out, "甲") {
		t.Fatalf("dup warning: %d %s", code, out)
	}
}
