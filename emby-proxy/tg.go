package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand/v2"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Telegram 助手（可选模块，默认关闭；关闭时不连接 Telegram、不启动任何后台任务，不影响反代）。
// 用你自己的 Telegram 账号（不是机器人）自动做两类事：
//   - 定时发送：每天在设定的时间段里随机挑一个时刻，给机器人 / 群发一条消息。
//     可选：点对方回复里的按钮（如「签到」）、回复里含某些字才算成功、发出后过几秒自动删除。
//     机器人签到、群里定时发言保号都是它。
//   - 关键词触发：盯着几个群 / 机器人，有人发了含关键词的消息，就自动回复，或者点那条消息上的按钮。
//     自动回复、抢红包（点 🧧 按钮）、抢注（看到「开注」就发注册命令）都是它。
// 账号掉线（在别的设备上被移除、改了密码）会推送通知。结果都记在日志里，失败也会推送。
// 只在主节点 / 单机运行，不同步、不导出。Telegram 协议用 gotd/td 实现（见 tg_gotd.go）。

const (
	tgDefaultAPIID   = 2040 // Telegram 官方桌面版公开的参数（和对方面板一样），可在设置里换成自己在 my.telegram.org 申请的
	tgDefaultAPIHash = "b18441a1ff607e10a989891a5462e627"
)

type TGConfig struct {
	Enabled  bool        `json:"enabled"`
	APIID    int         `json:"api_id,omitempty"`
	APIHash  string      `json:"api_hash,omitempty"`
	Accounts []TGAccount `json:"accounts,omitempty"`
	Tasks    []TGTask    `json:"tasks,omitempty"`
	Rules    []TGRule    `json:"rules,omitempty"`
}

type TGAccount struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Phone string `json:"phone"` // 打码后的手机号
}

// TGTask 定时发送。
type TGTask struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Enabled     bool   `json:"enabled"`
	Account     string `json:"account"`
	Chat        string `json:"chat"`                   // @用户名 或 t.me 链接
	Text        string `json:"text"`                   // 发送内容，多行时每次随机发一行
	Button      string `json:"button,omitempty"`       // 点对方回复里含这个字的按钮
	Success     string `json:"success,omitempty"`      // 回复里含这些字（逗号分隔，任一）才算成功
	Time        string `json:"time"`                   // 「08:00-09:30」时间段里随机，或「08:00」
	EveryDays   int    `json:"every_days,omitempty"`   // 每几天一次，默认 1
	DeleteAfter int    `json:"delete_after,omitempty"` // 发出后几秒删除自己的消息，0 = 不删
	NotifyOK    bool   `json:"notify_ok,omitempty"`    // 成功也推送（失败总是推送）
}

// TGRule 关键词触发。
type TGRule struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Account  string   `json:"account"`
	Chats    []string `json:"chats"`              // 盯着哪些群 / 机器人
	Keywords string   `json:"keywords,omitempty"` // 逗号分隔，任一命中；留空 = 只看按钮
	Reply    string   `json:"reply,omitempty"`    // 自动回复（多行随机一行）
	Click    string   `json:"click,omitempty"`    // 点消息上含这个字的按钮，如 🧧
	Cooldown int      `json:"cooldown,omitempty"` // 同一个群里两次触发至少隔几秒，默认 30
	Notify   bool     `json:"notify,omitempty"`   // 触发时推送
}

var (
	tgTimeRe  = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d(-([01]\d|2[0-3]):[0-5]\d)?$`)
	tgChatRe  = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_]{3,31}$`)
	tgSplitRe = regexp.MustCompile(`[,，]`)
)

// tgChatName 把 @name、name、https://t.me/name 统一成 name；不合法返回空。
func tgChatName(s string) string {
	s = strings.TrimSpace(s)
	for _, p := range []string{"https://t.me/", "http://t.me/", "t.me/", "@"} {
		s = strings.TrimPrefix(s, p)
	}
	s, _, _ = strings.Cut(s, "/")
	s, _, _ = strings.Cut(s, "?")
	if !tgChatRe.MatchString(s) {
		return ""
	}
	return s
}

func tgKeywords(s string) []string {
	var out []string
	for _, k := range tgSplitRe.Split(s, -1) {
		if k = strings.TrimSpace(k); k != "" {
			out = append(out, strings.ToLower(k))
		}
	}
	return out
}

func tgContainsAny(text string, keys []string) bool {
	t := strings.ToLower(text)
	for _, k := range keys {
		if strings.Contains(t, k) {
			return true
		}
	}
	return false
}

