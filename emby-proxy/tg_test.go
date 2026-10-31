package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeTG 模拟 Telegram：一个签到机器人（收到 /checkin 回复带「签到」按钮的消息，点了回复「签到成功」），
// 以及一个群。
type fakeTG struct {
	mu      sync.Mutex
	msgs    map[string][]tgMsg // chat key → 消息（旧的在前）
	nextID  int
	sent    []string
	clicked []string
	deleted []int
	botSays string // 点按钮后机器人的弹窗
}

func newFakeTG() *fakeTG {
	return &fakeTG{msgs: map[string][]tgMsg{}, nextID: 100, botSays: "签到成功，积分 +10"}
}

func (f *fakeTG) Cached(chat string) *tgPeer { p, _ := f.Resolve(context.Background(), chat); return p }

func (f *fakeTG) Resolve(_ context.Context, chat string) (*tgPeer, error) {
	n := tgChatName(chat)
	if n == "nobody" {
		return nil, errors.New("这个用户名不存在")
	}
	return &tgPeer{Key: "k_" + n, Name: "@" + n}, nil
}

func (f *fakeTG) add(chat string, m tgMsg) tgMsg {
	f.nextID++
	m.ID, m.Chat = f.nextID, chat
	f.msgs[chat] = append(f.msgs[chat], m)
	return m
}

func (f *fakeTG) Send(_ context.Context, p *tgPeer, text string, replyTo int) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, p.Name+":"+text)
	m := f.add(p.Key, tgMsg{Out: true, Text: text})
	if p.Name == "@signbot" && text == "/checkin" {
		f.add(p.Key, tgMsg{Text: "请点击下方按钮签到", Buttons: []tgButton{{Text: "✅ 签到", Data: []byte("sign")}, {Text: "取消", Data: []byte("x")}}})
	}
	return m.ID, nil
}

func (f *fakeTG) Recent(_ context.Context, p *tgPeer, limit int) ([]tgMsg, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ms := f.msgs[p.Key]
	var out []tgMsg
	for i := len(ms) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, ms[i])
	}
	return out, nil
}

func (f *fakeTG) Click(_ context.Context, p *tgPeer, id int, b tgButton) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clicked = append(f.clicked, b.Text)
	return f.botSays, nil
}

func (f *fakeTG) Delete(_ context.Context, p *tgPeer, ids []int) error {
	f.mu.Lock()
	f.deleted = append(f.deleted, ids...)
	f.mu.Unlock()
	return nil
}

func fastTG(t *testing.T) {
	ow, oh := tgWaitFor, tgHumanDelay
	tgWaitFor, tgHumanDelay = 300*time.Millisecond, func() time.Duration { return 0 }
	t.Cleanup(func() { tgWaitFor, tgHumanDelay = ow, oh })
}

func TestTGRunTask(t *testing.T) {
	fastTG(t)
	f := newFakeTG()
	ctx := context.Background()
	ok, msg := tgRunTask(ctx, f, TGTask{Name: "签到", Chat: "@signbot", Text: "/checkin", Button: "签到", Success: "成功,已签到"}, nil)
	if !ok || !strings.Contains(msg, "签到成功") || len(f.clicked) != 1 || f.clicked[0] != "✅ 签到" {
		t.Fatalf("checkin: %v %s %v", ok, msg, f.clicked)
	}
	f.botSays = "今天已经签过了"
	if ok, msg := tgRunTask(ctx, f, TGTask{Chat: "@signbot", Text: "/checkin", Button: "签到", Success: "成功"}, nil); ok || !strings.Contains(msg, "没有「成功」") {
		t.Fatalf("success keyword should fail: %v %s", ok, msg)
	}
	if ok, msg := tgRunTask(ctx, f, TGTask{Chat: "@quietbot", Text: "/checkin", Button: "签到"}, nil); ok || !strings.Contains(msg, "没有回复") {
		t.Fatalf("no reply: %v %s", ok, msg)
	}
	if ok, msg := tgRunTask(ctx, f, TGTask{Chat: "@nobody", Text: "hi"}, nil); ok || !strings.Contains(msg, "不存在") {
		t.Fatalf("resolve error: %v %s", ok, msg)
	}
	// 群里发言、2 秒后删除
	ok, _ = tgRunTask(ctx, f, TGTask{Chat: "@mygroup", Text: "早\n早上好", DeleteAfter: 1}, nil)
	if !ok {
		t.Fatal("group post")
	}
	time.Sleep(1300 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.deleted) != 1 {
		t.Fatalf("message should be deleted: %v", f.deleted)
	}
	if last := f.sent[len(f.sent)-1]; last != "@mygroup:早" && last != "@mygroup:早上好" {
		t.Fatalf("random line: %s", last)
	}
}

