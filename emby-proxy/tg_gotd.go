package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
)

// 用 gotd/td 实现 tgAPI：连接、收消息、发消息、点按钮、删除；手机号登录；会话加密保存。

func tgDevice() telegram.DeviceConfig {
	return telegram.DeviceConfig{DeviceModel: "Emby Proxy", SystemVersion: "Linux", AppVersion: version, SystemLangCode: "zh", LangCode: "zh"}
}

// ---- 会话：加密保存在 data/tg/账号ID.session（密钥同站点账号的 account.key） ----

func (t *tgModule) sessionPath(id string) string {
	return filepath.Join(filepath.Dir(t.m.path), "tg", id+".session")
}

func (t *tgModule) hasSession(id string) bool {
	_, err := os.Stat(t.sessionPath(id))
	return err == nil
}

type tgSessionFile struct {
	t  *tgModule
	id string
}

func (s tgSessionFile) LoadSession(context.Context) ([]byte, error) {
	data, err := os.ReadFile(s.t.sessionPath(s.id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, session.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s.t.open(s.id, data)
}

func (s tgSessionFile) StoreSession(_ context.Context, data []byte) error {
	return s.t.writeSession(s.id, data)
}

func (t *tgModule) writeSession(id string, data []byte) error {
	key, err := t.m.accountKey(true)
	if err != nil {
		return err
	}
	gcm, err := accountGCM(key)
	if err != nil {
		return err
	}
	nonce := make([]byte, gcm.NonceSize())
	rand.Read(nonce)
	if err := os.MkdirAll(filepath.Dir(t.sessionPath(id)), 0o700); err != nil {
		return err
	}
	return writeFileAtomic(t.sessionPath(id), gcm.Seal(nonce, nonce, data, []byte("tg|"+id)))
}

func (t *tgModule) open(id string, data []byte) ([]byte, error) {
	key, err := t.m.accountKey(false)
	if err != nil {
		return nil, err
	}
	gcm, err := accountGCM(key)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize() {
		return nil, errors.New("会话文件损坏")
	}
	out, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte("tg|"+id))
	if err != nil {
		return nil, errors.New("会话文件解不开（account.key 变了？），请重新登录")
	}
	return out, nil
}

// ---- 连接 ----

// connectGotd 连接一个账号并保持在线，直到 ctx 结束；授权失效返回 errTGLoggedOut。
func (t *tgModule) connectGotd(ctx context.Context, a TGAccount, onReady func(tgAPI)) error {
	id, hash := t.apiID()
	var api *gotdAPI
	d := tg.NewUpdateDispatcher()
	handle := func(m tg.MessageClass) {
		if msg, ok := m.(*tg.Message); ok && api != nil {
			t.onMessage(a.ID, api, convMsg(msg))
		}
	}
	d.OnNewMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewMessage) error { handle(u.Message); return nil })
	d.OnNewChannelMessage(func(ctx context.Context, e tg.Entities, u *tg.UpdateNewChannelMessage) error {
		handle(u.Message)
		return nil
	})
	// 私聊、普通群的消息常以精简形式推送（不经 dispatcher），单独处理（这种消息不带按钮）
	h := telegram.UpdateHandlerFunc(func(ctx context.Context, u tg.UpdatesClass) error {
		if api != nil {
			switch v := u.(type) {
			case *tg.UpdateShortMessage:
				t.onMessage(a.ID, api, tgMsg{ID: v.ID, Out: v.Out, Text: v.Message, Chat: "u" + strconv.FormatInt(v.UserID, 10)})
				return nil
			case *tg.UpdateShortChatMessage:
				t.onMessage(a.ID, api, tgMsg{ID: v.ID, Out: v.Out, Text: v.Message, Chat: "c" + strconv.FormatInt(v.ChatID, 10)})
				return nil
			}
		}
		return d.Handle(ctx, u)
	})
	client := telegram.NewClient(id, hash, telegram.Options{SessionStorage: tgSessionFile{t, a.ID}, UpdateHandler: h, Device: tgDevice()})
	err := client.Run(ctx, func(ctx context.Context) error {
		st, err := client.Auth().Status(ctx)
		if err != nil {
			return err
		}
		if !st.Authorized {
			return errTGLoggedOut
		}
		api = newGotdAPI(client.API())
		onReady(api)
		<-ctx.Done()
		return ctx.Err()
	})
	if err != nil && (auth.IsUnauthorized(err) || auth.IsKeyUnregistered(err) || strings.Contains(err.Error(), "SESSION_REVOKED") || strings.Contains(err.Error(), "USER_DEACTIVATED")) {
		return errTGLoggedOut
	}
	return err
}

