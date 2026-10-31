package main

import (
	"net/http"
	"slices"
	"strings"
	"time"
)

type tgTaskView struct {
	TGTask
	State   tgTaskState `json:"state"`
	Next    int64       `json:"next,omitempty"`
	Running bool        `json:"running,omitempty"`
}

type tgAccountView struct {
	TGAccount
	tgAcctStatus
}

func (a *Admin) tgState(w http.ResponseWriter, r *http.Request) {
	m, t := a.m, a.m.tg
	m.mu.RLock()
	c := TGConfig{}
	if m.cfg.TG != nil {
		c = *m.cfg.TG
	}
	m.mu.RUnlock()
	now := time.Now()
	t.mu.Lock()
	accs := []tgAccountView{}
	for _, acc := range c.Accounts {
		st := tgAcctStatus{State: "offline"}
		if s := t.status[acc.ID]; s != nil {
			st = *s
		}
		if !c.Enabled {
			st = tgAcctStatus{State: "off"}
		}
		accs = append(accs, tgAccountView{acc, st})
	}
	tasks := []tgTaskView{}
	for _, task := range c.Tasks {
		v := tgTaskView{TGTask: task, Running: t.running[task.ID]}
		if s := t.states[task.ID]; s != nil {
			v.State = *s
		}
		if task.Enabled && c.Enabled {
			if n := tgNext(task, v.State, now); !n.IsZero() {
				v.Next = n.Unix()
			}
		}
		tasks = append(tasks, v)
	}
	logs := make([]tgLog, len(t.logs))
	for i, l := range t.logs {
		logs[len(t.logs)-1-i] = l
	}
	t.mu.Unlock()
	if len(logs) > 100 {
		logs = logs[:100]
	}
	rules := c.Rules
	if rules == nil {
		rules = []TGRule{}
	}
	writeJSON(w, 200, map[string]any{
		"enabled": c.Enabled, "api_id": c.APIID, "custom_api": c.APIID > 0 && c.APIHash != "",
		"accounts": accs, "tasks": tasks, "rules": rules, "logs": logs,
	})
}

func (a *Admin) tgSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled bool   `json:"enabled"`
		APIID   int    `json:"api_id"`
		APIHash string `json:"api_hash"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	body.APIHash = strings.TrimSpace(body.APIHash)
	a.m.mu.RLock()
	if old := a.m.cfg.TG; old != nil && body.APIHash == "" && body.APIID > 0 && body.APIID == old.APIID {
		body.APIHash = old.APIHash // 只开关、没重新填 Hash：保留原来的
	}
	a.m.mu.RUnlock()
	if (body.APIID > 0) != (body.APIHash != "") {
		writeErr(w, 400, "API ID 和 API Hash 要一起填（或都留空用内置的）")
		return
	}
	m := a.m
	m.mu.Lock()
	if m.cfg.TG == nil {
		m.cfg.TG = &TGConfig{}
	}
	changedAPI := m.cfg.TG.APIID != body.APIID || m.cfg.TG.APIHash != body.APIHash
	m.cfg.TG.Enabled, m.cfg.TG.APIID, m.cfg.TG.APIHash = body.Enabled, body.APIID, body.APIHash
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if changedAPI { // 换了 API ID：已有连接断开重连
		m.tg.mu.Lock()
		for id, cancel := range m.tg.runners {
			cancel()
			delete(m.tg.runners, id)
		}
		m.tg.mu.Unlock()
	}
	m.tg.sync()
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) tgSaveTasks(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Tasks []TGTask `json:"tasks"`
		Rules []TGRule `json:"rules"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	m := a.m
	m.mu.Lock()
	if m.cfg.TG == nil {
		m.cfg.TG = &TGConfig{}
	}
	c := m.cfg.TG
	ids := map[string]bool{}
	for i := range body.Tasks {
		if err := body.Tasks[i].normalize(c); err != nil {
			m.mu.Unlock()
			writeErr(w, 400, err.Error())
			return
		}
		ids[body.Tasks[i].ID] = true
	}
	for i := range body.Rules {
		if err := body.Rules[i].normalize(c); err != nil {
			m.mu.Unlock()
			writeErr(w, 400, err.Error())
			return
		}
	}
	if r.URL.Query().Get("kind") != "rules" {
		c.Tasks = body.Tasks
	}
	if r.URL.Query().Get("kind") != "tasks" {
		c.Rules = body.Rules
	}
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	// 删掉的任务清掉状态；规则里新加的群要解析
	t := m.tg
	t.mu.Lock()
	for id := range t.states {
		if !ids[id] && r.URL.Query().Get("kind") != "rules" {
			delete(t.states, id)
		}
	}
	t.saveLocked()
	apis := map[string]tgAPI{}
	for k, v := range t.apis {
		apis[k] = v
	}
	t.mu.Unlock()
	for acc, api := range apis {
		go t.resolveRules(r.Context(), acc, api)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) tgRunTask(w http.ResponseWriter, r *http.Request) {
	c := a.m.tg.cfg()
	if c == nil {
		writeErr(w, 400, "先打开 Telegram 助手")
		return
	}
	i := slices.IndexFunc(c.Tasks, func(t TGTask) bool { return t.ID == r.PathValue("id") })
	if i < 0 {
		writeErr(w, 404, "任务不存在，先保存")
		return
	}
	t := a.m.tg
	t.mu.Lock()
	if t.running[c.Tasks[i].ID] {
		t.mu.Unlock()
		writeErr(w, 400, "正在执行中")
		return
	}
	t.running[c.Tasks[i].ID] = true
	t.mu.Unlock()
	ok, msg := t.runTask(c.Tasks[i], true)
	writeJSON(w, 200, map[string]any{"ok": ok, "msg": msg})
}

func (a *Admin) tgLogin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID, Phone, Code, Password string
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if a.m.tg.cfg() == nil {
		writeErr(w, 400, "先打开 Telegram 助手")
		return
	}
	var id string
	var s tgLoginStep
	switch {
	case body.Password != "":
		id, s = body.ID, a.m.tg.loginInput(body.ID, "password", body.Password)
	case body.Code != "":
		id, s = body.ID, a.m.tg.loginInput(body.ID, "code", body.Code)
	case body.Phone != "":
		id, s = a.m.tg.startLogin(body.Phone)
	default:
		writeErr(w, 400, "请填写手机号")
		return
	}
	out := map[string]any{"id": id, "need": s.Need}
	if s.Err != nil {
		out["error"] = s.Err.Error()
	}
	if s.Acc != nil {
		out["account"] = s.Acc
	}
	writeJSON(w, 200, out)
}

func (a *Admin) tgDeleteAccount(w http.ResponseWriter, r *http.Request) {
	if err := a.m.tg.removeAccount(r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) tgClearLogs(w http.ResponseWriter, r *http.Request) {
	t := a.m.tg
	t.mu.Lock()
	t.logs = nil
	t.saveLocked()
	t.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true})
}
