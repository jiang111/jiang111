package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// 导入导出：导出站点和设置（不含面板密码、签名密钥、监听端口），可导入到另一台或重装后的面板。
// 导入也接受服务器上的 config.json 原文件（同样忽略密码、密钥和端口）。

type exportSettings struct {
	EntryURL   string `json:"entry_url,omitempty"`
	PublicHost string `json:"public_host,omitempty"`
	PortBind   string `json:"port_bind,omitempty"`
	NginxCert  string `json:"nginx_cert,omitempty"`
	NginxKey   string `json:"nginx_key,omitempty"`
	NginxIPv6  bool   `json:"nginx_ipv6,omitempty"`
}

type exportFile struct {
	App      string          `json:"app"`
	Version  int             `json:"version"`
	Exported string          `json:"exported_at"`
	Settings *exportSettings `json:"settings,omitempty"`
	Sites    []*Site         `json:"sites"`
}

func (a *Admin) exportConfig(w http.ResponseWriter, r *http.Request) {
	m := a.m
	m.mu.RLock()
	out := exportFile{
		App: "emby-proxy", Version: 1, Exported: time.Now().Format(time.RFC3339),
		Settings: &exportSettings{
			EntryURL: m.cfg.EntryURL, PublicHost: m.cfg.PublicHost, PortBind: m.cfg.PortBind,
			NginxCert: m.cfg.NginxCert, NginxKey: m.cfg.NginxKey, NginxIPv6: m.cfg.NginxIPv6,
		},
		Sites: m.cfg.Sites,
	}
	data, err := json.MarshalIndent(out, "", "  ")
	m.mu.RUnlock()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	name := "emby-proxy-" + time.Now().Format("20060102-1504") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

// importConfig 导入配置。mode=merge：同 ID 的站点覆盖，其余追加；mode=replace：用导入的站点替换全部站点。
// 所有站点先整体校验，任何一个有问题就整体不导入。
func (a *Admin) importConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Mode string          `json:"mode"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<20)).Decode(&req); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	if req.Mode != "merge" && req.Mode != "replace" {
		writeErr(w, 400, "未知的导入方式")
		return
	}
	// 兼容两种格式：导出文件（settings 字段）和 config.json（设置在顶层）
	var in struct {
		exportFile
		exportSettings
	}
	if err := json.Unmarshal(req.Data, &in); err != nil {
		writeErr(w, 400, "文件不是有效的 JSON: "+err.Error())
		return
	}
	if in.Sites == nil {
		writeErr(w, 400, "文件里没有找到 sites，不是本面板导出的配置？")
		return
	}
	set := in.Settings
	if set == nil {
		set = &in.exportSettings
	}
	bind := strings.Trim(strings.TrimSpace(set.PortBind), "[]")
	if !validBind(bind) {
		writeErr(w, 400, "port_bind 不是有效的 IP")
		return
	}

	m := a.m
	m.mu.Lock()
	next := *m.cfg
	var sites []*Site
	index := map[string]int{}
	if req.Mode == "merge" {
		for _, s := range m.cfg.Sites {
			index[s.ID] = len(sites)
			sites = append(sites, s)
		}
	}
	var errs []string
	added, updated := 0, 0
	for _, s := range in.Sites {
		if s == nil {
			continue
		}
		cp := *s
		if cp.ID == "" || !pathRe.MatchString(cp.ID) {
			cp.ID = randHex(4)
		}
		if j, ok := index[cp.ID]; ok {
			sites[j] = &cp
			updated++
		} else {
			index[cp.ID] = len(sites)
			sites = append(sites, &cp)
			added++
		}
	}
	next.Sites = sites
	for _, s := range sites {
		if err := validateSite(&next, s); err != nil {
			errs = append(errs, fmt.Sprintf("「%s」: %v", s.Name, err))
		}
	}
	if len(errs) > 0 {
		m.mu.Unlock()
		if len(errs) > 10 {
			errs = append(errs[:10], fmt.Sprintf("……共 %d 个问题", len(errs)))
		}
		writeErr(w, 400, "导入失败，配置未改动：\n"+strings.Join(errs, "\n"))
		return
	}
	m.cfg.Sites = sites
	// 设置只应用文件里有值的项，只含站点的文件不会清空现有设置
	for _, f := range []struct {
		dst *string
		v   string
	}{
		{&m.cfg.EntryURL, set.EntryURL}, {&m.cfg.PublicHost, set.PublicHost},
		{&m.cfg.NginxCert, set.NginxCert}, {&m.cfg.NginxKey, set.NginxKey},
	} {
		if f.v != "" {
			*f.dst = f.v
		}
	}
	if set.NginxIPv6 {
		m.cfg.NginxIPv6 = true
	}
	if bind != "" && bind != m.cfg.PortBind {
		m.cfg.PortBind = bind
		m.rebindAll()
	} else {
		m.reload()
	}
	var bindErrs []string
	for _, s := range sites {
		if e := m.siteErr[s.ID]; e != "" {
			bindErrs = append(bindErrs, fmt.Sprintf("「%s」: %s", s.Name, e))
		}
	}
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "updated": updated, "total": len(sites), "warnings": bindErrs})
}