type gotdAPI struct {
	api   *tg.Client
	mu    sync.Mutex
	peers map[string]*tgPeer // @name（小写）→ 解析结果
}

func newGotdAPI(api *tg.Client) *gotdAPI { return &gotdAPI{api: api, peers: map[string]*tgPeer{}} }

type gotdPeer struct {
	in tg.InputPeerClass
	ch *tg.InputChannel // 频道 / 超级群才有
}

func (g *gotdAPI) Cached(chat string) *tgPeer {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.peers[strings.ToLower(tgChatName(chat))]
}

func (g *gotdAPI) Resolve(ctx context.Context, chat string) (*tgPeer, error) {
	name := tgChatName(chat)
	if p := g.Cached(name); p != nil {
		return p, nil
	}
	res, err := g.api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{Username: name})
	if err != nil {
		return nil, tgErr(err)
	}
	p := &tgPeer{Name: "@" + name}
	switch pr := res.Peer.(type) {
	case *tg.PeerUser:
		for _, u := range res.Users {
			if usr, ok := u.(*tg.User); ok && usr.ID == pr.UserID {
				p.Key, p.raw = "u"+strconv.FormatInt(usr.ID, 10), gotdPeer{in: &tg.InputPeerUser{UserID: usr.ID, AccessHash: usr.AccessHash}}
			}
		}
	case *tg.PeerChannel:
		for _, c := range res.Chats {
			if ch, ok := c.(*tg.Channel); ok && ch.ID == pr.ChannelID {
				p.Key = "ch" + strconv.FormatInt(ch.ID, 10)
				p.raw = gotdPeer{in: &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}, ch: &tg.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}}
			}
		}
	case *tg.PeerChat:
		p.Key, p.raw = "c"+strconv.FormatInt(pr.ChatID, 10), gotdPeer{in: &tg.InputPeerChat{ChatID: pr.ChatID}}
	}
	if p.raw == nil {
		return nil, errors.New("解析结果不完整")
	}
	g.mu.Lock()
	g.peers[strings.ToLower(name)] = p
	g.mu.Unlock()
	return p, nil
}

func randID() int64 {
	var b [8]byte
	rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]) >> 1)
}

func (g *gotdAPI) Send(ctx context.Context, p *tgPeer, text string, replyTo int) (int, error) {
	rid := randID()
	req := &tg.MessagesSendMessageRequest{Peer: p.raw.(gotdPeer).in, Message: text, RandomID: rid}
	if replyTo > 0 {
		req.SetReplyTo(&tg.InputReplyToMessage{ReplyToMsgID: replyTo})
	}
	up, err := g.api.MessagesSendMessage(ctx, req)
	if err != nil {
		return 0, tgErr(err)
	}
	return sentID(up, rid), nil
}

// sentID 从发送结果里找出新消息的 ID。
func sentID(up tg.UpdatesClass, rid int64) int {
	var list []tg.UpdateClass
	switch u := up.(type) {
	case *tg.UpdateShortSentMessage:
		return u.ID
	case *tg.Updates:
		list = u.Updates
	case *tg.UpdatesCombined:
		list = u.Updates
	}
	for _, x := range list {
		if v, ok := x.(*tg.UpdateMessageID); ok && v.RandomID == rid {
			return v.ID
		}
	}
	for _, x := range list {
		switch v := x.(type) {
		case *tg.UpdateNewMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				return m.ID
			}
		case *tg.UpdateNewChannelMessage:
			if m, ok := v.Message.(*tg.Message); ok {
				return m.ID
			}
		}
	}
	return 0
}

