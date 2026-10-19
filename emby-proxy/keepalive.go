package main

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 自动保号：定期用站点账号登录上游 Emby，随机挑一部电影 / 剧集模拟播放几分钟，在服务端留下观看记录，
// 避免账号长期不用被清理。逻辑照搬 emby-keepalive/keepalive.py：
//   登录（伪装 SenPlayer）→ 随机选片 → 上报开始播放 → 每隔几秒上报进度（同时限速拉流）→ 上报停止 → 退出登录。
// 服务器地址用站点的客户端访问地址（和播放器一样经过反代，上游看到的仍是负责节点的 IP）；
// 站点停用或没有客户端访问地址时直接连上游。
// 调度按站点：上次成功日期 + N 天的设定时刻执行；失败的过几小时只重跑它自己。同一批到期的并行执行，
// 开始时间错开几秒，结束后推送一条 Telegram 汇总。
// 只在主节点 / 单机运行（账号只在这里），设置和状态都不同步给从节点、不随导出。

const ticksPerSecond = 10_000_000

type KeepaliveClient struct {
	Name      string `json:"name"`
	Version   string `json:"version"`
	Device    string `json:"device"`
	UserAgent string `json:"user_agent"`
}

type KeepaliveConfig struct {
	Enabled        bool            `json:"enabled"`
	IntervalDays   int             `json:"interval_days"`   // 每几天一次
	RunAt          string          `json:"run_at"`          // 几点执行，HH:MM（容器时区）
	WatchMin       int             `json:"watch_min"`       // 每次观看时长范围（分钟）
	WatchMax       int             `json:"watch_max"`       //
	ReportInterval int             `json:"report_interval"` // 进度上报间隔（秒）
	StreamKbps     int             `json:"stream_kbps"`     // 同时拉流的速度，0 = 不拉流
	Retries        int             `json:"retries"`         // 每个账号最多试几次（401/403 不重试）
	RetryHours     int             `json:"retry_hours"`     // 失败后几小时再试
	Client         KeepaliveClient `json:"client"`          // 伪装的客户端身份
	TelegramToken  string          `json:"telegram_token,omitempty"`
	TelegramChat   string          `json:"telegram_chat,omitempty"`
}

func defaultKeepalive() KeepaliveConfig {
	return KeepaliveConfig{
		IntervalDays: 3, RunAt: "04:00", WatchMin: 6, WatchMax: 9, ReportInterval: 10, StreamKbps: 500,
		Retries: 3, RetryHours: 2,
		Client: KeepaliveClient{Name: "SenPlayer", Version: "6.2.1", Device: "Mac", UserAgent: "SenPlayer/6.2.1"},
	}
}

var runAtRe = regexp.MustCompile(`^([01]\d|2[0-3]):[0-5]\d$`)

func (c *KeepaliveConfig) validate() error {
	c.RunAt = strings.TrimSpace(c.RunAt)
	switch {
	case c.IntervalDays < 1 || c.IntervalDays > 365:
		return errors.New("间隔天数要在 1～365 之间")
	case !runAtRe.MatchString(c.RunAt):
		return errors.New("执行时间格式为 HH:MM，例如 04:00")
	case c.WatchMin < 1 || c.WatchMax < c.WatchMin || c.WatchMax > 180:
		return errors.New("观看时长要在 1～180 分钟之间，且最短不大于最长")
	case c.ReportInterval < 1 || c.ReportInterval > 300:
		return errors.New("进度上报间隔要在 1～300 秒之间")
	case c.StreamKbps < 0 || c.StreamKbps > 100000:
		return errors.New("拉流速度要在 0～100000 kbps 之间")
	case c.Retries < 1 || c.Retries > 10:
		return errors.New("重试次数要在 1～10 之间")
	case c.RetryHours < 1 || c.RetryHours > 72:
		return errors.New("失败后再试的间隔要在 1～72 小时之间")
	}
	for _, f := range []*string{&c.Client.Name, &c.Client.Version, &c.Client.Device, &c.Client.UserAgent} {
		*f = strings.TrimSpace(*f)
		if *f == "" || len(*f) > 200 || strings.ContainsAny(*f, "\"\r\n") {
			return errors.New("客户端身份的四项都要填写，且不能包含引号和换行")
		}
	}
	c.TelegramToken, c.TelegramChat = strings.TrimSpace(c.TelegramToken), strings.TrimSpace(c.TelegramChat)
	if len(c.TelegramToken) > 200 || len(c.TelegramChat) > 100 || strings.ContainsAny(c.TelegramToken, "/?# ") {
		return errors.New("Telegram 设置不正确")
	}
	return nil
}

