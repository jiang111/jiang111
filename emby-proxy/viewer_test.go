package main

import (
	"strings"
	"testing"
)

func TestViewerAccount(t *testing.T) {
	cfg := &Config{Sites: []*Site{{ID: "s1", Name: "A服", Enabled: true, Upstream: "https://secret-upstream.example.net",
		Backups: []string{"https://backup-secret.example.net"}, Headers: map[string]string{"X-K": "hidden"},
		Mode: "path", Path: "a", AccessKey: "kkkk1"}}}
	_, srv, admin := newAdmin(t, cfg)
	if code, out := adminDo(t, srv, admin, "PUT", "/api/viewers", `{"passwords":["view123"]}`, csrfHeader, "1"); code != 200 {
		t.Fatal(out)
	}
	if code, _ := adminDo(t, srv, admin, "PUT", "/api/viewers", `{"passwords":["pw123456"]}`, csrfHeader, "1"); code != 400 {
		t.Fatal("viewer password equal to admin password must be rejected")
	}
	v := adminLogin(t, srv.URL, "view123")
	code, out := adminDo(t, srv, v, "GET", "/api/state", "")
	if code != 200 || !strings.Contains(out, `"role":"viewer"`) || !strings.Contains(out, "A服") || !strings.Contains(out, "kkkk1") {
		t.Fatalf("viewer state: %d %s", code, out)
	}
	for _, secret := range []string{"secret-upstream", "backup-secret", "hidden", "viewers", "image_cache"} {
		if strings.Contains(out, secret) {
			t.Fatalf("viewer state leaks %q: %s", secret, out)
		}
	}
	for _, c := range [][3]string{{"POST", "/api/sites", `{}`}, {"GET", "/api/logs", ""}, {"GET", "/api/export", ""}, {"PUT", "/api/viewers", `{"passwords":[]}`}, {"GET", "/api/access", ""}} {
		if code, _ := adminDo(t, srv, v, c[0], c[1], c[2], csrfHeader, "1"); code != 403 {
			t.Fatalf("viewer %s %s should be 403, got %d", c[0], c[1], code)
		}
	}
	// 管理员看到的状态里有上游
	if _, out := adminDo(t, srv, admin, "GET", "/api/state", ""); !strings.Contains(out, "secret-upstream") || !strings.Contains(out, `"role":"admin"`) {
		t.Fatal("admin state should include upstream")
	}
	// 改只读密码后旧的只读登录失效
	adminDo(t, srv, admin, "PUT", "/api/viewers", `{"passwords":["other123"]}`, csrfHeader, "1")
	if code, _ := adminDo(t, srv, v, "GET", "/api/state", ""); code != 401 {
		t.Fatalf("old viewer session should be invalid, got %d", code)
	}
}