// tgPick 多行时随机挑一行。
func tgPick(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return lines[rand.IntN(len(lines))]
}

func (c *TGConfig) hasAccount(id string) bool {
	for _, a := range c.Accounts {
		if a.ID == id {
			return true
		}
	}
	return false
}

func (t *TGTask) normalize(c *TGConfig) error {
	t.Name, t.Text, t.Button, t.Success = strings.TrimSpace(t.Name), strings.TrimSpace(t.Text), strings.TrimSpace(t.Button), strings.TrimSpace(t.Success)
	t.Time = strings.ReplaceAll(strings.TrimSpace(t.Time), " ", "")
	if t.ID == "" {
		t.ID = randHex(4)
	}
	if !c.hasAccount(t.Account) {
		return errors.New("请选择账号")
	}
	if tgChatName(t.Chat) == "" {
		return fmt.Errorf("「%s」发给谁：填机器人或群的 @用户名（如 @xxx_bot）", t.Name)
	}
	t.Chat = "@" + tgChatName(t.Chat)
	if t.Text == "" {
		return fmt.Errorf("「%s」要发送的内容不能为空", t.Name)
	}
	if !tgTimeRe.MatchString(t.Time) {
		return fmt.Errorf("「%s」时间格式应为 08:00 或 08:00-09:30", t.Name)
	}
	if t.EveryDays <= 0 {
		t.EveryDays = 1
	}
	if t.EveryDays > 365 || t.DeleteAfter < 0 || t.DeleteAfter > 86400 {
		return fmt.Errorf("「%s」间隔天数或删除秒数超出范围", t.Name)
	}
	if t.Name == "" {
		t.Name = t.Chat
	}
	return nil
}

func (r *TGRule) normalize(c *TGConfig) error {
	r.Name, r.Keywords, r.Reply, r.Click = strings.TrimSpace(r.Name), strings.TrimSpace(r.Keywords), strings.TrimSpace(r.Reply), strings.TrimSpace(r.Click)
	if r.ID == "" {
		r.ID = randHex(4)
	}
	if !c.hasAccount(r.Account) {
		return errors.New("请选择账号")
	}
	var chats []string
	for _, ch := range r.Chats {
		if strings.TrimSpace(ch) == "" {
			continue
		}
		n := tgChatName(ch)
		if n == "" {
			return fmt.Errorf("「%s」群 / 机器人「%s」应为 @用户名", r.Name, ch)
		}
		chats = append(chats, "@"+n)
	}
	if len(chats) == 0 {
		return fmt.Errorf("「%s」至少填一个要盯着的群或机器人", r.Name)
	}
	r.Chats = chats
	if r.Reply == "" && r.Click == "" {
		return fmt.Errorf("「%s」要么填自动回复的内容，要么填要点的按钮", r.Name)
	}
	if r.Keywords == "" && r.Click == "" {
		return fmt.Errorf("「%s」请填关键词", r.Name)
	}
	if r.Cooldown <= 0 {
		r.Cooldown = 30
	}
	if r.Name == "" {
		r.Name = strings.Join(r.Chats, " ")
	}
	return nil
}

// ---- 调度 ----

// tgPlan 返回任务在某天的执行时刻：时间段里按「任务 ID + 日期」算出的固定随机时刻（重启后不变）。
func tgPlan(t TGTask, day time.Time) time.Time {
	from, to, _ := strings.Cut(t.Time, "-")
	at := func(hm string) time.Time {
		h, _ := strconv.Atoi(hm[:2])
		m, _ := strconv.Atoi(hm[3:])
		return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
	}
	start := at(from)
	if to == "" {
		return start
	}
	end := at(to)
	if !end.After(start) {
		return start
	}
	f := fnv.New64a()
	f.Write([]byte(t.ID + dayOf(day)))
	return start.Add(time.Duration(f.Sum64() % uint64(end.Sub(start))))
}

// tgWindowEnd 返回某天时间段的结束时刻（只写一个时间时就是那个时间）。
func tgWindowEnd(t TGTask, day time.Time) time.Time {
	from, to, _ := strings.Cut(t.Time, "-")
	if to == "" {
		to = from
	}
	h, _ := strconv.Atoi(to[:2])
	m, _ := strconv.Atoi(to[3:])
	return time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, day.Location())
}