// ---- 状态 ----

type kaSiteState struct {
	LastSuccess string `json:"last_success,omitempty"` // 上次成功的日期 2006-01-02
	LastAttempt int64  `json:"last_attempt,omitempty"` // 开始执行或失败的时间，成功后清零（失败后据此推迟重试）
	LastTime    int64  `json:"last_time,omitempty"`    // 最近一次结束的时间
	LastOK      bool   `json:"last_ok"`
	LastResult  string `json:"last_result,omitempty"`
}

type keepalive struct {
	m       *Manager
	mu      sync.Mutex
	state   map[string]*kaSiteState
	running map[string]bool
	logs    []string
	sec     time.Duration // 一「秒」的长度，测试时调小
	gap     func() time.Duration
}

func newKeepalive(m *Manager) *keepalive {
	k := &keepalive{m: m, state: map[string]*kaSiteState{}, running: map[string]bool{}, sec: time.Second,
		gap: func() time.Duration { return time.Duration(3000+rand.IntN(12000)) * time.Millisecond }}
	if data, err := os.ReadFile(k.path()); err == nil {
		json.Unmarshal(data, &k.state)
	}
	return k
}

func (k *keepalive) path() string { return filepath.Join(filepath.Dir(k.m.path), "keepalive.json") }

// saveLocked 调用方持有 k.mu。
func (k *keepalive) saveLocked() {
	data, _ := json.MarshalIndent(k.state, "", "  ")
	if err := writeFileAtomic(k.path(), data); err != nil {
		log.Printf("保号：保存状态失败: %v", err)
	}
}

func (k *keepalive) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Printf("保号：%s", msg)
	k.mu.Lock()
	k.logs = append(k.logs, time.Now().Format("01-02 15:04:05")+" "+msg)
	if len(k.logs) > 50 {
		k.logs = k.logs[len(k.logs)-50:]
	}
	k.mu.Unlock()
}

// keepaliveConfig 返回保号设置；config.json 里不完整（手改过）的项用默认值，后台循环不会因此出错。
func (m *Manager) keepaliveConfig() KeepaliveConfig {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d := defaultKeepalive()
	if m.cfg.Keepalive == nil {
		return d
	}
	c := *m.cfg.Keepalive
	if !runAtRe.MatchString(c.RunAt) {
		c.RunAt = d.RunAt
	}
	for _, p := range []struct{ v, def *int }{{&c.IntervalDays, &d.IntervalDays}, {&c.WatchMin, &d.WatchMin},
		{&c.ReportInterval, &d.ReportInterval}, {&c.Retries, &d.Retries}, {&c.RetryHours, &d.RetryHours}} {
		if *p.v < 1 {
			*p.v = *p.def
		}
	}
	c.WatchMax = max(c.WatchMax, c.WatchMin)
	c.StreamKbps = max(c.StreamKbps, 0)
	if c.Client.Name == "" || c.Client.Version == "" || c.Client.Device == "" || c.Client.UserAgent == "" {
		c.Client = d.Client
	}
	return c
}

// kaNext：上次成功日期 + N 天的设定时刻；从没成功过的是今天的设定时刻；失败后不早于失败时间 + 几小时。
func kaNext(cfg KeepaliveConfig, st *kaSiteState, now time.Time) time.Time {
	hh, _ := strconv.Atoi(cfg.RunAt[:2])
	mm, _ := strconv.Atoi(cfg.RunAt[3:])
	day := now
	if st != nil && st.LastSuccess != "" {
		if d, err := time.ParseInLocation(dayLayout, st.LastSuccess, now.Location()); err == nil {
			day = d.AddDate(0, 0, cfg.IntervalDays)
		}
	}
	next := time.Date(day.Year(), day.Month(), day.Day(), hh, mm, 0, 0, now.Location())
	if st != nil && st.LastAttempt > 0 {
		if retry := time.Unix(st.LastAttempt, 0).Add(time.Duration(cfg.RetryHours) * time.Hour); retry.After(next) {
			next = retry
		}
	}
	return next
}

