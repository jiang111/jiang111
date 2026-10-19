package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAccountCrypto(t *testing.T) {
	key := make([]byte, 32)
	enc, err := sealAccount(key, "s1", siteAccount{"alice", "s3cret"})
	if err != nil || !strings.HasPrefix(enc, accountPrefix) || strings.Contains(enc, "alice") {
		t.Fatalf("seal: %q %v", enc, err)
	}
	if a, err := openAccount(key, "s1", enc); err != nil || a.Username != "alice" || a.Password != "s3cret" {
		t.Fatalf("open: %+v %v", a, err)
	}
	if _, err := openAccount(key, "s2", enc); err == nil {
		t.Fatal("ciphertext moved to another site must not decrypt")
	}
	other := make([]byte, 32)
	other[0] = 1
	if _, err := openAccount(other, "s1", enc); err == nil {
		t.Fatal("wrong key must not decrypt")
	}
	if _, err := openAccount(key, "s1", enc[:len(enc)-4]+"AAAA"); err == nil {
		t.Fatal("tampered ciphertext must not decrypt")
	}
}

// 账号接口：加密落盘；登录后状态里能看到，没登录看不到；导出、同步不带；删站点一起删。
func TestAccountAPI(t *testing.T) {
	dir := t.TempDir()
	cfg := &Config{AdminPassword: "pw123456", Secret: "s", AdminListen: ":1", Listen: ":2", ClusterSecret: randHex(16)}
	m := NewManager(filepath.Join(dir, "config.json"), cfg)
	c := newCluster(m)
	a := &Admin{m: m, cluster: c, failures: map[string][]time.Time{}}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	ck := adminLogin(t, srv.URL, "pw123456")
	do := func(method, path, body string) (int, string) {
		return adminDo(t, srv, ck, method, path, body, csrfHeader, "1")
	}
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"ctl"}`); code != 200 {
		t.Fatal(out)
	}
	_, out := do("POST", "/api/sites", `{"name":"A","upstream":"https://a.example.net","mode":"path","path":"a","enabled":true}`)
	var r struct{ ID string }
	json.Unmarshal([]byte(out), &r)
	id := r.ID

	if code, out := do("PUT", "/api/sites/"+id+"/account", `{"username":"","password":"x"}`); code != 400 {
		t.Fatalf("empty username: %d %s", code, out)
	}
	if code, out := do("PUT", "/api/sites/nope/account", `{"username":"alice","password":"x"}`); code != 404 {
		t.Fatalf("unknown site: %d %s", code, out)
	}
	if code, out := do("PUT", "/api/sites/"+id+"/account", `{"username":"alice-user","password":"s3cret-pass"}`); code != 200 {
		t.Fatalf("save: %d %s", code, out)
	}
	secret := func(where, s string) {
		t.Helper()
		if strings.Contains(s, "s3cret-pass") || strings.Contains(s, "alice-user") {
			t.Fatalf("%s leaks account: %s", where, s)
		}
	}
	disk, _ := os.ReadFile(filepath.Join(dir, "config.json"))
	secret("config.json", string(disk))
	if !strings.Contains(string(disk), `"accounts"`) {
		t.Fatal("account not saved")
	}
	if fi, err := os.Stat(filepath.Join(dir, "account.key")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("account.key: %v %v", fi, err)
	}

	// 登录后状态里直接有账号密码；没登录访问不到
	_, st := do("GET", "/api/state", "")
	if !strings.Contains(st, `"account":{"username":"alice-user","password":"s3cret-pass"}`) {
		t.Fatalf("state: %s", st)
	}
	if code, out := adminDo(t, srv, nil, "GET", "/api/state", ""); code != 401 || strings.Contains(out, "s3cret") {
		t.Fatalf("state without login: %d %s", code, out)
	}
	if code, _ := adminDo(t, srv, nil, "PUT", "/api/sites/"+id+"/account", `{"username":"x","password":"y"}`, csrfHeader, "1"); code != 401 {
		t.Fatalf("save without login: %d", code)
	}
	_, exp := do("GET", "/api/export", "")
	secret("export", exp)
	data, _, err := c.payload()
	if err != nil {
		t.Fatal(err)
	}
	secret("sync payload", string(data))
	if strings.Contains(string(data), "accounts") || strings.Contains(string(data), accountPrefix) {
		t.Fatalf("sync payload carries accounts: %s", data)
	}

	// 整体保存：密码可以改成空
	if code, out := do("PUT", "/api/sites/"+id+"/account", `{"username":"bob","password":""}`); code != 200 {
		t.Fatal(out)
	}
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, `"account":{"username":"bob","password":""}`) {
		t.Fatalf("empty password: %s", st)
	}

	// 缺少 account.key：显示无法解密；重新填写可以
	os.Remove(filepath.Join(dir, "account.key"))
	m.acctMu.Lock()
	m.acctKey = nil
	m.acctMu.Unlock()
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, "缺少 account.key") {
		t.Fatalf("missing key state: %s", st)
	}
	if code, out := do("PUT", "/api/sites/"+id+"/account", `{"username":"bob","password":"new-pass"}`); code != 200 {
		t.Fatal(out)
	}
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, `"password":"new-pass"`) {
		t.Fatalf("re-entered: %s", st)
	}

	// 删除账号、删除站点
	if code, _ := do("DELETE", "/api/sites/"+id+"/account", ""); code != 200 || len(m.cfg.Accounts) != 0 {
		t.Fatalf("delete account: %v", m.cfg.Accounts)
	}
	do("PUT", "/api/sites/"+id+"/account", `{"username":"bob","password":"x"}`)
	if code, _ := do("DELETE", "/api/sites/"+id, ""); code != 200 || len(m.cfg.Accounts) != 0 {
		t.Fatalf("delete site should drop its account: %v", m.cfg.Accounts)
	}

	// 从节点不接受修改；应用主节点同步时清掉本机账号
	m.mu.Lock()
	m.cfg.Cluster.Role = "node"
	m.cfg.Accounts = map[string]string{"x": "v1:abc"}
	m.mu.Unlock()
	if code, _ := do("PUT", "/api/sites/x/account", `{"username":"bob","password":"x"}`); code != 409 {
		t.Fatalf("node should reject: %d", code)
	}
	if err := c.apply(&syncPayload{ClusterSecret: "k"}); err != nil || m.cfg.Accounts != nil {
		t.Fatalf("node apply should drop accounts: %v %v", err, m.cfg.Accounts)
	}
}