type tgTaskState struct {
	LastDay string `json:"last_day,omitempty"` // 最后一次成功（或用完重试次数）的日期
	LastAt  int64  `json:"last_at,omitempty"`
	LastOK  bool   `json:"last_ok"`
	LastMsg string `json:"last_msg,omitempty"`
	TryDay  string `json:"try_day,omitempty"`
	Tries   int    `json:"tries,omitempty"`
}

const (
	tgMaxTries  = 3
	tgRetryWait = 3 * time.Minute
)

// tgDue 判断任务现在该不该执行。
func tgDue(t TGTask, st tgTaskState, now time.Time) bool {
	if !t.Enabled {
		return false
	}
	today := dayOf(now)
	if st.LastDay == today {
		return false
	}
	if st.LastDay != "" {
		last, err := time.ParseInLocation("2006-01-02", st.LastDay, now.Location())
		if err == nil && now.Sub(last) < time.Duration(t.EveryDays)*24*time.Hour-12*time.Hour {
			return false
		}
	}
	if now.Before(tgPlan(t, now)) {
		return false
	}
	// 新建的任务：今天的时间段已经过了（超过半小时）就从明天开始，不会一保存就马上发
	if st.LastDay == "" && st.TryDay == "" && now.After(tgWindowEnd(t, now).Add(30*time.Minute)) {
		return false
	}
	if st.TryDay == today && st.Tries > 0 && now.Sub(time.Unix(st.LastAt, 0)) < tgRetryWait {
		return false
	}
	return true
}

// tgNext 返回任务下一次执行的时间（只用于面板显示）。
func tgNext(t TGTask, st tgTaskState, now time.Time) time.Time {
	for d := 0; d < 400; d++ {
		at := tgPlan(t, now.AddDate(0, 0, d))
		if at.Before(now) {
			at = now
		}
		if tgDue(t, st, at.Add(time.Second)) {
			return at
		}
	}
	return time.Time{}
}

// ---- 运行时 ----

type tgLog struct {
	Time int64  `json:"t"`
	OK   bool   `json:"ok"`
	Text string `json:"text"`
}

type tgAcctStatus struct {
	State string `json:"state"` // connecting | online | offline | logged_out
	Err   string `json:"err,omitempty"`
}

type tgModule struct {
	m *Manager

	mu       sync.Mutex
	states   map[string]*tgTaskState
	running  map[string]bool
	logs     []tgLog
	status   map[string]*tgAcctStatus
	apis     map[string]tgAPI // 已连上的账号
	runners  map[string]context.CancelFunc
	cooldown map[string]time.Time
	logins   map[string]*tgLogin
	loopOn   bool

	// 测试可替换：连接一个账号（阻塞到 ctx 结束或出错）
	connect func(ctx context.Context, acc TGAccount, onReady func(tgAPI)) error
}

func newTGModule(m *Manager) *tgModule {
	t := &tgModule{m: m, states: map[string]*tgTaskState{}, running: map[string]bool{}, status: map[string]*tgAcctStatus{},
		apis: map[string]tgAPI{}, runners: map[string]context.CancelFunc{}, cooldown: map[string]time.Time{}, logins: map[string]*tgLogin{}}
	t.connect = t.connectGotd
	t.load()
	return t
}

func (t *tgModule) statePath() string { return filepath.Join(filepath.Dir(t.m.path), "tg_state.json") }

func (t *tgModule) load() {
	data, err := os.ReadFile(t.statePath())
	if err != nil {
		return
	}
	var saved struct {
		States map[string]*tgTaskState `json:"states"`
		Logs   []tgLog                 `json:"logs"`
	}
	if json.Unmarshal(data, &saved) == nil {
		if saved.States != nil {
			t.states = saved.States
		}
		t.logs = saved.Logs
	}
}

// saveLocked 调用方需持有 t.mu。
func (t *tgModule) saveLocked() {
	data, _ := json.Marshal(map[string]any{"states": t.states, "logs": t.logs})
	writeFileAtomic(t.statePath(), data)
}

func (t *tgModule) logf(ok bool, format string, args ...any) {
	t.mu.Lock()
	t.logs = append(t.logs, tgLog{time.Now().Unix(), ok, fmt.Sprintf(format, args...)})
	if len(t.logs) > 300 {
		t.logs = t.logs[len(t.logs)-300:]
	}
	t.saveLocked()
	t.mu.Unlock()
}

// cfg 返回 Telegram 助手配置的副本；本机是从节点或没开启时返回 nil。
func (t *tgModule) cfg() *TGConfig {
	m := t.m
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cfg.TG == nil || !m.cfg.TG.Enabled || m.cfg.Cluster.Role == "node" {
		return nil
	}
	c := *m.cfg.TG
	c.Accounts = append([]TGAccount(nil), c.Accounts...)
	c.Tasks = append([]TGTask(nil), c.Tasks...)
	c.Rules = append([]TGRule(nil), c.Rules...)
	return &c
}

