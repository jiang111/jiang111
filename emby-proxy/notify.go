package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 通知：保号结果、线路切换 / 故障、自动封禁都通过这里推送。
// 渠道在「保号」页的通知设置里填：Telegram（机器人 Token + Chat ID）、Bark（iPhone 推送）。

var barkAPI = "https://api.day.app"

// barkURL 把用户填的 Bark 地址或 Key 变成推送地址。
func barkURL(v string) (string, error) {
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if !strings.Contains(v, "://") {
		if v == "" || strings.ContainsAny(v, "/?# ") || len(v) > 100 {
			return "", errors.New("Bark 地址不正确，应为 https://api.day.app/你的Key 或直接填 Key")
		}
		return barkAPI + "/" + v, nil
	}
	u, err := url.Parse(v)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(strings.Trim(u.Path, "/")) == 0 {
		return "", errors.New("Bark 地址不正确，应为 https://api.day.app/你的Key 或直接填 Key")
	}
	return v, nil
}

func maskBark(v string) string {
	i := strings.LastIndex(v, "/")
	key := v[i+1:]
	return v[:i+1] + key[:min(4, len(key))] + "…"
}

func barkSend(target, text string) error {
	u, err := barkURL(target)
	if err != nil {
		return err
	}
	title, body, ok := strings.Cut(text, "\n")
	if !ok {
		title, body = "Emby 反代", text
	}
	data, _ := json.Marshal(map[string]string{"title": title, "body": body, "group": "Emby 反代"})
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Post(u, "application/json; charset=utf-8", bytes.NewReader(data))
	if err != nil {
		return errors.New(simplifyNetErr(err))
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != 200 {
		return fmt.Errorf("Bark 返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

// notifyAll 推送到所有配置了的渠道，返回各渠道的错误。
func notifyAll(cfg KeepaliveConfig, text string) []error {
	var errs []error
	if cfg.TelegramToken != "" && cfg.TelegramChat != "" {
		if err := telegramSend(cfg.TelegramToken, cfg.TelegramChat, text); err != nil {
			errs = append(errs, fmt.Errorf("Telegram：%v", err))
		}
	}
	if cfg.Bark != "" {
		if err := barkSend(cfg.Bark, text); err != nil {
			errs = append(errs, fmt.Errorf("Bark：%v", err))
		}
	}
	return errs
}

// notify 后台推送一条通知（线路切换、自动封禁等）。
func (m *Manager) notify(text string) {
	go func() {
		for _, err := range notifyAll(m.keepaliveConfig(), text) {
			log.Printf("通知失败：%v", err)
		}
	}()
}

// testNotify：POST /api/notify/test，发一条测试通知，马上知道填得对不对。
func (a *Admin) testNotify(w http.ResponseWriter, r *http.Request) {
	cfg := a.m.keepaliveConfig()
	if (cfg.TelegramToken == "" || cfg.TelegramChat == "") && cfg.Bark == "" {
		writeErr(w, 400, "还没有填写通知方式，先填好并保存")
		return
	}
	if errs := notifyAll(cfg, "Emby 反代：测试通知\n收到这条说明通知设置正确 ✅"); len(errs) > 0 {
		writeErr(w, 502, errors.Join(errs...).Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// saveNotify：PUT /api/notify，只改通知设置。telegram_token / bark 不传表示不修改，空字符串表示清除。
func (a *Admin) saveNotify(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TelegramToken *string `json:"telegram_token"`
		TelegramChat  string  `json:"telegram_chat"`
		Bark          *string `json:"bark"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	c := a.m.keepaliveConfig()
	c.TelegramChat = body.TelegramChat
	if body.TelegramToken != nil {
		c.TelegramToken = *body.TelegramToken
	}
	if body.Bark != nil {
		c.Bark = *body.Bark
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