func TestTGSchedule(t *testing.T) {
	task := TGTask{ID: "t1", Enabled: true, Time: "08:00-09:00", EveryDays: 1}
	day := time.Date(2026, 11, 1, 0, 0, 0, 0, time.Local)
	p := tgPlan(task, day)
	if p.Hour() != 8 || p != tgPlan(task, day) {
		t.Fatalf("plan should be stable within window: %v", p)
	}
	if tgDue(task, tgTaskState{}, p.Add(-time.Minute)) || !tgDue(task, tgTaskState{}, p.Add(time.Minute)) {
		t.Fatal("due around planned time")
	}
	if tgDue(task, tgTaskState{}, day.Add(23*time.Hour)) {
		t.Fatal("new task created after the window should wait until tomorrow")
	}
	if !tgDue(task, tgTaskState{LastDay: "2026-10-31"}, day.Add(23*time.Hour)) {
		t.Fatal("existing task that missed its window (server was down) should catch up")
	}
	if tgDue(task, tgTaskState{LastDay: "2026-11-01"}, p.Add(time.Hour)) {
		t.Fatal("done today")
	}
	// 失败后 3 分钟内不重试
	st := tgTaskState{TryDay: "2026-11-01", Tries: 1, LastAt: p.Add(time.Minute).Unix()}
	if tgDue(task, st, p.Add(2*time.Minute)) || !tgDue(task, st, p.Add(5*time.Minute)) {
		t.Fatal("retry wait")
	}
	// 每 3 天一次
	task.EveryDays = 3
	if tgDue(task, tgTaskState{LastDay: "2026-10-30"}, tgPlan(task, day).Add(time.Minute)) {
		t.Fatal("every 3 days: too early")
	}
	if !tgDue(task, tgTaskState{LastDay: "2026-10-29"}, tgPlan(task, day).Add(time.Minute)) {
		t.Fatal("every 3 days: due")
	}
	if tgChatName("https://t.me/abc_bot") != "abc_bot" || tgChatName("@x") != "" || tgChatName("@hello123") != "hello123" {
		t.Fatal("chat name")
	}
}

func TestTGModuleRules(t *testing.T) {
	fastTG(t)
	cfg := &Config{TG: &TGConfig{Enabled: true, Accounts: []TGAccount{{ID: "1", Name: "me"}}}}
	cfg.TG.Rules = []TGRule{
		{Name: "抢红包", Enabled: true, Account: "1", Chats: []string{"@group1"}, Click: "🧧"},
		{Name: "抢注", Enabled: true, Account: "1", Chats: []string{"@group1"}, Keywords: "开注,开放注册", Reply: "/register", Cooldown: 60},
	}
	for i := range cfg.TG.Rules {
		if err := cfg.TG.Rules[i].normalize(cfg.TG); err != nil {
			t.Fatal(err)
		}
	}
	m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
	f := newFakeTG()
	var sent []string
	var mu sync.Mutex
	m.lines.notify = func(s string) { mu.Lock(); sent = append(sent, s); mu.Unlock() }
	m.notify("x") // 未配置通知渠道不应出错
	t.Cleanup(func() {})
	tm := m.tg
	key := "k_group1"
	tm.onMessage("1", f, tgMsg{ID: 1, Chat: key, Text: "发红包啦", Buttons: []tgButton{{Text: "🧧 抢", Data: []byte("r")}}})
	tm.onMessage("1", f, tgMsg{ID: 2, Chat: key, Text: "现在开注了！"})
	tm.onMessage("1", f, tgMsg{ID: 3, Chat: key, Text: "又开注了"})          // 冷却中
	tm.onMessage("1", f, tgMsg{ID: 4, Chat: "k_other", Text: "开注"})      // 不是盯着的群
	tm.onMessage("1", f, tgMsg{ID: 5, Chat: key, Text: "开注", Out: true}) // 自己发的
	time.Sleep(300 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.clicked) != 1 || f.clicked[0] != "🧧 抢" {
		t.Fatalf("red packet click: %v", f.clicked)
	}
	if len(f.sent) != 1 || f.sent[0] != "@group1:/register" {
		t.Fatalf("register reply: %v", f.sent)
	}
	tm.mu.Lock()
	n := len(tm.logs)
	tm.mu.Unlock()
	if n != 2 {
		t.Fatalf("logs: %d", n)
	}
}