func (t *tgModule) apiID() (int, string) {
	m := t.m
	m.mu.RLock()
	defer m.mu.RUnlock()
	if c := m.cfg.TG; c != nil && c.APIID > 0 && c.APIHash != "" {
		return c.APIID, c.APIHash
	}
	return tgDefaultAPIID, tgDefaultAPIHash
}

// sync 按配置启动 / 停止各账号的连接和调度循环。配置变化后调用；关闭时全部停掉。
func (t *tgModule) sync() {
	c := t.cfg()
	want := map[string]TGAccount{}
	if c != nil {
		for _, a := range c.Accounts {
			if t.hasSession(a.ID) {
				want[a.ID] = a
			}
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, cancel := range t.runners {
		if _, ok := want[id]; !ok {
			cancel()
			delete(t.runners, id)
			delete(t.apis, id)
			delete(t.status, id)
		}
	}
	for id, a := range want {
		if t.runners[id] != nil {
			continue
		}
		if st := t.status[id]; st != nil && st.State == "logged_out" {
			continue // 掉线了，等重新登录
		}
		ctx, cancel := context.WithCancel(context.Background())
		t.runners[id] = cancel
		t.status[id] = &tgAcctStatus{State: "connecting"}
		go t.runAccount(ctx, a)
	}
	if c != nil && !t.loopOn {
		t.loopOn = true
		go t.loop()
	}
}

var errTGLoggedOut = errors.New("账号已掉线（在别的设备上被移除，或改了密码），请重新登录")

// runAccount 保持一个账号在线，断了 30 秒后重连；掉线（授权失效）则停下等重新登录。
func (t *tgModule) runAccount(ctx context.Context, a TGAccount) {
	for ctx.Err() == nil {
		err := t.connect(ctx, a, func(api tgAPI) {
			t.mu.Lock()
			t.apis[a.ID] = api
			t.status[a.ID] = &tgAcctStatus{State: "online"}
			t.mu.Unlock()
			t.resolveRules(ctx, a.ID, api)
		})
		t.mu.Lock()
		delete(t.apis, a.ID)
		if ctx.Err() != nil {
			t.mu.Unlock()
			return
		}
		if errors.Is(err, errTGLoggedOut) {
			t.status[a.ID] = &tgAcctStatus{State: "logged_out", Err: err.Error()}
			delete(t.runners, a.ID)
			t.mu.Unlock()
			t.logf(false, "%s：%v", a.Name, err)
			t.m.notify("Telegram 助手：账号「" + a.Name + "」已掉线，请到面板重新登录")
			return
		}
		msg := "连接断开"
		if err != nil {
			msg = err.Error()
		}
		t.status[a.ID] = &tgAcctStatus{State: "offline", Err: msg}
		t.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(30 * time.Second):
		}
	}
}

func (t *tgModule) api(id string) tgAPI {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.apis[id]
}

// loop 每 20 秒检查到期的定时发送；模块关闭后自己退出。
func (t *tgModule) loop() {
	for {
		time.Sleep(20 * time.Second)
		c := t.cfg()
		if c == nil {
			t.mu.Lock()
			t.loopOn = false
			t.mu.Unlock()
			return
		}
		t.tick(c, time.Now())
	}
}

func (t *tgModule) tick(c *TGConfig, now time.Time) {
	for _, task := range c.Tasks {
		t.mu.Lock()
		st := t.states[task.ID]
		if st == nil {
			st = &tgTaskState{}
		}
		due := !t.running[task.ID] && tgDue(task, *st, now) && t.apis[task.Account] != nil
		if due {
			t.running[task.ID] = true
		}
		t.mu.Unlock()
		if due {
			go t.runTask(task, false)
		}
	}
}

// runTask 执行一次定时发送并记录结果。manual = 面板上点了「立即执行」（不影响当天的定时）。
func (t *tgModule) runTask(task TGTask, manual bool) (bool, string) {
	defer func() {
		t.mu.Lock()
		t.running[task.ID] = false
		t.mu.Unlock()
	}()
	api := t.api(task.Account)
	ok, msg := false, "账号没有连上 Telegram"
	if api != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		ok, msg = tgRunTask(ctx, api, task, t.m)
		cancel()
	}
	now := time.Now()
	t.mu.Lock()
	st := t.states[task.ID]
	if st == nil {
		st = &tgTaskState{}
		t.states[task.ID] = st
	}
	today := dayOf(now)
	st.LastAt, st.LastOK, st.LastMsg = now.Unix(), ok, msg
	if !manual {
		if st.TryDay != today {
			st.TryDay, st.Tries = today, 0
		}
		st.Tries++
		if ok || st.Tries >= tgMaxTries {
			st.LastDay = today
		}
	} else if ok && st.LastDay != today {
		st.LastDay = today // 手动执行成功了，今天就不再自动执行
	}
	tries := st.Tries
	t.saveLocked()
	t.mu.Unlock()
	how := ""
	if manual {
		how = "（手动）"
	}
	t.logf(ok, "%s%s：%s", task.Name, how, msg)
	switch {
	case ok && task.NotifyOK:
		t.m.notify("Telegram 助手 ✅ " + task.Name + "\n" + msg)
	case !ok && (manual || tries >= tgMaxTries):
		t.m.notify("Telegram 助手 ❌ " + task.Name + "\n" + msg)
	}
	return ok, msg
}