func peerKey(p tg.PeerClass) string {
	switch v := p.(type) {
	case *tg.PeerUser:
		return "u" + strconv.FormatInt(v.UserID, 10)
	case *tg.PeerChat:
		return "c" + strconv.FormatInt(v.ChatID, 10)
	case *tg.PeerChannel:
		return "ch" + strconv.FormatInt(v.ChannelID, 10)
	}
	return ""
}

func convMsg(m *tg.Message) tgMsg {
	out := tgMsg{ID: m.ID, Out: m.Out, Text: m.Message, Chat: peerKey(m.PeerID)}
	switch rm := m.ReplyMarkup.(type) {
	case *tg.ReplyInlineMarkup:
		for _, row := range rm.Rows {
			for _, b := range row.Buttons {
				if cb, ok := b.(*tg.KeyboardButtonCallback); ok {
					out.Buttons = append(out.Buttons, tgButton{Text: cb.Text, Data: cb.Data})
				}
			}
		}
	case *tg.ReplyKeyboardMarkup: // 底部键盘按钮：「点」= 发送按钮上的文字
		for _, row := range rm.Rows {
			for _, b := range row.Buttons {
				if kb, ok := b.(*tg.KeyboardButton); ok {
					out.Buttons = append(out.Buttons, tgButton{Text: kb.Text})
				}
			}
		}
	}
	return out
}

func (g *gotdAPI) Recent(ctx context.Context, p *tgPeer, limit int) ([]tgMsg, error) {
	res, err := g.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{Peer: p.raw.(gotdPeer).in, Limit: limit})
	if err != nil {
		return nil, tgErr(err)
	}
	var ms []tg.MessageClass
	switch r := res.(type) {
	case *tg.MessagesMessages:
		ms = r.Messages
	case *tg.MessagesMessagesSlice:
		ms = r.Messages
	case *tg.MessagesChannelMessages:
		ms = r.Messages
	}
	var out []tgMsg
	for _, x := range ms {
		if m, ok := x.(*tg.Message); ok {
			out = append(out, convMsg(m))
		}
	}
	return out, nil
}

func (g *gotdAPI) Click(ctx context.Context, p *tgPeer, msgID int, b tgButton) (string, error) {
	if b.Data == nil { // 底部键盘按钮
		_, err := g.Send(ctx, p, b.Text, 0)
		return "", err
	}
	req := &tg.MessagesGetBotCallbackAnswerRequest{Peer: p.raw.(gotdPeer).in, MsgID: msgID}
	req.SetData(b.Data)
	ans, err := g.api.MessagesGetBotCallbackAnswer(ctx, req)
	if err != nil {
		if strings.Contains(err.Error(), "BOT_RESPONSE_TIMEOUT") {
			return "", nil // 机器人没弹提示，不算失败
		}
		return "", tgErr(err)
	}
	return ans.Message, nil
}

func (g *gotdAPI) Delete(ctx context.Context, p *tgPeer, ids []int) error {
	gp := p.raw.(gotdPeer)
	var err error
	if gp.ch != nil {
		_, err = g.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{Channel: gp.ch, ID: ids})
	} else {
		req := &tg.MessagesDeleteMessagesRequest{ID: ids}
		req.SetRevoke(true)
		_, err = g.api.MessagesDeleteMessages(ctx, req)
	}
	return tgErr(err)
}

