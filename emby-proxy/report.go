package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// 统计汇总：从节点每 10 秒把本机各站点的统计上报给主节点（POST /api/cluster/report，同步令牌鉴权），
// 主节点面板显示「本机 + 各在线节点」之和以及各节点拆分。同时带上这 10 秒新增的访问日志（最多 maxReportLogs 条），
// 主节点「访问日志」汇总显示各服务器的日志（只在内存里，重启清空）。超过 45 秒没上报的节点不计入，
// 避免死节点的连接数、播放数一直停在最后的值。上报数据只在内存里，主节点重启后等下一轮上报即可。

const (
	reportInterval = 10 * time.Second
	reportMaxAge   = 45 * time.Second
	maxReportLogs  = 500
)

type nodeReport struct {
	Time    int64                // 主节点收到的时间
	Sites   map[string]StatsView // 站点 ID → 统计；收到后整体替换，不再修改
	Traffic *trafficInfo         // 节点本月反代流量（流量均衡用；旧版本节点没有）
}

// statsBase 是主节点「清零统计」时记下的某节点累计值，汇总时显示 上报值 - 基准。
type statsBase struct {
	Requests, Bytes, Errors, RelayRequests, RelayBytes int64
}

func baseOf(v StatsView) statsBase {
	return statsBase{v.Requests, v.Bytes, v.Errors, v.RelayRequests, v.RelayBytes}
}

// fix：上报值比基准小（节点重启、或在节点面板上清零过），说明计数从头开始了，该字段基准归零。
// 逐字段判断：重启后 Requests/Bytes 会从 stats.json 恢复，Errors 却从 0 开始。
func (b *statsBase) fix(v StatsView) {
	for _, p := range []struct {
		base *int64
		cur  int64
	}{{&b.Requests, v.Requests}, {&b.Bytes, v.Bytes}, {&b.Errors, v.Errors}, {&b.RelayRequests, v.RelayRequests}, {&b.RelayBytes, v.RelayBytes}} {
		if p.cur < *p.base {
			*p.base = 0
		}
	}
}

// checkToken 校验从节点带来的同步令牌（不走面板登录）；失败时已写好 401。
func (c *Cluster) checkToken(w http.ResponseWriter, r *http.Request) bool {
	m := c.m
	m.mu.RLock()
	role, token := m.cfg.Cluster.Role, m.cfg.Cluster.Token
	m.mu.RUnlock()
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if role != "master" || token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		time.Sleep(300 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "同步令牌不正确，或这台服务器不是主节点")
		return false
	}
	return true
}

// nodeName 规范化从节点报来的名字：空则用来源 IP，最长 64 字节。
func nodeName(r *http.Request, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = clientIP(r)
	}
	if len(name) > 64 {
		name = name[:64]
	}
	return name
}

