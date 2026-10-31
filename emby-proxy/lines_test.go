package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deadURL 返回一个连不上的地址（监听后立即关闭）。
func deadURL(t *testing.T) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return "http://" + addr
}

func TestLineFailoverOnRequest(t *testing.T) {
	dead := deadURL(t)
	m, entry, up, _ := setup(t, Site{Mode: "path", Path: "hk"})
	// 主线路连不上，备用线路是真正的上游
	m.mu.Lock()
	m.cfg.Sites[0].Backups = []string{up.URL}
	m.cfg.Sites[0].Upstream = dead
	normalizeSite(m.cfg.Sites[0])
	m.reload()
	m.mu.Unlock()

	for i := 0; i < 3; i++ {
		resp, body := get(t, entry.URL+"/hk/emby/Items/1/PlaybackInfo")
		if resp.StatusCode != 200 || !strings.Contains(body, "MediaSources") {
			t.Fatalf("request %d via backup failed: %d %s", i, resp.StatusCode, body)
		}
		// 备用线路的地址也要改写成反代地址
		if strings.Contains(body, up.URL) {
			t.Fatalf("backup host not rewritten: %s", body)
		}
	}
	m.mu.RLock()
	lv, ev := m.lineViews("s1")
	m.mu.RUnlock()
	if len(lv) != 2 || !lv[0].Down || lv[0].Active || !lv[1].Active {
		t.Fatalf("primary should be down and backup active: %+v", lv)
	}
	if len(ev) == 0 || !strings.Contains(ev[len(ev)-1].Text, "备用线路 1") {
		t.Fatalf("switch event missing: %+v", ev)
	}
	// POST 有请求体，不能重放，不换线重试（这里主线路已判为不可用，直接走备用线路）
	resp, err := http.Post(entry.URL+"/hk/emby/Items/1/PlaybackInfo", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == 502 {
		t.Fatal("POST should go to the active (backup) line")
	}
}

func TestLineProbeChoose(t *testing.T) {
	m, _, up, _ := setup(t, Site{Mode: "path", Path: "hk"})
	m.mu.Lock()
	m.cfg.Sites[0].Backups = []string{"http://backup.example.net:8096"}
	m.reload()
	m.mu.Unlock()

	var mu sync.Mutex
	lat := map[string]time.Duration{up.URL: 200 * time.Millisecond, "http://backup.example.net:8096": 40 * time.Millisecond}
	fail := map[string]bool{}
	var sent []string
	m.lines.probe = func(s Site, ln *upLine) (time.Duration, error) {
		mu.Lock()
		defer mu.Unlock()
		if fail[ln.key] {
			return 0, errors.New("connection refused")
		}
		return lat[ln.key], nil
	}
	m.lines.notify = func(text string) { mu.Lock(); sent = append(sent, text); mu.Unlock() }
	probe := func() {
		m.lines.mu.Lock()
		m.lines.next = map[string]time.Time{}
		m.lines.mu.Unlock()
		m.probeDue(time.Now(), true)
	}
	active := func() string {
		m.mu.RLock()
		defer m.mu.RUnlock()
		return m.sites["s1"].activeLine().key
	}

	probe()
	if a := active(); a != "http://backup.example.net:8096" {
		t.Fatalf("should switch to the much faster backup, got %s", a)
	}
	// 只快一点点不切换（防止来回跳）
	mu.Lock()
	lat[up.URL] = 35 * time.Millisecond
	mu.Unlock()
	for i := 0; i < 5; i++ {
		probe()
	}
	if a := active(); a != "http://backup.example.net:8096" {
		t.Fatalf("should not flap for a small gain, got %s", a)
	}
	// 备用线路挂了：连续 2 次失败后换回主线路
	mu.Lock()
	fail["http://backup.example.net:8096"] = true
	mu.Unlock()
	probe()
	if a := active(); a != "http://backup.example.net:8096" {
		t.Fatal("one failure should not switch yet")
	}
	probe()
	if a := active(); a != up.URL {
		t.Fatalf("should fail over to primary, got %s", a)
	}
	// 全部挂了：推送一次「全部线路不可用」，恢复后推送「已恢复」
	mu.Lock()
	fail[up.URL] = true
	mu.Unlock()
	probe()
	probe()
	probe()
	mu.Lock()
	fail[up.URL] = false
	mu.Unlock()
	probe()
	probe()
	mu.Lock()
	defer mu.Unlock()
	all := strings.Join(sent, "\n")
	if strings.Count(all, "全部线路不可用") != 1 || !strings.Contains(all, "已恢复") || !strings.Contains(all, "切换到备用线路 1") {
		t.Fatalf("unexpected notifications:\n%s", all)
	}
	if strings.Contains(all, "backup.example.net") || strings.Contains(all, up.URL) {
		t.Fatal("notifications must not contain upstream addresses")
	}
}

func TestValidateBackups(t *testing.T) {
	cfg := &Config{Listen: ":1", AdminListen: ":2"}
	s := &Site{Name: "a", Upstream: "a.example.com", Backups: []string{" a.example.com ", "1.2.3.4:8096", "1.2.3.4:8096", ""}, Mode: "path", Path: "a"}
	if err := validateSite(cfg, s); err != nil {
		t.Fatal(err)
	}
	if len(s.Backups) != 1 || s.Backups[0] != "http://1.2.3.4:8096" {
		t.Fatalf("backups not normalized: %v", s.Backups)
	}
	s.Backups = []string{"ftp://x"}
	if err := validateSite(cfg, s); err == nil {
		t.Fatal("bad backup should be rejected")
	}
}

func TestImageCache(t *testing.T) {
	var hits atomic.Int64
	img := []byte(strings.Repeat("P", 5000))
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch {
		case strings.Contains(r.URL.Path, "/Images/"):
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("ETag", `"abc"`)
			if strings.Contains(r.URL.Path, "Private") {
				w.Header().Set("Cache-Control", "private")
			}
			w.Write(img)
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	site := Site{ID: "s1", Name: "t", Enabled: true, Upstream: up.URL, Mode: "path", Path: "hk"}
	cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{&site}}
	if err := validateSite(cfg, &site); err != nil {
		t.Fatal(err)
	}
	m := NewManager(t.TempDir()+"/config.json", cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()
	entry := httptest.NewServer(m)
	defer entry.Close()

	u := entry.URL + "/hk/emby/Items/1/Images/Primary?maxHeight=300&tag=t1&api_key="
	r1, b1 := get(t, u+"userA")
	r2, b2 := get(t, u+"userB") // 不同用户的凭据参数不影响命中
	if r1.StatusCode != 200 || r2.StatusCode != 200 || b1 != string(img) || b2 != string(img) {
		t.Fatalf("bad responses %d %d", r1.StatusCode, r2.StatusCode)
	}
	if r2.Header.Get("X-Cache") != "HIT" || hits.Load() != 1 {
		t.Fatalf("second request should hit cache: upstream hits=%d", hits.Load())
	}
	if r2.Header.Get("Content-Type") != "image/jpeg" || r2.Header.Get("Etag") != `"abc"` {
		t.Fatalf("cached headers lost: %v", r2.Header)
	}
	if r3, _ := get(t, u+"x", "If-None-Match", `"abc"`); r3.StatusCode != 304 {
		t.Fatalf("If-None-Match should give 304, got %d", r3.StatusCode)
	}
	// 尺寸参数不同是不同的图
	get(t, entry.URL+"/hk/emby/Items/1/Images/Primary?maxHeight=600&tag=t1")
	if hits.Load() != 2 {
		t.Fatalf("different size must miss, hits=%d", hits.Load())
	}
	// 没有 tag、Range、private 都不缓存
	for _, c := range []struct{ path, hdr string }{
		{"/hk/emby/Items/2/Images/Primary", ""},
		{"/hk/emby/Items/3/Images/Primary?tag=x", "Range"},
		{"/hk/emby/Items/4/Images/Private?tag=x", ""},
	} {
		before := hits.Load()
		for i := 0; i < 2; i++ {
			if c.hdr != "" {
				get(t, entry.URL+c.path, c.hdr, "bytes=0-10")
			} else {
				get(t, entry.URL+c.path)
			}
		}
		if hits.Load()-before != 2 {
			t.Fatalf("%s should not be cached", c.path)
		}
	}
	v := m.imgs.view()
	if v.Entries != 2 || v.Hits < 2 || v.Saved == 0 {
		t.Fatalf("unexpected cache view %+v", v)
	}
}

func TestImageCacheLRU(t *testing.T) {
	c := newImageCache(10_000)
	for i := 0; i < 5; i++ {
		c.put(fmt.Sprint(i), make([]byte, 2400), http.Header{})
	}
	if c.get("0") != nil || c.get("4") == nil {
		t.Fatal("oldest entry should be evicted")
	}
	if c.view().Bytes > 10_000 {
		t.Fatal("cache over budget")
	}
	off := newImageCache(0)
	off.put("a", []byte("x"), http.Header{})
	if off.get("a") != nil {
		t.Fatal("disabled cache must not store")
	}
}

// 客户端不读数据（卡死）时，超过写超时断开，上游拉流随之取消。
func TestWriteStall(t *testing.T) {
	old := writeStall
	writeStall = 300 * time.Millisecond
	defer func() { writeStall = old }()
	canceled := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp4")
		buf := make([]byte, 64<<10)
		for {
			if _, err := w.Write(buf); err != nil {
				close(canceled)
				return
			}
			select {
			case <-r.Context().Done():
				close(canceled)
				return
			default:
			}
		}
	}))
	defer up.Close()
	site := Site{ID: "s1", Name: "t", Enabled: true, Upstream: up.URL, Mode: "path", Path: "hk"}
	cfg := &Config{Listen: ":0", AdminListen: ":0", Sites: []*Site{&site}}
	validateSite(cfg, &site)
	m := NewManager(t.TempDir()+"/config.json", cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()
	entry := httptest.NewServer(m)
	defer entry.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(entry.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetReadBuffer(4096)
	}
	fmt.Fprintf(conn, "GET /hk/emby/videos/1/stream HTTP/1.1\r\nHost: x\r\n\r\n")
	io.ReadFull(conn, make([]byte, 100)) // 读一点就不读了
	select {
	case <-canceled:
	case <-time.After(10 * time.Second):
		t.Fatal("stalled client should be cut off and upstream canceled")
	}
}
