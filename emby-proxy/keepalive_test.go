package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 和 Python 的 str(uuid.uuid5(uuid.NAMESPACE_URL, ...)).upper() 一致，沿用 emby-keepalive 的设备 ID。
func TestUUID5URL(t *testing.T) {
	if got := uuid5URL("https://a.example.com:443alice"); got != "26F12887-97BE-50E2-B258-5CF5157361B5" {
		t.Fatal(got)
	}
}

func TestKaNext(t *testing.T) {
	tz := time.FixedZone("UTC+8", 8*3600)
	at := func(day, hm string) time.Time {
		v, _ := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, tz)
		return v
	}
	cfg := defaultKeepalive() // 每 3 天 04:00，失败 2 小时后再试
	now := at("2026-10-19", "10:00")
	cases := []struct {
		st   *kaSiteState
		want time.Time
	}{
		{nil, at("2026-10-19", "04:00")}, // 从没成功过：今天到点就跑
		{&kaSiteState{LastSuccess: "2026-10-17"}, at("2026-10-20", "04:00")},
		{&kaSiteState{LastSuccess: "2026-10-17", LastAttempt: at("2026-10-20", "05:00").Unix()}, at("2026-10-20", "07:00")},
		{&kaSiteState{LastAttempt: at("2026-10-19", "09:00").Unix()}, at("2026-10-19", "11:00")},
	}
	for i, c := range cases {
		if got := kaNext(cfg, c.st, now); !got.Equal(c.want) {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

// config.json 里的保号设置不完整时用默认值，不会让后台循环出错。
func TestKeepaliveConfigDefaults(t *testing.T) {
	m, _ := kaManager(t, "https://up.example.net", "alice", "pw")
	m.cfg.Keepalive = &KeepaliveConfig{Enabled: true, WatchMin: 5, WatchMax: 2, Sites: map[string]bool{"s1": true}}
	c := m.keepaliveConfig()
	d := defaultKeepalive()
	if c.RunAt != d.RunAt || c.IntervalDays != d.IntervalDays || c.WatchMax != 5 || c.Retries != d.Retries || c.Client != d.Client {
		t.Fatalf("defaults: %+v", c)
	}
	if due, _ := m.ka.due(time.Now()); len(due) != 1 {
		t.Fatal("should still schedule")
	}
}

func TestKeepaliveValidate(t *testing.T) {
	ok := defaultKeepalive()
	if err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*KeepaliveConfig){
		"run_at":   func(c *KeepaliveConfig) { c.RunAt = "4:00" },
		"interval": func(c *KeepaliveConfig) { c.IntervalDays = 0 },
		"watch":    func(c *KeepaliveConfig) { c.WatchMin, c.WatchMax = 9, 6 },
		"client":   func(c *KeepaliveConfig) { c.Client.Name = `Sen"Player` },
		"empty":    func(c *KeepaliveConfig) { c.Client.UserAgent = " " },
		"retries":  func(c *KeepaliveConfig) { c.Retries = 0 },
		"telegram": func(c *KeepaliveConfig) { c.TelegramToken = "1/../x" },
	} {
		c := defaultKeepalive()
		mod(&c)
		if c.validate() == nil {
			t.Errorf("%s should be rejected", name)
		}
	}
}

// kaFakeEmby 记录保号客户端的请求。
type kaFakeEmby struct {
	mu        sync.Mutex
	events    []string
	logins    int
	auth      string
	ua        string
	ranges    []string
	itemsCode int
	positions []int64
}

func newKaFakeEmby(t *testing.T) (*kaFakeEmby, *httptest.Server) {
	f := &kaFakeEmby{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		authed := r.Header.Get("X-Emby-Token") == "tok"
		switch {
		case r.Method == "POST" && r.URL.Path == "/emby/Users/AuthenticateByName":
			f.logins++
			f.auth, f.ua = r.Header.Get("X-Emby-Authorization"), r.Header.Get("User-Agent")
			var b struct{ Username, Pw string }
			json.NewDecoder(r.Body).Decode(&b)
			if b.Username != "alice" || b.Pw != "pw" {
				w.WriteHeader(401)
				io.WriteString(w, "Invalid username or password")
				return
			}
			io.WriteString(w, `{"AccessToken":"tok","User":{"Id":"u1"}}`)
		case !authed:
			w.WriteHeader(401)
		case r.Method == "GET" && r.URL.Path == "/emby/Users/u1/Items":
			if f.itemsCode != 0 {
				w.WriteHeader(f.itemsCode)
				return
			}
			io.WriteString(w, `{"Items":[{"Id":"short","Name":"短片","RunTimeTicks":10},{"Id":"i1","Name":"第1集","SeriesName":"某剧","RunTimeTicks":36000000000000}]}`)
		case r.Method == "POST" && r.URL.Path == "/emby/Items/i1/PlaybackInfo":
			io.WriteString(w, `{"PlaySessionId":"ps","MediaSources":[{"Id":"ms","Container":"mkv,webm"}]}`)
		case r.Method == "GET" && r.URL.Path == "/emby/videos/i1/original.mkv":
			if q := r.URL.Query(); q.Get("MediaSourceId") != "ms" || q.Get("PlaySessionId") != "ps" || q.Get("api_key") != "tok" {
				t.Errorf("stream query: %s", r.URL.RawQuery)
			}
			f.ranges = append(f.ranges, r.Header.Get("Range"))
			f.mu.Unlock()
			http.ServeContent(w, r, "v.mkv", time.Time{}, bytes.NewReader(make([]byte, 100<<10))) // 100 KB 的片子
			f.mu.Lock()
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/emby/Sessions/"):
			ev := strings.TrimPrefix(r.URL.Path, "/emby/Sessions/")
			f.events = append(f.events, ev)
			var b struct {
				ItemId, PlaySessionId string
				PositionTicks         int64
			}
			json.NewDecoder(r.Body).Decode(&b)
			if ev != "Logout" {
				if b.ItemId != "i1" || b.PlaySessionId != "ps" {
					t.Errorf("%s body: %+v", ev, b)
				}
				f.positions = append(f.positions, b.PositionTicks)
			}
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

// kaManager：一个站点指向假的 Emby（停用状态，直接连上游），存好账号。
func kaManager(t *testing.T, upstream, user, pw string) (*Manager, string) {
	dir := t.TempDir()
	cfg := &Config{Sites: []*Site{{ID: "s1", Name: "A服", Mode: "path", Path: "a", Upstream: upstream}}}
	m := NewManager(filepath.Join(dir, "config.json"), cfg)
	key, err := m.accountKey(true)
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := sealAccount(key, "s1", siteAccount{Username: user, Password: pw})
	m.cfg.Accounts = map[string]string{"s1": enc}
	m.ka.sec = time.Millisecond
	m.ka.gap = func() time.Duration { return 0 }
	return m, dir
}

func fastKeepalive() KeepaliveConfig {
	c := defaultKeepalive()
	c.WatchMin, c.WatchMax, c.ReportInterval, c.StreamKbps = 1, 1, 10, 32 // 「一分钟」= 60 毫秒，每 10 毫秒上报
	return c
}

// 完整流程：登录 → 选够长的片 → 开始 / 进度 / 停止 → 拉流 → 退出；记录成功日期并推送 Telegram。
func TestKeepaliveRun(t *testing.T) {
	f, srv := newKaFakeEmby(t)
	m, dir := kaManager(t, srv.URL, "alice", "pw")
	var tgPath, tgText string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		tgPath, tgText = r.URL.Path, r.Form.Get("chat_id")+"|"+r.Form.Get("text")
	}))
	defer tg.Close()
	old := telegramAPI
	telegramAPI = tg.URL
	defer func() { telegramAPI = old }()

	cfg := fastKeepalive()
	cfg.TelegramToken, cfg.TelegramChat = "123:abc", "42"
	ts, _ := m.ka.targets()
	if len(ts) != 1 || ts[0].Base != srv.URL || ts[0].Acc.DeviceID != uuid5URL(srv.URL+"alice") {
		t.Fatalf("targets: %+v", ts)
	}
	m.ka.runBatch(ts, cfg, true)

	f.mu.Lock()
	ev := strings.Join(f.events, ",")
	if !strings.HasPrefix(ev, "Playing,Playing/Progress,Playing/Progress") || !strings.HasSuffix(ev, "Playing/Stopped,Logout") {
		t.Errorf("events: %s", ev)
	}
	// 每 10「秒」用 Range 请求一段（至少 64 KB）；读到文件尾后从头再来
	if len(f.ranges) < 3 || f.ranges[0] != "bytes=0-65535" || f.ranges[1] != "bytes=65536-131071" || f.ranges[2] != "bytes=0-65535" {
		t.Errorf("stream ranges: %v", f.ranges)
	}
	if !strings.Contains(f.auth, `Client="SenPlayer"`) || !strings.Contains(f.auth, `DeviceId="`+uuid5URL(srv.URL+"alice")+`"`) || f.ua != "SenPlayer/6.2.1" {
		t.Errorf("client identity: %s / %s", f.auth, f.ua)
	}
	for i := 1; i < len(f.positions); i++ {
		if f.positions[i] < f.positions[i-1] {
			t.Errorf("position went back: %v", f.positions)
		}
	}
	f.mu.Unlock()

	st := m.ka.state["s1"]
	if !st.LastOK || st.LastSuccess != time.Now().Format(dayLayout) || st.LastAttempt != 0 || !strings.Contains(st.LastResult, "《某剧 - 第1集》") || !strings.Contains(st.LastResult, "拉流 0.") {
		t.Fatalf("state: %+v", st)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "keepalive.json")); !strings.Contains(string(data), time.Now().Format(dayLayout)) {
		t.Fatalf("state file: %s", data)
	}
	if tgPath != "/bot123:abc/sendMessage" || !strings.HasPrefix(tgText, "42|Emby 保号全部成功") || !strings.Contains(tgText, "A服") {
		t.Fatalf("telegram: %s %s", tgPath, tgText)
	}
	if strings.Contains(tgText, "pw") && strings.Contains(tgText, "alice") {
		t.Fatal("telegram leaks account")
	}
}