// tgRunTask 发送消息 → （可选）等回复、点按钮 → 判断成功 → （可选）过几秒删除。
func tgRunTask(ctx context.Context, api tgAPI, task TGTask, m *Manager) (bool, string) {
	p, err := api.Resolve(ctx, task.Chat)
	if err != nil {
		return false, "找不到 " + task.Chat + "：" + err.Error()
	}
	before := 0
	if ms, err := api.Recent(ctx, p, 1); err == nil && len(ms) > 0 {
		before = ms[0].ID
	}
	id, err := api.Send(ctx, p, tgPick(task.Text), 0)
	if err != nil {
		return false, "发送失败：" + err.Error()
	}
	if task.DeleteAfter > 0 {
		go func() {
			time.Sleep(time.Duration(task.DeleteAfter) * time.Second)
			dctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := api.Delete(dctx, p, []int{id}); err != nil && m != nil && m.tg != nil {
				m.tg.logf(false, "%s：删除消息失败：%v", task.Name, err)
			}
		}()
	}
	if task.Button == "" && task.Success == "" {
		return true, "已发送"
	}
	// 等对方回复（机器人一般几秒内回复）
	var got []string
	reply, ok := tgWait(ctx, api, p, max(before, id), func(ms tgMsg) bool {
		return task.Button == "" || ms.button(task.Button) != nil
	})
	if !ok {
		if task.Button != "" {
			return false, "对方没有回复带「" + task.Button + "」按钮的消息"
		}
		return false, "对方没有回复"
	}
	got = append(got, reply.Text)
	if task.Button != "" {
		b := reply.button(task.Button)
		ans, err := api.Click(ctx, p, reply.ID, *b)
		if err != nil {
			return false, "点「" + b.Text + "」失败：" + err.Error()
		}
		if ans != "" {
			got = append(got, ans)
		}
		// 点完按钮机器人可能再发一条或改原消息，稍等再看一眼
		sleepCtx(ctx, 3*time.Second)
		if ms, err := api.Recent(ctx, p, 5); err == nil {
			for _, x := range ms {
				if !x.Out && x.ID >= reply.ID && x.Text != reply.Text {
					got = append(got, x.Text)
				}
			}
		}
	}
	result := strings.TrimSpace(strings.Join(got, " | "))
	short := []rune(strings.Join(strings.Fields(result), " "))
	if len(short) > 80 {
		short = append(short[:80], '…')
	}
	if task.Success != "" && !tgContainsAny(result, tgKeywords(task.Success)) {
		return false, "回复里没有「" + task.Success + "」：" + string(short)
	}
	return true, string(short)
}

var tgWaitFor = 20 * time.Second // 测试可调短