// ---- 要保号的站点 ----

type kaTarget struct {
	ID, Name, Base string
	Acc            siteAccount
	Transport      http.RoundTripper
}

// targets 返回存了账号的站点（解不开的账号带 err）。
func (k *keepalive) targets() ([]kaTarget, map[string]string) {
	m := k.m
	key, keyErr := m.accountKey(false)
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []kaTarget
	bad := map[string]string{}
	for _, s := range m.cfg.Sites {
		enc := m.cfg.Accounts[s.ID]
		if enc == "" {
			continue
		}
		if keyErr != nil {
			bad[s.ID] = keyErr.Error()
			continue
		}
		acc, err := openAccount(key, s.ID, enc)
		if err != nil {
			bad[s.ID] = err.Error()
			continue
		}
		if acc.Username == "" {
			continue
		}
		if acc.DeviceID == "" {
			acc.DeviceID = uuid5URL(s.Upstream + acc.Username)
		}
		t := kaTarget{ID: s.ID, Name: s.Name, Acc: acc}
		if s.Enabled && s.PublicURL != "" {
			t.Base, t.Transport = s.PublicURL, m.trs[tlsVerify]
		} else {
			t.Base, t.Transport = s.Upstream, m.transport(s.InsecureTLS, s.IgnoreExpired)
		}
		out = append(out, t)
	}
	return out, bad
}

// uuid5URL 和 Python 的 str(uuid.uuid5(uuid.NAMESPACE_URL, name)).upper() 相同（沿用 emby-keepalive 的设备 ID）。
func uuid5URL(name string) string {
	ns := []byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}
	h := sha1.Sum(append(ns, name...))
	u := h[:16]
	u[6] = u[6]&0x0f | 0x50
	u[8] = u[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:16]))
}

// ---- 调度 ----

func (k *keepalive) loop() {
	for now := range time.Tick(30 * time.Second) {
		k.tick(now)
	}
}

func (k *keepalive) tick(now time.Time) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("保号：调度出错: %v", r)
		}
	}()
	if due, cfg := k.due(now); len(due) > 0 {
		go k.runBatch(due, cfg, false)
	}
}

// due 返回到期的站点（开关打开、不是从节点、没在执行）。
func (k *keepalive) due(now time.Time) ([]kaTarget, KeepaliveConfig) {
	cfg := k.m.keepaliveConfig()
	if k.m.isNode() || !cfg.Enabled {
		return nil, cfg
	}
	ts, _ := k.targets()
	var due []kaTarget
	k.mu.Lock()
	for _, t := range ts {
		if !k.running[t.ID] && !now.Before(kaNext(cfg, k.state[t.ID], now)) {
			due = append(due, t)
		}
	}
	k.mu.Unlock()
	return due, cfg
}

// runNow 是面板的「立即执行」：所有存了账号的站点跑一轮（开关没开也可以）。返回启动的站点数。
func (k *keepalive) runNow() (int, error) {
	ts, _ := k.targets()
	if len(ts) == 0 {
		return 0, errors.New("还没有站点保存了账号")
	}
	k.mu.Lock()
	for _, r := range k.running {
		if r {
			k.mu.Unlock()
			return 0, errors.New("保号正在执行，请等它结束")
		}
	}
	k.mu.Unlock()
	go k.runBatch(ts, k.m.keepaliveConfig(), true)
	return len(ts), nil
}

type kaResult struct {
	ok  bool
	msg string
}