// serveReport 是主节点接收从节点统计的接口。
func (c *Cluster) serveReport(w http.ResponseWriter, r *http.Request) {
	if !c.checkToken(w, r) {
		return
	}
	var body struct {
		Name    string               `json:"name"`
		Sites   map[string]StatsView `json:"sites"`
		Traffic *trafficInfo         `json:"traffic"`
		Logs    []LogEntry           `json:"logs"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	name := nodeName(r, body.Name)
	if len(body.Logs) > maxReportLogs {
		body.Logs = body.Logs[len(body.Logs)-maxReportLogs:]
	}
	for _, e := range body.Logs {
		e.Server = name
		c.remoteLogs.add(e)
	}
	// 只保留本机存在的站点（先于 c.mu 拿 m.mu，两把锁不嵌套）
	c.m.mu.RLock()
	sites := make(map[string]StatsView, len(c.m.cfg.Sites))
	for _, s := range c.m.cfg.Sites {
		if v, ok := body.Sites[s.ID]; ok {
			sites[s.ID] = v
		}
	}
	c.m.mu.RUnlock()
	body.Sites = sites
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.reports) >= 200 && c.reports[name] == nil {
		writeErr(w, http.StatusTooManyRequests, "节点数过多")
		return
	}
	for id, v := range body.Sites {
		if b, ok := c.bases[id][name]; ok {
			b.fix(v)
			c.bases[id][name] = b
		}
	}
	if body.Traffic != nil && body.Traffic.Bytes < 0 {
		body.Traffic.Bytes = 0
	}
	c.reports[name] = &nodeReport{Time: time.Now().Unix(), Sites: body.Sites, Traffic: body.Traffic}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// markCleared 在主节点清零某站点统计时调用：记下各节点当前上报值作为基准，以及清零的分钟。
func (c *Cluster) markCleared(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.clearedMin[id] = time.Now().Unix() / 60
	bs := map[string]statsBase{}
	for name, rep := range c.reports {
		if v, ok := rep.Sites[id]; ok {
			bs[name] = baseOf(v)
		}
	}
	c.bases[id] = bs
}

// dropSite 删除站点时清掉它的基准。
func (c *Cluster) dropSite(id string) {
	c.mu.Lock()
	delete(c.bases, id)
	delete(c.clearedMin, id)
	c.mu.Unlock()
}

type reportInfo struct {
	Name       string `json:"name"`
	LastReport int64  `json:"last_report"`
	Online     bool   `json:"online"` // 45 秒内上报过，计入汇总
}

type nodeStats struct {
	Name  string    `json:"name"`
	Stats StatsView `json:"stats"`
	Self  bool      `json:"self"`
}

// statsSnapshot 是汇总用的数据快照，取完后不再碰 c.mu（面板 state() 里要在持有 m.mu 时使用）。
type statsSnapshot struct {
	reports    map[string]*nodeReport // 只含在线节点
	nodes      []reportInfo
	bases      map[string]map[string]statsBase
	clearedMin map[string]int64
}

func (c *Cluster) statsSnapshot() *statsSnapshot {
	now := time.Now()
	sn := &statsSnapshot{reports: map[string]*nodeReport{}, nodes: []reportInfo{},
		bases: map[string]map[string]statsBase{}, clearedMin: map[string]int64{}}
	c.mu.Lock()
	for name, rep := range c.reports {
		online := now.Sub(time.Unix(rep.Time, 0)) <= reportMaxAge
		if online {
			sn.reports[name] = rep
		}
		sn.nodes = append(sn.nodes, reportInfo{Name: name, LastReport: rep.Time, Online: online})
	}
	for id, bs := range c.bases {
		cp := make(map[string]statsBase, len(bs))
		for k, v := range bs {
			cp[k] = v
		}
		sn.bases[id] = cp
	}
	for id, v := range c.clearedMin {
		sn.clearedMin[id] = v
	}
	c.mu.Unlock()
	sort.Slice(sn.nodes, func(i, j int) bool { return sn.nodes[i].Name < sn.nodes[j].Name })
	return sn
}

// adjust 扣掉清零基准；流量曲线里清零那一分钟及之前的桶不计入
// （那一分钟里清零之后的流量也会被丢掉，最多少算 1 分钟，换来不用逐节点保存曲线基准）。
func (sn *statsSnapshot) adjust(id, name string, v StatsView, repTime int64) StatsView {
	b := sn.bases[id][name]
	sub := func(cur, base int64) int64 { return max(0, cur-base) }
	v.Requests, v.Bytes, v.Errors = sub(v.Requests, b.Requests), sub(v.Bytes, b.Bytes), sub(v.Errors, b.Errors)
	v.RelayRequests, v.RelayBytes = sub(v.RelayRequests, b.RelayRequests), sub(v.RelayBytes, b.RelayBytes)
	series := make([]int64, 60)
	if len(v.Series) == 60 {
		copy(series, v.Series)
	}
	if cm, ok := sn.clearedMin[id]; ok {
		now := repTime / 60
		for k := range series {
			if now-int64(59-k) <= cm {
				series[k] = 0
			}
		}
	}
	v.Series = series
	return v
}

// site 汇总一个站点：本机 + 各在线节点。返回总和与各节点拆分（按名称排序）。
// withSelf 为 false 时拆分里不列出本机（只做控制的主节点不处理播放，列出来只是一行 0），本机的零星计数仍计入总和。
func (sn *statsSnapshot) site(id, self string, local StatsView, withSelf bool) (StatsView, []nodeStats) {
	ns := []nodeStats{{Name: self, Stats: local, Self: true}}
	for name, rep := range sn.reports {
		if v, ok := rep.Sites[id]; ok {
			ns = append(ns, nodeStats{Name: name, Stats: sn.adjust(id, name, v, rep.Time)})
		}
	}
	sort.Slice(ns, func(i, j int) bool { return ns[i].Name < ns[j].Name })
	sum := StatsView{Series: make([]int64, 60)}
	for _, n := range ns {
		v := n.Stats
		sum.Requests += v.Requests
		sum.Bytes += v.Bytes
		sum.Active += v.Active
		sum.Streams += v.Streams
		sum.Errors += v.Errors
		sum.RelayRequests += v.RelayRequests
		sum.RelayBytes += v.RelayBytes
		sum.LastAccess = max(sum.LastAccess, v.LastAccess)
		for k := 0; k < len(v.Series) && k < 60; k++ {
			sum.Series[k] += v.Series[k]
		}
	}
	if !withSelf {
		out := ns[:0]
		for _, n := range ns {
			if !n.Self {
				out = append(out, n)
			}
		}
		ns = out
	}
	return sum, ns
}

// ---- 从节点上报 ----

var reportClient = &http.Client{Timeout: 10 * time.Second}

var errReportUnsupported = errors.New("主节点版本较旧，不支持统计汇总")

// reportLoop 是从节点的统计上报循环（和同步循环独立）；角色不是从节点时什么也不做。
func (c *Cluster) reportLoop() {
	t := time.NewTicker(reportInterval)
	defer t.Stop()
	for range t.C {
		if c.m.isNode() {
			c.reportTick()
		}
	}
}

// reportTick 上报一次并限频打日志：旧版主节点只提示一次，其它错误同样内容 10 分钟提示一次。
// 只在 reportLoop 的 goroutine 里调用，日志状态不加锁。
func (c *Cluster) reportTick() {
	err := c.report()
	switch {
	case err == nil:
		c.repOld, c.repErr = false, ""
	case errors.Is(err, errReportUnsupported):
		if !c.repOld {
			log.Printf("上报统计: %v（升级主节点后自动恢复）", err)
			c.repOld = true
		}
	default:
		if err.Error() != c.repErr || time.Since(c.repLogAt) > 10*time.Minute {
			log.Printf("上报统计失败: %v", err)
			c.repErr, c.repLogAt = err.Error(), time.Now()
		}
	}
}

func (c *Cluster) report() error {
	m := c.m
	m.mu.RLock()
	cc := m.cfg.Cluster
	sites := map[string]StatsView{}
	for _, s := range m.cfg.Sites {
		if st := m.stats[s.ID]; st != nil {
			sites[s.ID] = st.view()
		}
	}
	m.mu.RUnlock()
	if cc.MasterURL == "" || cc.Token == "" {
		return nil // 未配置时同步循环会提示
	}
	name := cc.NodeName
	if name == "" {
		name = hostname
	}
	var tr trafficInfo
	tr.Month, tr.Bytes = m.traffic.get(time.Now())
	logs, seq := m.logs.since(c.logSeq, maxReportLogs)
	data, err := json.Marshal(map[string]any{"name": name, "sites": sites, "traffic": tr, "logs": logs})
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", strings.TrimRight(cc.MasterURL, "/")+"/api/cluster/report", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("主节点地址不正确: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+cc.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeader, "1") // 面板对所有写请求要求这个头
	resp, err := reportClient.Do(req)
	if err != nil {
		return fmt.Errorf("连接主节点失败: %s", simplifyNetErr(err))
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch resp.StatusCode {
	case 200:
		c.logSeq = seq // 上报成功才前进；失败的下次连同新的一起发（最多 maxReportLogs 条）
		return nil
	case http.StatusNotFound, http.StatusMethodNotAllowed:
		return errReportUnsupported
	}
	return fmt.Errorf("主节点返回 HTTP %d", resp.StatusCode)
}
