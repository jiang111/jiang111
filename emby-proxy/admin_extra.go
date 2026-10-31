package main

import (
	_ "embed"
	"net/http"
	"time"
)

//go:embed web/icons.json
var iconsJSON []byte // 内置图标库（TFEL Emby 图标，528 个，图片托管在 emby-icon.vercel.app）

func serveIcons(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(iconsJSON)
}

// reorderSites：PUT /api/sites/order {"ids":[...]}，按给出的顺序排列站点（没列出的排在后面，保持原顺序）。
func (a *Admin) reorderSites(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	m := a.m
	m.mu.Lock()
	pos := map[string]int{}
	for i, id := range body.IDs {
		if _, ok := pos[id]; !ok {
			pos[id] = i
		}
	}
	var head, tail []*Site
	head = make([]*Site, len(body.IDs))
	for _, s := range m.cfg.Sites {
		if i, ok := pos[s.ID]; ok {
			head[i] = s
		} else {
			tail = append(tail, s)
		}
	}
	out := tail[:0:0]
	for _, s := range head {
		if s != nil {
			out = append(out, s)
		}
	}
	m.cfg.Sites = append(out, tail...)
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// probeAll：POST /api/lines/probe，立即给所有站点测速（等结果出来再返回）。
func (a *Admin) probeAll(w http.ResponseWriter, r *http.Request) {
	h := &a.m.lines
	h.mu.Lock()
	h.init()
	h.next = map[string]time.Time{}
	h.mu.Unlock()
	a.m.probeDue(time.Now(), true)
	writeJSON(w, 200, map[string]any{"ok": true})
}

// saveIPPref：PUT /api/ip-pref {"ip_pref":"v4"}，全局的连上游 IP 优先（同步给从节点）。
func (a *Admin) saveIPPref(w http.ResponseWriter, r *http.Request) {
	var body struct {
		IPPref string `json:"ip_pref"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	switch body.IPPref {
	case "", "auto":
		body.IPPref = ""
	case "v4", "v6":
	default:
		writeErr(w, 400, "只能是 auto / v4 / v6")
		return
	}
	m := a.m
	m.mu.Lock()
	m.cfg.IPPref = body.IPPref
	m.reload()
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