func TestTGModuleOffRunsNothing(t *testing.T) {
	cfg := &Config{TG: &TGConfig{Enabled: false, Accounts: []TGAccount{{ID: "1", Name: "me"}}}}
	m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
	called := false
	m.tg.connect = func(ctx context.Context, a TGAccount, on func(tgAPI)) error { called = true; <-ctx.Done(); return nil }
	m.tg.writeSession("1", []byte("s"))
	m.tg.sync()
	time.Sleep(50 * time.Millisecond)
	if called || m.tg.loopOn {
		t.Fatal("disabled module must not connect or schedule")
	}
	// 打开：连接、上线；关闭：断开
	m.mu.Lock()
	m.cfg.TG.Enabled = true
	m.mu.Unlock()
	f := newFakeTG()
	ready := make(chan struct{})
	m.tg.connect = func(ctx context.Context, a TGAccount, on func(tgAPI)) error {
		on(f)
		close(ready)
		<-ctx.Done()
		return nil
	}
	m.tg.sync()
	<-ready
	if m.tg.api("1") == nil {
		t.Fatal("should be online")
	}
	m.mu.Lock()
	m.cfg.TG.Enabled = false
	m.mu.Unlock()
	m.tg.sync()
	if m.tg.api("1") != nil || len(m.tg.runners) != 0 {
		t.Fatal("disabling should disconnect")
	}
	// 会话加密：文件里不是明文，能解开
	if data, err := (tgSessionFile{m.tg, "1"}).LoadSession(context.Background()); err != nil || string(data) != "s" {
		t.Fatalf("session roundtrip: %q %v", data, err)
	}
}

func TestTGLoggedOutNotifies(t *testing.T) {
	cfg := &Config{TG: &TGConfig{Enabled: true, Accounts: []TGAccount{{ID: "1", Name: "小明"}}}}
	m := NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
	m.tg.writeSession("1", []byte("s"))
	done := make(chan struct{})
	m.tg.connect = func(ctx context.Context, a TGAccount, on func(tgAPI)) error { defer close(done); return errTGLoggedOut }
	m.tg.sync()
	<-done
	time.Sleep(50 * time.Millisecond)
	m.tg.mu.Lock()
	st := m.tg.status["1"]
	m.tg.mu.Unlock()
	if st == nil || st.State != "logged_out" {
		t.Fatalf("status: %+v", st)
	}
}

func TestTGTaskValidation(t *testing.T) {
	c := &TGConfig{Accounts: []TGAccount{{ID: "1"}}}
	good := TGTask{Account: "1", Chat: "t.me/sign_bot", Text: "/checkin", Time: "08:00 - 09:00"}
	if err := good.normalize(c); err != nil || good.Chat != "@sign_bot" || good.Time != "08:00-09:00" || good.EveryDays != 1 {
		t.Fatalf("%v %+v", err, good)
	}
	for _, bad := range []TGTask{{Account: "2", Chat: "@abcd", Text: "x", Time: "08:00"}, {Account: "1", Chat: "@abcd", Text: "", Time: "08:00"}, {Account: "1", Chat: "@abcd", Text: "x", Time: "8点"}} {
		if bad.normalize(c) == nil {
			t.Fatalf("should reject %+v", bad)
		}
	}
	r := TGRule{Account: "1", Chats: []string{"@grp1"}, Keywords: "hi"}
	if r.normalize(c) == nil {
		t.Fatal("rule without action must be rejected")
	}
}