func (k *keepalive) runBatch(ts []kaTarget, cfg KeepaliveConfig, manual bool) {
	k.mu.Lock()
	var run []kaTarget
	for _, t := range ts {
		if !k.running[t.ID] {
			k.running[t.ID] = true
			run = append(run, t)
			st := k.state[t.ID]
			if st == nil {
				st = &kaSiteState{}
				k.state[t.ID] = st
			}
			st.LastAttempt = time.Now().Unix() // 执行中重启了，过几小时再试，不会立即重跑
		}
	}
	k.saveLocked()
	k.mu.Unlock()
	if len(run) == 0 {
		return
	}
	how := "到期"
	if manual {
		how = "手动"
	}
	k.logf("开始%s保号：%d 个账号", how, len(run))
	res := make([]kaResult, len(run))
	var wg sync.WaitGroup
	for i, t := range run {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res[i] = k.runSite(t, cfg)
			k.mu.Lock()
			st := k.state[t.ID]
			st.LastTime, st.LastOK, st.LastResult = time.Now().Unix(), res[i].ok, res[i].msg
			if res[i].ok {
				st.LastSuccess, st.LastAttempt = time.Now().Format(dayLayout), 0
			} else {
				st.LastAttempt = time.Now().Unix()
			}
			k.running[t.ID] = false
			k.saveLocked()
			k.mu.Unlock()
		}()
		if i < len(run)-1 {
			time.Sleep(k.gap())
		}
	}
	wg.Wait()
	allOK := true
	lines := []string{}
	for i, r := range res {
		allOK = allOK && r.ok
		mark := "✅ "
		if !r.ok {
			mark = "❌ "
		}
		lines = append(lines, mark+run[i].Name+"："+r.msg)
	}
	title := "Emby 保号全部成功 ✅"
	if !allOK {
		title = fmt.Sprintf("Emby 保号有失败 ❌（%d 小时后再试失败的）", cfg.RetryHours)
	}
	summary := title + "\n" + strings.Join(lines, "\n")
	k.logf("%s", strings.ReplaceAll(summary, "\n", " | "))
	if cfg.TelegramToken != "" && cfg.TelegramChat != "" {
		if err := telegramSend(cfg.TelegramToken, cfg.TelegramChat, summary); err != nil {
			k.logf("Telegram 通知失败：%v", err)
		}
	}
}

// runSite 对一个站点保号，失败按设置重试（账号被拒绝 401/403 不重试，避免触发登录限流）。
func (k *keepalive) runSite(t kaTarget, cfg KeepaliveConfig) (res kaResult) {
	defer func() {
		if r := recover(); r != nil {
			res = kaResult{false, fmt.Sprintf("失败 - 程序出错: %v", r)}
		}
	}()
	var err error
	for attempt := 1; attempt <= cfg.Retries; attempt++ {
		var msg string
		if msg, err = k.watch(t, cfg); err == nil {
			k.logf("[%s] ✅ 完成，%s", t.Name, msg)
			return kaResult{true, msg}
		}
		var he *embyHTTPError
		if errors.As(err, &he) && (he.Code == 401 || he.Code == 403) {
			k.logf("[%s] ❌ 被拒绝，不再重试：%v", t.Name, err)
			break
		}
		k.logf("[%s] ❌ 第 %d 次失败：%v", t.Name, attempt, err)
		if attempt < cfg.Retries {
			time.Sleep(time.Duration(60*attempt) * k.sec)
		}
	}
	return kaResult{false, "失败 - " + err.Error()}
}

// ---- Emby 客户端 ----

type embyHTTPError struct {
	Code   int
	Detail string
}

func (e *embyHTTPError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("HTTP %d %s", e.Code, e.Detail)
	}
	return fmt.Sprintf("HTTP %d", e.Code)
}

type embyClient struct {
	base, deviceID string
	cl             KeepaliveClient
	hc             *http.Client
	token, userID  string
}

func newEmbyClient(t kaTarget, cl KeepaliveClient) *embyClient {
	base := strings.TrimRight(t.Base, "/")
	if !strings.HasSuffix(strings.ToLower(base), "/emby") {
		base += "/emby" // 和官方 / SenPlayer 客户端一样走 /emby 前缀
	}
	return &embyClient{base: base, deviceID: t.Acc.DeviceID, cl: cl, hc: &http.Client{Transport: t.Transport}}
}

func (c *embyClient) authHeader() string {
	h := "MediaBrowser "
	if c.userID != "" {
		h += fmt.Sprintf(`UserId="%s", `, c.userID)
	}
	h += fmt.Sprintf(`Client="%s", Device="%s", DeviceId="%s", Version="%s"`, c.cl.Name, c.cl.Device, c.deviceID, c.cl.Version)
	if c.token != "" {
		h += fmt.Sprintf(`, Token="%s"`, c.token)
	}
	return h
}