// tgErr 把常见的 Telegram 错误翻译成人话。
func tgErr(err error) error {
	if err == nil {
		return nil
	}
	s := err.Error()
	for k, v := range map[string]string{
		"USERNAME_NOT_OCCUPIED": "这个用户名不存在", "USERNAME_INVALID": "用户名格式不对",
		"CHAT_WRITE_FORBIDDEN": "没有在这个群发言的权限", "USER_BANNED_IN_CHANNEL": "你在这个群被禁言了",
		"FLOOD_WAIT": "操作太频繁，被 Telegram 限流了，稍后再试", "PEER_FLOOD": "操作太频繁，被 Telegram 限制了",
		"PHONE_CODE_INVALID": "验证码不对", "PHONE_CODE_EXPIRED": "验证码过期了，请重新获取",
		"PASSWORD_HASH_INVALID": "两步验证密码不对", "PHONE_NUMBER_INVALID": "手机号格式不对（要带国家码，如 +886912345678）",
		"PHONE_NUMBER_BANNED": "这个手机号被 Telegram 封了", "API_ID_INVALID": "API ID / Hash 不对",
		"MESSAGE_ID_INVALID": "消息已不存在", "CHANNEL_PRIVATE": "无法访问这个群（没加入或被踢了）",
	} {
		if strings.Contains(s, k) {
			return errors.New(v)
		}
	}
	return err
}

// ---- 手机号登录 ----

type tgLoginStep struct {
	Need string // code | password | ""（完成）
	Err  error
	Acc  *TGAccount
}

type tgLogin struct {
	phone   string
	codeCh  chan string
	pwCh    chan string
	res     chan tgLoginStep
	cancel  context.CancelFunc
	created time.Time
}

func maskPhone(p string) string {
	r := []rune(p)
	if len(r) <= 6 {
		return p
	}
	return string(r[:4]) + strings.Repeat("*", len(r)-7) + string(r[len(r)-3:])
}

