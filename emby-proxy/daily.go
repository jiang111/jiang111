package main

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 每日统计：每个站点每天的播放次数、访问 IP 数、流量，保留 30 天（data/daily.json）。
//   - 播放次数按 PlaybackInfo 计（客户端每次开始播放都会请求它），同一 IP 同一部片子 30 分钟内只算一次，
//     拖动、切换音轨、重连不会重复计数。
//   - 访问 IP 数是当天访问过本站的不同客户端 IP（每站每天最多记 5000 个）。
//   - 流量每分钟从累计流量的增量记入当天。
// 多台服务器时各台记自己处理的部分，从节点随统计上报，主节点面板显示各台之和。

const (
	dailyKeep   = 30
	dailyMaxIPs = 5000
	playDedup   = 30 * time.Minute
)

type DayStat struct {
	Plays int64 `json:"plays"`
	IPs   int   `json:"ips"`
	Bytes int64 `json:"bytes"`
}

type dayAcc struct {
	Plays int64           `json:"plays"`
	Bytes int64           `json:"bytes"`
	IPs   map[string]bool `json:"ips"`
}

type dailyStore struct {
	mu        sync.Mutex
	days      map[string]map[string]*dayAcc // 日期 → 站点 → 累计
	lastPlay  map[string]time.Time          // 站点|IP|片子 → 最近一次计数
	lastBytes map[string]int64              // 站点 → 上次记账时的累计流量
	now       func() time.Time
}

func newDailyStore() *dailyStore {
	return &dailyStore{days: map[string]map[string]*dayAcc{}, lastPlay: map[string]time.Time{}, lastBytes: map[string]int64{}}
}

func (d *dailyStore) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

func dayOf(t time.Time) string { return t.Format("2006-01-02") }

// acc 调用方需持有 d.mu。
func (d *dailyStore) acc(day, site string) *dayAcc {
	m := d.days[day]
	if m == nil {
		m = map[string]*dayAcc{}
		d.days[day] = m
	}
	a := m[site]
	if a == nil {
		a = &dayAcc{IPs: map[string]bool{}}
		m[site] = a
	}
	if a.IPs == nil {
		a.IPs = map[string]bool{}
	}
	return a
}

// play 记一次开始播放（PlaybackInfo）。
func (d *dailyStore) play(site, ip, item string) {
	now := d.clock()
	k := site + "|" + ip + "|" + item
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.lastPlay[k]; ok && now.Sub(t) < playDedup {
		return
	}
	d.lastPlay[k] = now
	if len(d.lastPlay) > 50000 {
		for key, t := range d.lastPlay {
			if now.Sub(t) >= playDedup {
				delete(d.lastPlay, key)
			}
		}
	}
	d.acc(dayOf(now), site).Plays++
}

// visit 记一个访问 IP。
func (d *dailyStore) visit(site, ip string) {
	day := dayOf(d.clock())
	d.mu.Lock()
	a := d.acc(day, site)
	if len(a.IPs) < dailyMaxIPs {
		a.IPs[ip] = true
	}
	d.mu.Unlock()
}

// bytesTick 把各站点累计流量的增量记入当天（每分钟调用）。
func (d *dailyStore) bytesTick(totals map[string]int64) {
	day := dayOf(d.clock())
	d.mu.Lock()
	defer d.mu.Unlock()
	for site, cur := range totals {
		last, ok := d.lastBytes[site]
		d.lastBytes[site] = cur
		if ok && cur > last {
			d.acc(day, site).Bytes += cur - last
		}
	}
	d.pruneLocked()
}

func (d *dailyStore) pruneLocked() {
	cut := dayOf(d.clock().AddDate(0, 0, -dailyKeep))
	for day := range d.days {
		if day < cut {
			delete(d.days, day)
		}
	}
}

// export 返回最近 n 天（含今天）的统计：日期 → 站点 → 数字。
func (d *dailyStore) export(n int) map[string]map[string]DayStat {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := map[string]map[string]DayStat{}
	now := d.clock()
	for i := 0; i < n; i++ {
		day := dayOf(now.AddDate(0, 0, -i))
		for site, a := range d.days[day] {
			if out[day] == nil {
				out[day] = map[string]DayStat{}
			}
			out[day][site] = DayStat{Plays: a.Plays, IPs: len(a.IPs), Bytes: a.Bytes}
		}
	}
	return out
}

func (d *dailyStore) dropSite(site string) {
	d.mu.Lock()
	for _, m := range d.days {
		delete(m, site)
	}
	d.mu.Unlock()
}

func (m *Manager) dailyPath() string { return filepath.Join(filepath.Dir(m.path), "daily.json") }

func (d *dailyStore) save(path string) {
	d.mu.Lock()
	data, err := json.Marshal(d.days)
	d.mu.Unlock()
	if err == nil {
		writeFileAtomic(path, data)
	}
}

func (d *dailyStore) load(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var days map[string]map[string]*dayAcc
	if json.Unmarshal(data, &days) != nil {
		return
	}
	d.mu.Lock()
	d.days = days
	d.pruneLocked()
	d.mu.Unlock()
}

// dailyTick 每分钟：记流量增量并保存。
func (m *Manager) dailyTick() {
	m.mu.RLock()
	totals := map[string]int64{}
	for id, st := range m.stats {
		totals[id] = st.Bytes.Load()
	}
	m.mu.RUnlock()
	m.daily.bytesTick(totals)
	m.daily.save(m.dailyPath())
}

// playItem 从 /items/{id}/playbackinfo 里取片子 ID。
func playItem(p string) string {
	i := strings.Index(p, "/items/")
	if i < 0 {
		return ""
	}
	rest := p[i+len("/items/"):]
	id, _, _ := strings.Cut(rest, "/")
	return id
}

// ---- 面板 ----

type statsSite struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// statsAPI：GET /api/stats?days=7。主节点合并各节点上报的每日统计。
func (a *Admin) statsAPI(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("days"))
	if n <= 0 || n > dailyKeep {
		n = 7
	}
	m := a.m
	data := m.daily.export(n)
	if m.isMaster() {
		a.cluster.mu.Lock()
		for _, rep := range a.cluster.reports {
			if time.Since(time.Unix(rep.Time, 0)) > 24*time.Hour {
				continue
			}
			for day, sites := range rep.Daily {
				for site, v := range sites {
					if data[day] == nil {
						data[day] = map[string]DayStat{}
					}
					cur := data[day][site]
					cur.Plays += v.Plays
					cur.IPs += v.IPs
					cur.Bytes += v.Bytes
					data[day][site] = cur
				}
			}
		}
		a.cluster.mu.Unlock()
	}
	viewer := roleOf(r) == "viewer"
	m.mu.RLock()
	sites := []statsSite{}
	keep := map[string]bool{}
	for _, s := range m.cfg.Sites {
		if viewer && !s.Enabled {
			continue
		}
		sites = append(sites, statsSite{s.ID, s.Name})
		keep[s.ID] = true
	}
	m.mu.RUnlock()
	now := time.Now()
	days := make([]string, n)
	for i := range days {
		days[n-1-i] = dayOf(now.AddDate(0, 0, -i))
	}
	for day, ss := range data {
		for id := range ss {
			if !keep[id] {
				delete(ss, id)
			}
		}
		if len(ss) == 0 {
			delete(data, day)
		}
	}
	sort.Strings(days)
	writeJSON(w, 200, map[string]any{"days": days, "sites": sites, "data": data})
}