func (c *embyClient) newReq(ctx context.Context, method, path string, q url.Values, body any) (*http.Request, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Emby-Authorization", c.authHeader())
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.cl.UserAgent)
	if c.token != "" {
		req.Header.Set("X-Emby-Token", c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return req, nil
}

// call 发 API 请求（30 秒超时），out 非空时解析 JSON。
func (c *embyClient) call(ctx context.Context, method, path string, q url.Values, body, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := c.newReq(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return errors.New(simplifyNetErr(err))
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if resp.StatusCode >= 400 {
		d := strings.TrimSpace(string(data))
		if len(d) > 200 {
			d = d[:200]
		}
		return &embyHTTPError{resp.StatusCode, d}
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s 返回的不是 JSON", path)
		}
	}
	return nil
}

func (c *embyClient) login(ctx context.Context, user, pw string) error {
	var r struct {
		AccessToken string
		User        struct{ Id string }
	}
	if err := c.call(ctx, "POST", "/Users/AuthenticateByName", nil, map[string]string{"Username": user, "Pw": pw}, &r); err != nil {
		return err
	}
	if r.AccessToken == "" || r.User.Id == "" {
		return errors.New("登录返回里没有令牌")
	}
	c.token, c.userID = r.AccessToken, r.User.Id
	return nil
}

func (c *embyClient) logout() {
	c.call(context.Background(), "POST", "/Sessions/Logout", nil, nil, nil)
}

type embyItem struct {
	Id, Name, SeriesName string
	RunTimeTicks         int64
}

func (c *embyClient) pickItem(ctx context.Context, minSeconds int) (embyItem, error) {
	q := url.Values{"IncludeItemTypes": {"Movie,Episode"}, "Recursive": {"true"}, "SortBy": {"Random"},
		"Limit": {"30"}, "Fields": {"RunTimeTicks,MediaSources"}}
	var r struct{ Items []embyItem }
	if err := c.call(ctx, "GET", "/Users/"+c.userID+"/Items", q, nil, &r); err != nil {
		return embyItem{}, err
	}
	var long []embyItem
	for _, it := range r.Items {
		if it.RunTimeTicks >= int64(minSeconds)*ticksPerSecond {
			long = append(long, it)
		}
	}
	if len(long) == 0 {
		long = r.Items
	}
	if len(long) == 0 {
		return embyItem{}, errors.New("媒体库里没有找到电影 / 剧集")
	}
	return long[rand.IntN(len(long))], nil
}

// playbackInfo 返回播放会话、媒体源和容器格式；失败时用默认值继续（和原脚本一样）。
func (c *embyClient) playbackInfo(ctx context.Context, id string) (session, source, container string) {
	session, source, container = randHex(16), id, "mkv"
	var r struct {
		PlaySessionId string
		MediaSources  []struct{ Id, Container string }
	}
	q := url.Values{"UserId": {c.userID}, "StartTimeTicks": {"0"}, "IsPlayback": {"true"},
		"AutoOpenLiveStream": {"true"}, "MaxStreamingBitrate": {"140000000"}}
	if err := c.call(ctx, "POST", "/Items/"+id+"/PlaybackInfo", q, map[string]any{"DeviceProfile": map[string]any{}}, &r); err != nil {
		return
	}
	if r.PlaySessionId != "" {
		session = r.PlaySessionId
	}
	if len(r.MediaSources) > 0 {
		if r.MediaSources[0].Id != "" {
			source = r.MediaSources[0].Id
		}
		if ct, _, _ := strings.Cut(r.MediaSources[0].Container, ","); ct != "" {
			container = ct
		}
	}
	return
}

// stream 模拟边播边下载，直到 ctx 结束，返回拉取的字节数。每 10「秒」用 Range 请求一小段（kbps × 10 秒的量），
// 和播放器断点续传一样，而不是慢慢读一条长连接：服务器之间的 TCP 缓冲区很大时，上游几秒就把几十 MB 塞进缓冲区，
// 之后 nginx 超过 60 秒（send_timeout）写不出去就断开，上游据此判定播放已停止（有的服务器随后让播放会话失效）。
func (c *embyClient) stream(ctx context.Context, id, source, session, container string, kbps int, sec time.Duration) (int64, error) {
	q := url.Values{"DeviceId": {c.deviceID}, "MediaSourceId": {source}, "PlaySessionId": {session},
		"api_key": {c.token}, "X-Emby-Token": {c.token}}
	chunk := max(int64(kbps)*1024/8*10, 64<<10)
	var off, total int64
	fails := 0
	for {
		n, err := c.streamChunk(ctx, "/videos/"+id+"/original."+container, q, off, chunk)
		if ctx.Err() != nil {
			return total, nil
		}
		total += n
		switch {
		case err != nil:
			if fails++; fails >= 3 {
				return total, err
			}
		case n < chunk:
			off, fails = 0, 0 // 到文件尾（或服务器忽略了 Range），从头再来
		default:
			off, fails = off+n, 0
		}
		select {
		case <-ctx.Done():
			return total, nil
		case <-time.After(10 * sec):
		}
	}
}

// streamChunk 请求 [off, off+n) 这一段并读完；416（超出文件长度）当作读到文件尾。
func (c *embyClient) streamChunk(ctx context.Context, path string, q url.Values, off, n int64) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	req, err := c.newReq(ctx, "GET", path, q, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+n-1))
	resp, err := c.hc.Do(req)
	if err != nil {
		return 0, errors.New(simplifyNetErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		return 0, nil
	}
	if resp.StatusCode >= 400 {
		return 0, &embyHTTPError{resp.StatusCode, ""}
	}
	got, err := io.Copy(io.Discard, io.LimitReader(resp.Body, n))
	if err != nil && got == 0 {
		return 0, errors.New(simplifyNetErr(err))
	}
	return got, nil
}