// startLogin 发验证码，返回登录 ID。
func (t *tgModule) startLogin(phone string) (string, tgLoginStep) {
	phone = strings.ReplaceAll(strings.TrimSpace(phone), " ", "")
	if !strings.HasPrefix(phone, "+") {
		phone = "+" + phone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	l := &tgLogin{phone: phone, codeCh: make(chan string), pwCh: make(chan string), res: make(chan tgLoginStep, 1), cancel: cancel, created: time.Now()}
	lid := randHex(8)
	t.mu.Lock()
	for k, x := range t.logins { // 清掉过期的
		if time.Since(x.created) > 10*time.Minute {
			x.cancel()
			delete(t.logins, k)
		}
	}
	t.logins[lid] = l
	t.mu.Unlock()
	go t.loginFlow(ctx, l)
	return lid, l.wait()
}

func (l *tgLogin) wait() tgLoginStep {
	select {
	case s := <-l.res:
		return s
	case <-time.After(60 * time.Second):
		return tgLoginStep{Err: errors.New("连接 Telegram 超时，请稍后再试")}
	}
}

func (t *tgModule) loginByID(id string) *tgLogin {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.logins[id]
}

func (t *tgModule) loginInput(id, kind, v string) tgLoginStep {
	l := t.loginByID(id)
	if l == nil {
		return tgLoginStep{Err: errors.New("登录已过期，请重新开始")}
	}
	ch := l.codeCh
	if kind == "password" {
		ch = l.pwCh
	}
	select {
	case ch <- strings.TrimSpace(v):
	case <-time.After(5 * time.Second):
		return tgLoginStep{Err: errors.New("现在不需要输入这个，请重新开始登录")}
	}
	s := l.wait()
	if s.Need == "" { // 完成或出错，这次登录结束
		t.mu.Lock()
		delete(t.logins, id)
		t.mu.Unlock()
	}
	return s
}

func (t *tgModule) loginFlow(ctx context.Context, l *tgLogin) {
	defer l.cancel()
	id, hash := t.apiID()
	mem := &session.StorageMemory{}
	client := telegram.NewClient(id, hash, telegram.Options{SessionStorage: mem, Device: tgDevice(), NoUpdates: true})
	err := client.Run(ctx, func(ctx context.Context) error {
		sent, err := client.Auth().SendCode(ctx, l.phone, auth.SendCodeOptions{})
		if err != nil {
			return tgErr(err)
		}
		code, ok := sent.(*tg.AuthSentCode)
		if !ok {
			return errors.New("Telegram 返回了意外的结果，请重试")
		}
		l.res <- tgLoginStep{Need: "code"}
		for {
			var c string
			select {
			case c = <-l.codeCh:
			case <-ctx.Done():
				return ctx.Err()
			}
			_, err := client.Auth().SignIn(ctx, l.phone, c, code.PhoneCodeHash)
			if errors.Is(err, auth.ErrPasswordAuthNeeded) {
				l.res <- tgLoginStep{Need: "password"}
				for {
					var pw string
					select {
					case pw = <-l.pwCh:
					case <-ctx.Done():
						return ctx.Err()
					}
					if _, err = client.Auth().Password(ctx, pw); err == nil {
						break
					}
					l.res <- tgLoginStep{Need: "password", Err: tgErr(err)}
				}
			} else if err != nil {
				l.res <- tgLoginStep{Need: "code", Err: tgErr(err)}
				continue
			}
			break
		}
		self, err := client.Self(ctx)
		if err != nil {
			return tgErr(err)
		}
		data, err := mem.Bytes(nil)
		if err != nil {
			return err
		}
		name := strings.TrimSpace(self.FirstName + " " + self.LastName)
		if self.Username != "" {
			name += " @" + self.Username
		}
		acc := TGAccount{ID: strconv.FormatInt(self.ID, 10), Name: strings.TrimSpace(name), Phone: maskPhone(l.phone)}
		if err := t.writeSession(acc.ID, data); err != nil {
			return err
		}
		if err := t.addAccount(acc); err != nil {
			return err
		}
		l.res <- tgLoginStep{Acc: &acc}
		return nil
	})
	if err != nil {
		select {
		case l.res <- tgLoginStep{Err: err}:
		default:
		}
	}
}

// addAccount 登录成功：加入账号列表（同一个账号重新登录则覆盖），并立即连上。
func (t *tgModule) addAccount(acc TGAccount) error {
	m := t.m
	m.mu.Lock()
	if m.cfg.TG == nil {
		m.cfg.TG = &TGConfig{}
	}
	c := m.cfg.TG
	found := false
	for i := range c.Accounts {
		if c.Accounts[i].ID == acc.ID {
			c.Accounts[i], found = acc, true
		}
	}
	if !found {
		c.Accounts = append(c.Accounts, acc)
	}
	m.mu.Unlock()
	t.mu.Lock()
	if cancel := t.runners[acc.ID]; cancel != nil { // 重新登录：断开旧连接，用新会话重连
		cancel()
		delete(t.runners, acc.ID)
	}
	delete(t.status, acc.ID)
	t.mu.Unlock()
	if err := m.save(); err != nil {
		return err
	}
	t.sync()
	t.logf(true, "账号「%s」已登录", acc.Name)
	return nil
}

// removeAccount 删除账号：断开连接、删除会话文件（Telegram「设置 → 设备」里那一项也建议移除）。
func (t *tgModule) removeAccount(id string) error {
	m := t.m
	m.mu.Lock()
	if c := m.cfg.TG; c != nil {
		var keep []TGAccount
		for _, a := range c.Accounts {
			if a.ID != id {
				keep = append(keep, a)
			}
		}
		c.Accounts = keep
	}
	m.mu.Unlock()
	t.mu.Lock()
	if cancel := t.runners[id]; cancel != nil {
		cancel()
		delete(t.runners, id)
	}
	delete(t.apis, id)
	delete(t.status, id)
	t.mu.Unlock()
	if err := os.Remove(t.sessionPath(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("删除会话失败: %v", err)
	}
	return m.save()
}