// 账号被拒绝不重试；其它错误按次数重试，失败后记下时间，几小时后再试。
func TestKeepaliveFailures(t *testing.T) {
	f, srv := newKaFakeEmby(t)
	m, _ := kaManager(t, srv.URL, "alice", "wrong")
	var tgText string
	tg := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.ParseForm()
		tgText = r.Form.Get("text")
	}))
	defer tg.Close()
	old := telegramAPI
	telegramAPI = tg.URL
	defer func() { telegramAPI = old }()
	cfg := fastKeepalive()
	cfg.TelegramToken, cfg.TelegramChat = "123:abc", "42"
	ts, _ := m.ka.targets()
	m.ka.runBatch(ts, cfg, false)
	if f.logins != 1 {
		t.Fatalf("401 should not retry: %d logins", f.logins)
	}
	st := m.ka.state["s1"]
	if st.LastOK || st.LastSuccess != "" || !st.AuthFailed || !strings.Contains(st.LastResult, "401") || !strings.Contains(st.LastResult, "已停止自动保号") {
		t.Fatalf("state: %+v", st)
	}
	if !strings.Contains(tgText, "有账号密码被拒绝") || !strings.Contains(tgText, "🔒 A服：账号密码被拒绝") {
		t.Fatalf("telegram: %s", tgText)
	}
	// 被拒绝后不再定时重试（普通失败过几小时会再试）；更新账号或重新打开开关后恢复
	cfg.Enabled, cfg.Sites = true, map[string]bool{"s1": true}
	m.cfg.Keepalive = &cfg
	if due, _ := m.ka.due(time.Now().Add(24 * time.Hour)); len(due) != 0 {
		t.Fatal("auth-failed site must not be retried")
	}
	if v := m.ka.view(time.Now()); len(v.Sites) != 1 || !v.Sites[0].AuthFailed || v.Sites[0].Next != 0 {
		t.Fatalf("view: %+v", v.Sites)
	}
	m.ka.resetAuthFailed("s1")
	if due, _ := m.ka.due(time.Now()); len(due) != 1 {
		t.Fatal("should be due again after the account is updated")
	}

	f2, srv2 := newKaFakeEmby(t)
	f2.itemsCode = 500
	m2, _ := kaManager(t, srv2.URL, "alice", "pw")
	ts2, _ := m2.ka.targets()
	m2.ka.runBatch(ts2, cfg, false)
	if f2.logins != cfg.Retries {
		t.Fatalf("should retry %d times: %d", cfg.Retries, f2.logins)
	}
	if st := m2.ka.state["s1"]; st.LastOK || !strings.Contains(st.LastResult, "HTTP 500") {
		t.Fatalf("state: %+v", st)
	}
	// 失败后要等 retry_hours 才到期
	m2.cfg.Keepalive = &cfg
	m2.cfg.Keepalive.Enabled, m2.cfg.Keepalive.Sites = true, map[string]bool{"s1": true}
	if due, _ := m2.ka.due(time.Now()); len(due) != 0 {
		t.Fatal("failed site should wait before retrying")
	}
	if due, _ := m2.ka.due(time.Now().Add(3 * time.Hour)); len(due) != 1 {
		t.Fatal("failed site should be due after retry_hours")
	}
}