// watch 模拟一次播放，返回「《片名》N 分钟」。
func (k *keepalive) watch(t kaTarget, cfg KeepaliveConfig) (string, error) {
	ctx := context.Background()
	c := newEmbyClient(t, cfg.Client)
	seconds := cfg.WatchMin*60 + rand.IntN((cfg.WatchMax-cfg.WatchMin)*60+1)
	k.logf("[%s] 登录", t.Name)
	if err := c.login(ctx, t.Acc.Username, t.Acc.Password); err != nil {
		return "", fmt.Errorf("登录失败：%w", err)
	}
	defer c.logout()
	item, err := c.pickItem(ctx, seconds+60)
	if err != nil {
		return "", err
	}
	title := item.Name
	if item.SeriesName != "" {
		title = item.SeriesName + " - " + title
	}
	total := int(item.RunTimeTicks / ticksPerSecond)
	start := 0
	if total > seconds+120 {
		start = rand.IntN(min(total-seconds-60, 1800) + 1)
	}
	session, source, container := c.playbackInfo(ctx, item.Id)
	k.logf("[%s] 开始播放《%s》，从 %d 分开始，计划看 %d 分 %d 秒", t.Name, title, start/60, seconds/60, seconds%60)
	base := map[string]any{"ItemId": item.Id, "MediaSourceId": source, "PlaySessionId": session, "PlayMethod": "DirectPlay",
		"CanSeek": true, "IsPaused": false, "IsMuted": false, "VolumeLevel": 100, "PlaybackRate": 1, "RepeatMode": "RepeatNone"}
	body := func(pos int, extra ...string) map[string]any {
		b := map[string]any{"PositionTicks": int64(pos) * ticksPerSecond}
		for key, v := range base {
			b[key] = v
		}
		if len(extra) > 0 {
			b["EventName"] = extra[0]
		}
		return b
	}

	sctx, stop := context.WithCancel(ctx)
	var sw sync.WaitGroup
	var pulled int64
	if cfg.StreamKbps > 0 {
		sw.Add(1)
		go func() {
			defer sw.Done()
			var err error
			if pulled, err = c.stream(sctx, item.Id, source, session, container, cfg.StreamKbps, k.sec); err != nil {
				k.logf("[%s]   拉流失败（不影响播放上报）：%v", t.Name, err)
			}
		}()
	}
	pos := start
	defer func() {
		stop()
		sw.Wait()
	}()
	if err := c.call(ctx, "POST", "/Sessions/Playing", nil, body(start), nil); err != nil {
		return "", fmt.Errorf("上报开始播放失败：%w", err)
	}
	began := time.Now()
	every := time.Duration(cfg.ReportInterval) * k.sec
	end := began.Add(time.Duration(seconds) * k.sec)
	for time.Now().Before(end) {
		time.Sleep(min(every, time.Until(end)))
		pos = start + int(time.Since(began)/k.sec)
		if err := c.call(ctx, "POST", "/Sessions/Playing/Progress", nil, body(pos, "timeupdate"), nil); err != nil {
			k.logf("[%s]   进度上报失败（继续）：%v", t.Name, err)
		}
	}
	stop()
	sw.Wait()
	if err := c.call(ctx, "POST", "/Sessions/Playing/Stopped", nil, body(pos), nil); err != nil {
		return "", fmt.Errorf("上报停止播放失败：%w", err)
	}
	msg := fmt.Sprintf("《%s》%d 分钟", title, (pos-start)/60)
	if cfg.StreamKbps > 0 {
		msg += fmt.Sprintf("，拉流 %.1f MB", float64(pulled)/(1<<20))
	}
	return msg, nil
}

