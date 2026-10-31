package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestUpdateFlow(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.RawQuery, "path=emby-proxy") {
			t.Errorf("query: %s", r.URL.RawQuery)
		}
		w.Write([]byte(`[{"sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","commit":{"message":"新功能\n详细","committer":{"date":"2026-10-31T00:00:00Z"}}}]`))
	}))
	defer gh.Close()
	old, oldC := githubAPI, commit
	githubAPI, commit = gh.URL, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	defer func() { githubAPI, commit = old, oldC }()

	m, srv, c := newAdmin(t, &Config{})
	code, out := adminDo(t, srv, c, "GET", "/api/update", "")
	if code != 200 || !strings.Contains(out, `"behind":true`) || !strings.Contains(out, "新功能") || strings.Contains(out, "详细") {
		t.Fatalf("update info: %s", out)
	}
	if code, out := adminDo(t, srv, c, "POST", "/api/update", `{}`, csrfHeader, "1"); code != 400 || !strings.Contains(out, "重新执行一次安装命令") {
		t.Fatalf("without host updater: %d %s", code, out)
	}
	os.MkdirAll(m.updateDir(), 0o755)
	os.WriteFile(filepath.Join(m.updateDir(), "installed"), []byte("1"), 0o644)
	if code, out := adminDo(t, srv, c, "POST", "/api/update", `{}`, csrfHeader, "1"); code != 200 {
		t.Fatal(out)
	}
	if b, _ := os.ReadFile(filepath.Join(m.updateDir(), "request")); strings.TrimSpace(string(b)) != strings.Repeat("b", 40) {
		t.Fatalf("request file: %q", b)
	}
	os.WriteFile(filepath.Join(m.updateDir(), "status"), []byte("running\n9999999999\nbbbb\n"), 0o644)
	if err := m.requestUpdate(strings.Repeat("c", 40)); err == nil || !strings.Contains(err.Error(), "正在更新") {
		t.Fatal("should refuse while running")
	}
	if code, _ := adminDo(t, srv, c, "POST", "/api/update", `{"sha":"x;rm -rf /"}`, csrfHeader, "1"); code != 400 {
		t.Fatal("bad sha must be rejected")
	}
	// 从节点跟随：版本不同 → 请求更新（同一版本不重复请求）
	os.WriteFile(filepath.Join(m.updateDir(), "status"), []byte("ok\n1\nbbbb\n"), 0o644)
	os.Remove(filepath.Join(m.updateDir(), "request"))
	m.followMaster(strings.Repeat("d", 40))
	if b, _ := os.ReadFile(filepath.Join(m.updateDir(), "request")); !strings.HasPrefix(string(b), "dddd") {
		t.Fatal("node should follow master version")
	}
	os.Remove(filepath.Join(m.updateDir(), "request"))
	m.followMaster(strings.Repeat("d", 40))
	if _, err := os.Stat(filepath.Join(m.updateDir(), "request")); err == nil {
		t.Fatal("same target should not be requested again soon")
	}
	m.followMaster(commit)
	if _, err := os.Stat(filepath.Join(m.updateDir(), "request")); err == nil {
		t.Fatal("same version must not update")
	}
}