// 到期判断：总开关或站点开关关闭、从节点不跑；没跑过的到点就跑；成功后等 N 天；用站点客户端地址。
func TestKeepaliveDue(t *testing.T) {
	m, _ := kaManager(t, "https://up.example.net", "alice", "pw")
	now := time.Now()
	if due, _ := m.ka.due(now); len(due) != 0 {
		t.Fatal("disabled should not run")
	}
	cfg := defaultKeepalive()
	cfg.Enabled, cfg.RunAt = true, "00:00"
	m.cfg.Keepalive = &cfg
	m.cfg.Sites[0].Enabled, m.cfg.Sites[0].PublicURL = true, "https://emby.example.com:8001"
	if due, _ := m.ka.due(now); len(due) != 0 {
		t.Fatal("site switch off: should not run")
	}
	if _, err := m.ka.runNow(); err == nil || !strings.Contains(err.Error(), "没有开启保号的站点") {
		t.Fatalf("run now with site switch off: %v", err)
	}
	cfg.Sites = map[string]bool{"s1": true}
	due, _ := m.ka.due(now)
	if len(due) != 1 || due[0].Base != "https://emby.example.com:8001" {
		t.Fatalf("never-run site should be due, via public URL: %+v", due)
	}
	m.ka.state["s1"] = &kaSiteState{LastSuccess: now.Format(dayLayout)}
	if due, _ := m.ka.due(now); len(due) != 0 {
		t.Fatal("succeeded today: wait interval")
	}
	if due, _ := m.ka.due(now.AddDate(0, 0, 3)); len(due) != 1 {
		t.Fatal("should be due after interval")
	}
	m.ka.running["s1"] = true
	if due, _ := m.ka.due(now.AddDate(0, 0, 3)); len(due) != 0 {
		t.Fatal("running site should not start again")
	}
	m.ka.running["s1"] = false
	m.cfg.Cluster.Role = "node"
	if due, _ := m.ka.due(now.AddDate(0, 0, 3)); len(due) != 0 {
		t.Fatal("node should not run")
	}
}