var telegramAPI = "https://api.telegram.org" // 测试时替换

func telegramSend(token, chat, text string) error {
	resp, err := (&http.Client{Timeout: 20 * time.Second}).PostForm(telegramAPI+"/bot"+token+"/sendMessage",
		url.Values{"chat_id": {chat}, "text": {text}})
	if err != nil {
		return errors.New(simplifyNetErr(err))
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---- 面板 ----

type kaSiteView struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	LastSuccess string `json:"last_success,omitempty"`
	Next        int64  `json:"next,omitempty"` // 开关打开时的下次执行时间
	Running     bool   `json:"running"`
	LastOK      bool   `json:"last_ok"`
	LastTime    int64  `json:"last_time,omitempty"`
	LastResult  string `json:"last_result,omitempty"`
	Error       string `json:"error,omitempty"` // 账号解不开
}

type kaView struct {
	Config KeepaliveConfig `json:"config"` // Telegram 令牌只显示前 4 位
	Sites  []kaSiteView    `json:"sites"`
	Log    []string        `json:"log"`
}

// view 不能在持有 m.mu 时调用（targets 要拿 m.mu）。
func (k *keepalive) view(now time.Time) *kaView {
	cfg := k.m.keepaliveConfig()
	ts, bad := k.targets()
	if cfg.TelegramToken != "" {
		cfg.TelegramToken = cfg.TelegramToken[:min(4, len(cfg.TelegramToken))] + "…"
	}
	// 按站点顺序列出存了账号的站点
	type row struct{ id, name, err string }
	var rows []row
	k.m.mu.RLock()
	ok := map[string]bool{}
	for _, t := range ts {
		ok[t.ID] = true
	}
	for _, s := range k.m.cfg.Sites {
		if e, isBad := bad[s.ID]; ok[s.ID] || isBad {
			rows = append(rows, row{s.ID, s.Name, e})
		}
	}
	k.m.mu.RUnlock()

	v := &kaView{Config: cfg, Sites: []kaSiteView{}}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, r := range rows {
		sv := kaSiteView{ID: r.id, Name: r.name, Running: k.running[r.id], Error: r.err}
		if st := k.state[r.id]; st != nil {
			sv.LastSuccess, sv.LastOK, sv.LastTime, sv.LastResult = st.LastSuccess, st.LastOK, st.LastTime, st.LastResult
		}
		if cfg.Enabled && r.err == "" {
			sv.Next = kaNext(cfg, k.state[r.id], now).Unix()
		}
		v.Sites = append(v.Sites, sv)
	}
	v.Log = append([]string{}, k.logs...)
	return v
}

// saveKeepalive：PUT /api/keepalive。telegram_token 不传表示不修改，空字符串表示清除。
func (a *Admin) saveKeepalive(w http.ResponseWriter, r *http.Request) {
	var body struct {
		KeepaliveConfig
		TelegramToken *string `json:"telegram_token"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	c := body.KeepaliveConfig
	old := a.m.keepaliveConfig()
	c.TelegramToken = old.TelegramToken
	if body.TelegramToken != nil {
		c.TelegramToken = *body.TelegramToken
	}
	if err := c.validate(); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.m.mu.Lock()
	a.m.cfg.Keepalive = &c
	a.m.mu.Unlock()
	if err := a.m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) runKeepalive(w http.ResponseWriter, r *http.Request) {
	n, err := a.m.ka.runNow()
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "count": n})
}