// tgWait 等对方在 after 之后发来一条满足条件的消息。
func tgWait(ctx context.Context, api tgAPI, p *tgPeer, after int, ok func(tgMsg) bool) (tgMsg, bool) {
	deadline := time.Now().Add(tgWaitFor)
	for time.Now().Before(deadline) && ctx.Err() == nil {
		sleepCtx(ctx, tgWaitFor/10)
		ms, err := api.Recent(ctx, p, 5)
		if err != nil {
			continue
		}
		for _, x := range ms { // 新的在前
			if !x.Out && x.ID > after && ok(x) {
				return x, true
			}
		}
	}
	return tgMsg{}, false
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// ---- 关键词触发 ----

// resolveRules 账号连上后，把它的规则里的群 / 机器人解析成 ID（收到消息时按 ID 匹配）。
func (t *tgModule) resolveRules(ctx context.Context, acc string, api tgAPI) {
	c := t.cfg()
	if c == nil {
		return
	}
	for _, r := range c.Rules {
		if r.Account != acc || !r.Enabled {
			continue
		}
		for _, ch := range r.Chats {
			if _, err := api.Resolve(ctx, ch); err != nil {
				t.logf(false, "%s：找不到 %s：%v", r.Name, ch, err)
			}
		}
	}
}

// onMessage 收到新消息时调用（各账号的连接里）。
func (t *tgModule) onMessage(acc string, api tgAPI, msg tgMsg) {
	if msg.Out {
		return
	}
	c := t.cfg()
	if c == nil {
		return
	}
	for _, r := range c.Rules {
		if !r.Enabled || r.Account != acc {
			continue
		}
		var p *tgPeer
		for _, ch := range r.Chats {
			if cp := api.Cached(ch); cp != nil && cp.Key == msg.Chat {
				p = cp
			}
		}
		if p == nil {
			continue
		}
		keys := tgKeywords(r.Keywords)
		var btn *tgButton
		if r.Click != "" {
			btn = msg.button(r.Click)
		}
		switch {
		case len(keys) > 0 && !tgContainsAny(msg.Text, keys):
			continue
		case len(keys) == 0 && btn == nil: // 只看按钮的规则（抢红包）：消息上没有这个按钮
			continue
		}
		ck := r.ID + "|" + msg.Chat
		t.mu.Lock()
		if time.Since(t.cooldown[ck]) < time.Duration(r.Cooldown)*time.Second {
			t.mu.Unlock()
			continue
		}
		t.cooldown[ck] = time.Now()
		t.mu.Unlock()
		go t.fire(r, api, p, msg, btn)
	}
}

var tgHumanDelay = func() time.Duration { return time.Duration(800+rand.IntN(1700)) * time.Millisecond }

// fire 执行规则：随机等一两秒（像真人），点按钮、回复。
func (t *tgModule) fire(r TGRule, api tgAPI, p *tgPeer, msg tgMsg, btn *tgButton) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	sleepCtx(ctx, tgHumanDelay())
	var done []string
	ok := true
	if btn != nil {
		ans, err := api.Click(ctx, p, msg.ID, *btn)
		if err != nil {
			ok = false
			done = append(done, "点「"+btn.Text+"」失败："+err.Error())
		} else {
			done = append(done, strings.TrimSpace("点了「"+btn.Text+"」 "+ans))
		}
	} else if r.Click != "" && r.Reply == "" {
		return
	}
	if r.Reply != "" {
		if _, err := api.Send(ctx, p, tgPick(r.Reply), msg.ID); err != nil {
			ok = false
			done = append(done, "回复失败："+err.Error())
		} else {
			done = append(done, "已回复")
		}
	}
	text := r.Name + "：" + strings.Join(done, "，")
	t.logf(ok, "%s", text)
	if r.Notify || !ok {
		t.m.notify("Telegram 助手 " + map[bool]string{true: "✅ ", false: "❌ "}[ok] + text)
	}
}

// ---- 和 Telegram 打交道的接口（真实实现在 tg_gotd.go，测试用假的） ----

type tgPeer struct {
	Key  string // u123 / c123 / ch123，和 tgMsg.Chat 对应
	Name string
	raw  any // 实现自用（InputPeer）
}

type tgButton struct {
	Text string
	Data []byte
}

type tgMsg struct {
	ID      int
	Chat    string
	Out     bool
	Text    string
	Buttons []tgButton
}

func (m tgMsg) button(sub string) *tgButton {
	sub = strings.ToLower(strings.TrimSpace(sub))
	for i := range m.Buttons {
		if strings.Contains(strings.ToLower(m.Buttons[i].Text), sub) {
			return &m.Buttons[i]
		}
	}
	return nil
}

type tgAPI interface {
	Resolve(ctx context.Context, chat string) (*tgPeer, error)
	Cached(chat string) *tgPeer // 已解析过的（不发请求）
	Send(ctx context.Context, p *tgPeer, text string, replyTo int) (int, error)
	Recent(ctx context.Context, p *tgPeer, limit int) ([]tgMsg, error) // 新的在前
	Click(ctx context.Context, p *tgPeer, msgID int, b tgButton) (string, error)
	Delete(ctx context.Context, p *tgPeer, ids []int) error
}