// 面板接口：保存设置（校验、令牌打码 / 保留 / 清除）、状态里有保号信息、导出不带；编辑账号保留设备 ID。
func TestKeepaliveAPI(t *testing.T) {
	m, srv, ck := newAdmin(t, &Config{})
	do := func(method, path, body string) (int, string) {
		return adminDo(t, srv, ck, method, path, body, csrfHeader, "1")
	}
	if code, out := do("POST", "/api/keepalive/run", ""); code != 409 || !strings.Contains(out, "没有开启保号的站点") {
		t.Fatalf("run without accounts: %d %s", code, out)
	}
	body := func(extra string) string {
		c := defaultKeepalive()
		c.Enabled = true
		b, _ := json.Marshal(c)
		return strings.TrimSuffix(string(b), "}") + extra + "}"
	}
	if code, out := do("PUT", "/api/keepalive", strings.Replace(body(""), `"04:00"`, `"25:00"`, 1)); code != 400 {
		t.Fatalf("bad run_at: %d %s", code, out)
	}
	if code, out := do("PUT", "/api/keepalive", body(`,"telegram_token":"123456:secret-token","telegram_chat":"42"`)); code != 200 {
		t.Fatal(out)
	}
	_, st := do("GET", "/api/state", "")
	if strings.Contains(st, "secret-token") || !strings.Contains(st, `"telegram_token":"1234…"`) || !strings.Contains(st, `"enabled":true`) {
		t.Fatalf("state: %s", st)
	}
	// 不传令牌：保留
	do("PUT", "/api/keepalive", body(`,"telegram_chat":"42"`))
	if m.cfg.Keepalive.TelegramToken != "123456:secret-token" {
		t.Fatal("token should be kept")
	}
	_, exp := do("GET", "/api/export", "")
	if strings.Contains(exp, "secret-token") || strings.Contains(exp, "keepalive") {
		t.Fatalf("export: %s", exp)
	}
	do("PUT", "/api/keepalive", body(`,"telegram_token":""`))
	if m.cfg.Keepalive.TelegramToken != "" {
		t.Fatal("token should be cleared")
	}

	// 账号：设置设备 ID 后，编辑账号（不带设备 ID）保留原来的
	_, out := do("POST", "/api/sites", `{"name":"A","upstream":"https://a.example.net","mode":"path","path":"a","enabled":true}`)
	var r struct{ ID string }
	json.Unmarshal([]byte(out), &r)
	do("PUT", "/api/sites/"+r.ID+"/account", `{"username":"alice","password":"pw","device_id":"DEV-1"}`)
	do("PUT", "/api/sites/"+r.ID+"/account", `{"username":"alice","password":"pw2"}`)
	ts, _ := m.ka.targets()
	if len(ts) != 1 || ts[0].Acc.DeviceID != "DEV-1" || ts[0].Acc.Password != "pw2" {
		t.Fatalf("device id should be kept: %+v", ts)
	}
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, `"keepalive":{`) || !strings.Contains(st, `"name":"A","on":false`) {
		t.Fatalf("keepalive view: %s", st)
	}

	// 站点开关：打开后状态里 on=true 且有下次执行时间；保存保号设置不影响站点开关；删除站点一起清掉
	if code, _ := do("PUT", "/api/sites/nope/keepalive", `{"enabled":true}`); code != 404 {
		t.Fatalf("unknown site: %d", code)
	}
	m.ka.state[r.ID] = &kaSiteState{AuthFailed: true, LastAttempt: 1}
	if code, out := do("PUT", "/api/sites/"+r.ID+"/keepalive", `{"enabled":true}`); code != 200 || m.ka.state[r.ID].AuthFailed {
		t.Fatalf("enabling the switch should resume: %d %s", code, out)
	}
	m.ka.state[r.ID].AuthFailed = true
	if do("PUT", "/api/sites/"+r.ID+"/account", `{"username":"alice","password":"new"}`); m.ka.state[r.ID].AuthFailed {
		t.Fatal("saving the account should resume")
	}
	do("PUT", "/api/keepalive", body(`,"sites":{}`))
	if !m.cfg.Keepalive.Sites[r.ID] {
		t.Fatal("saving settings should keep site switches")
	}
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, `"name":"A","on":true`) || !strings.Contains(st, `"next":`) {
		t.Fatalf("site on: %s", st)
	}
	do("PUT", "/api/sites/"+r.ID+"/keepalive", `{"enabled":false}`)
	if m.cfg.Keepalive.Sites[r.ID] {
		t.Fatal("site switch off")
	}
	do("PUT", "/api/sites/"+r.ID+"/keepalive", `{"enabled":true}`)
	do("DELETE", "/api/sites/"+r.ID, "")
	if len(m.cfg.Keepalive.Sites) != 0 {
		t.Fatalf("delete site should drop its switch: %v", m.cfg.Keepalive.Sites)
	}

	m.mu.Lock()
	m.cfg.Cluster.Role = "node"
	m.mu.Unlock()
	if code, _ := do("PUT", "/api/keepalive", body("")); code != 409 {
		t.Fatalf("node should reject: %d", code)
	}
	if code, _ := do("PUT", "/api/sites/x/keepalive", `{"enabled":true}`); code != 409 {
		t.Fatalf("node should reject site switch: %d", code)
	}
	if _, st := do("GET", "/api/state", ""); !strings.Contains(st, `"keepalive":null`) {
		t.Fatal("node should not show keepalive")
	}
}
