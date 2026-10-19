package main

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPlayKey(t *testing.T) {
	for _, c := range []struct{ p, want string }{
		{"/emby/videos/123/stream.mkv", "ip|123"},
		{"/videos/123/hls1/main/0.ts", "ip|123"},
		{"/v/seg/1.ts", "ip|/v/seg/1.ts"},
		{"/videos/", "ip|/videos/"},
	} {
		if got := playKey("ip", c.p); got != c.want {
			t.Errorf("playKey(%q) = %q, want %q", c.p, got, c.want)
		}
	}
}

// waitIdle 等本机请求的收尾（defer 里的 end）跑完：客户端读完响应时 handler 可能还没返回。
func waitIdle(st *Stats) {
	for d := time.Now().Add(2 * time.Second); st.Active.Load() != 0 && time.Now().Before(d); {
		time.Sleep(5 * time.Millisecond)
	}
}

// 播放去重：同一 IP 同一片子的并发请求算 1 个；不同片子、不同 IP 分开算；结束后保留 15 秒。
func TestPlayDedup(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	st := &Stats{now: func() time.Time { return now }}
	a1 := playKey("203.0.113.1", "/emby/videos/1/stream")
	a1b := playKey("203.0.113.1", "/videos/1/original.mkv")
	a2 := playKey("203.0.113.1", "/emby/videos/2/stream")
	b1 := playKey("203.0.113.2", "/emby/videos/1/stream")

	st.startPlay(a1)
	st.startPlay(a1b)
	if n := st.streams(); n != 1 {
		t.Fatalf("same ip same item: %d", n)
	}
	st.startPlay(a2)
	if n := st.streams(); n != 2 {
		t.Fatalf("different item: %d", n)
	}
	st.startPlay(b1)
	if n := st.streams(); n != 3 {
		t.Fatalf("different ip: %d", n)
	}
	st.endPlay(a2)
	st.endPlay(b1)
	st.endPlay(a1)
	now = now.Add(5 * time.Second)
	st.endPlay(a1b) // a1 最后一个请求在这里结束
	now = now.Add(12 * time.Second)
	// a2、b1 已结束 17 秒；a1 结束 12 秒，仍算在播
	if n := st.streams(); n != 1 {
		t.Fatalf("linger: %d", n)
	}
	now = now.Add(3 * time.Second)
	if n := st.streams(); n != 0 || len(st.plays) != 0 {
		t.Fatalf("after linger: %d %d", n, len(st.plays))
	}
}

// 走一遍反代：视频响应计入播放，按 IP + 片子去重。
func TestPlayDedupHTTP(t *testing.T) {
	m, entry, _, _ := setup(t, Site{Mode: "path", Path: "hk", StreamMode: "proxy"})
	st := m.stats["s1"]
	u := entry.URL + "/hk/emby/videos/1/stream?api_key=x"
	get(t, u, "Range", "bytes=0-9", "X-Real-Ip", "203.0.113.1")
	get(t, u, "Range", "bytes=10-19", "X-Real-Ip", "203.0.113.1")
	waitIdle(st)
	if v := st.view(); v.Streams != 1 || v.Active != 0 || v.Requests != 2 {
		t.Fatalf("same ip: %+v", v)
	}
	get(t, u, "X-Real-Ip", "203.0.113.2")
	if n := st.view().Streams; n != 2 {
		t.Fatalf("two ips: %d", n)
	}
	get(t, entry.URL+"/hk/emby/System/Info/Public", "X-Real-Ip", "203.0.113.3")
	if n := st.view().Streams; n != 2 {
		t.Fatalf("api request counted as play: %d", n)
	}
}

func clusterMaster(t *testing.T) (*Manager, *Cluster, func(method, path, body string, hdr ...string) (int, string), string, string) {
	master, srv, ck := newAdmin(t, &Config{})
	c := newCluster(master)
	srv.Config.Handler = (&Admin{m: master, cluster: c, failures: map[string][]time.Time{}}).Handler()
	do := func(method, path, body string, hdr ...string) (int, string) {
		return adminDo(t, srv, ck, method, path, body, append([]string{csrfHeader, "1"}, hdr...)...)
	}
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx"}`); code != 200 {
		t.Fatal(out)
	}
	code, out := do("POST", "/api/sites", `{"name":"A","upstream":"https://a.example.net","mode":"path","path":"a","enabled":true}`)
	if code != 200 {
		t.Fatal(out)
	}
	var r struct{ ID string }
	json.Unmarshal([]byte(out), &r)
	return master, c, do, r.ID, srv.URL
}

type stateResp struct {
	Sites []struct {
		ID           string      `json:"id"`
		Stats        StatsView   `json:"stats"`
		ClusterStats *StatsView  `json:"cluster_stats"`
		NodeStats    []nodeStats `json:"node_stats"`
	} `json:"sites"`
	Nodes []reportInfo `json:"cluster_stats_nodes"`
}

func TestClusterReport(t *testing.T) {
	master, c, do, id, _ := clusterMaster(t)
	token := master.cfg.Cluster.Token
	st := master.stats[id]
	st.Requests.Store(10)
	st.Bytes.Store(1000)
	st.Active.Store(1)

	report := func(tok string, v StatsView) int {
		v.Series = make([]int64, 60)
		v.Series[59] = v.Bytes
		body, _ := json.Marshal(map[string]any{"name": "isif", "sites": map[string]StatsView{id: v}})
		code, _ := do("POST", "/api/cluster/report", string(body), "Authorization", "Bearer "+tok)
		return code
	}
	state := func() stateResp {
		_, out := do("GET", "/api/state", "")
		var s stateResp
		if err := json.Unmarshal([]byte(out), &s); err != nil || len(s.Sites) != 1 {
			t.Fatalf("state: %v %s", err, out)
		}
		return s
	}

	if code := report("wrong", StatsView{Requests: 1}); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if code := report(token, StatsView{Requests: 5, Bytes: 500, Active: 2, Streams: 1, RelayBytes: 50}); code != 200 {
		t.Fatalf("report: %d", code)
	}
	s := state()
	cs := s.Sites[0].ClusterStats
	if cs == nil || cs.Requests != 15 || cs.Bytes != 1500 || cs.Active != 3 || cs.Streams != 1 || cs.RelayBytes != 50 || cs.Series[59] < 500 {
		t.Fatalf("cluster stats: %+v", cs)
	}
	if ns := s.Sites[0].NodeStats; len(ns) != 2 || ns[0].Name != "fx" || !ns[0].Self || ns[1].Name != "isif" || ns[1].Stats.Active != 2 {
		t.Fatalf("node stats: %+v", ns)
	}
	if len(s.Nodes) != 1 || s.Nodes[0].Name != "isif" || !s.Nodes[0].Online {
		t.Fatalf("report nodes: %+v", s.Nodes)
	}
	if s.Sites[0].Stats.Requests != 10 {
		t.Fatalf("local stats changed: %+v", s.Sites[0].Stats)
	}

	// 清零：本机清零，节点的累计值以当前上报为基准
	if code, out := do("POST", "/api/sites/"+id+"/clear-stats", ""); code != 200 {
		t.Fatal(out)
	}
	cs = state().Sites[0].ClusterStats
	if cs.Requests != 0 || cs.Bytes != 0 || cs.RelayBytes != 0 || cs.Active != 3 || cs.Streams != 1 || cs.Series[59] != 0 {
		t.Fatalf("after clear: %+v", cs)
	}
	report(token, StatsView{Requests: 8, Bytes: 800, Errors: 2, RelayBytes: 60})
	if cs = state().Sites[0].ClusterStats; cs.Requests != 3 || cs.Bytes != 300 || cs.RelayBytes != 10 || cs.Errors != 2 {
		t.Fatalf("delta after clear: %+v", cs)
	}
	// 节点重启：Requests 从 stats.json 恢复（不变小），转发流量从 0 开始 → 该字段基准归零
	report(token, StatsView{Requests: 9, Bytes: 900, RelayBytes: 7})
	if cs = state().Sites[0].ClusterStats; cs.Requests != 4 || cs.Bytes != 400 || cs.RelayBytes != 7 || cs.Errors != 0 {
		t.Fatalf("after node restart: %+v", cs)
	}

	// 45 秒没上报的节点不计入
	c.mu.Lock()
	c.reports["isif"].Time = time.Now().Add(-50 * time.Second).Unix()
	c.mu.Unlock()
	s = state()
	if cs = s.Sites[0].ClusterStats; cs.Active != 1 || cs.Streams != 0 || len(s.Sites[0].NodeStats) != 1 {
		t.Fatalf("stale node counted: %+v %+v", cs, s.Sites[0].NodeStats)
	}
	if len(s.Nodes) != 1 || s.Nodes[0].Online {
		t.Fatalf("stale node should be offline: %+v", s.Nodes)
	}

	// 只做控制的主节点本机没有流量：不列出本机那一行
	st.Requests.Store(0)
	st.Bytes.Store(0)
	st.Active.Store(0)
	master.stats[id].clear()
	report(token, StatsView{Requests: 20, Active: 1})
	if ns := state().Sites[0].NodeStats; len(ns) != 1 || ns[0].Name != "isif" {
		t.Fatalf("control-only master with no traffic should not be listed: %+v", ns)
	}
	master.mu.Lock()
	master.cfg.Cluster.AdvertiseAddr = "203.0.113.9" // 主节点也接流量：本机照常列出
	master.mu.Unlock()
	if ns := state().Sites[0].NodeStats; len(ns) != 2 || !ns[0].Self {
		t.Fatalf("serving master should be listed: %+v", ns)
	}

	// 删除站点清掉基准
	do("DELETE", "/api/sites/"+id, "")
	c.mu.Lock()
	_, ok := c.bases[id]
	c.mu.Unlock()
	if ok {
		t.Fatal("bases not dropped")
	}
}

// 非主节点不返回汇总字段；/api/cluster/report 对非主节点一律 401。
func TestClusterReportNotMaster(t *testing.T) {
	_, srv, ck := newAdmin(t, &Config{})
	code, out := adminDo(t, srv, ck, "GET", "/api/state", "")
	if code != 200 || strings.Contains(out, "cluster_stats") {
		t.Fatalf("standalone state: %s", out)
	}
	code, _ = adminDo(t, srv, nil, "POST", "/api/cluster/report", `{}`, csrfHeader, "1", "Authorization", "Bearer ")
	if code != http.StatusUnauthorized {
		t.Fatalf("report on standalone: %d", code)
	}
}

// 从节点上报：发到主节点的 /api/cluster/report；主节点是旧版本（404）时返回 errReportUnsupported。
func TestNodeReport(t *testing.T) {
	master, mc, _, id, masterURL := clusterMaster(t)
	old := httptest.NewServer(http.NotFoundHandler()) // 旧版主节点：没有这个接口
	t.Cleanup(old.Close)

	node, _, _ := newAdmin(t, &Config{})
	node.mu.Lock()
	node.cfg.Sites = []*Site{{ID: id, Name: "A"}}
	node.stats[id] = &Stats{}
	node.stats[id].Requests.Store(3)
	node.cfg.Cluster = ClusterConfig{Role: "node", NodeName: "isif", Token: master.cfg.Cluster.Token}
	node.cfg.Cluster.MasterURL = masterURL
	node.mu.Unlock()
	nc := newCluster(node)
	if err := nc.report(); err != nil {
		t.Fatal(err)
	}
	mc.mu.Lock()
	rep := mc.reports["isif"]
	mc.mu.Unlock()
	if rep == nil || rep.Sites[id].Requests != 3 || rep.Traffic == nil || rep.Traffic.Month == "" {
		t.Fatalf("report not recorded: %+v", rep)
	}

	node.mu.Lock()
	node.cfg.Cluster.MasterURL = old.URL
	node.mu.Unlock()
	if err := nc.report(); err != errReportUnsupported {
		t.Fatalf("old master: %v", err)
	}
}

func TestLogRingSince(t *testing.T) {
	l := newLogRing(4)
	add := func(from, to int) {
		for i := from; i < to; i++ {
			l.add(LogEntry{Time: int64(i)})
		}
	}
	times := func(es []LogEntry) (out []int64) {
		for _, e := range es {
			out = append(out, e.Time)
		}
		return
	}
	add(0, 3)
	es, seq := l.since(0, 10)
	if fmt.Sprint(times(es)) != "[0 1 2]" || seq != 3 {
		t.Fatalf("first: %v %d", times(es), seq)
	}
	add(3, 5)
	if es, seq = l.since(seq, 10); fmt.Sprint(times(es)) != "[3 4]" || seq != 5 {
		t.Fatalf("incremental: %v %d", times(es), seq)
	}
	if es, _ = l.since(seq, 10); len(es) != 0 {
		t.Fatal("nothing new")
	}
	add(5, 15) // 环只有 4 格：之前没取的被覆盖，只能拿到最近 4 条
	if es, seq = l.since(seq, 10); fmt.Sprint(times(es)) != "[11 12 13 14]" || seq != 15 {
		t.Fatalf("overwritten: %v %d", times(es), seq)
	}
	add(15, 18)
	if es, _ = l.since(seq, 2); fmt.Sprint(times(es)) != "[16 17]" {
		t.Fatalf("limit keeps newest: %v", times(es))
	}
	if got := times(l.list(func(e *LogEntry) bool { return e.Time%2 == 1 }, 10)); fmt.Sprint(got) != "[17 15]" {
		t.Fatalf("list newest first with filter: %v", got)
	}
}

// 访问日志汇总：从节点上报时带上增量日志（成功才前进），主节点合并本机和各节点、按时间倒序，可按站点 / 服务器筛选。
func TestClusterLogs(t *testing.T) {
	master, mc, do, id, masterURL := clusterMaster(t)
	master.logs.add(LogEntry{Time: 100, Site: id, Path: "/master"})

	node, _, _ := newAdmin(t, &Config{})
	node.mu.Lock()
	node.cfg.Sites = []*Site{{ID: id, Name: "A"}}
	node.cfg.Cluster = ClusterConfig{Role: "node", NodeName: "isif", Token: master.cfg.Cluster.Token, MasterURL: masterURL}
	node.mu.Unlock()
	node.logs.add(LogEntry{Time: 50, Site: id, Path: "/old"})
	node.logs.add(LogEntry{Time: 150, Site: "other", Path: "/other"})
	nc := newCluster(node)
	if err := nc.report(); err != nil || nc.logSeq != 2 {
		t.Fatalf("report: %v seq=%d", err, nc.logSeq)
	}
	node.logs.add(LogEntry{Time: 200, Site: id, Path: "/new"})
	nc.report()
	if n := mc.remoteLogs.total; n != 3 {
		t.Fatalf("each entry should be sent once: %d", n)
	}

	var got []LogEntry
	get := func(q string) string {
		_, out := do("GET", "/api/logs?"+q, "")
		got = nil
		json.Unmarshal([]byte(out), &got)
		s := []string{}
		for _, e := range got {
			s = append(s, e.Server+e.Path)
		}
		return strings.Join(s, ",")
	}
	if s := get("limit=10"); s != "isif/new,isif/other,fx/master,isif/old" {
		t.Fatalf("merged: %s", s)
	}
	if s := get("server=isif&site=" + id); s != "isif/new,isif/old" {
		t.Fatalf("filtered: %s", s)
	}
	if s := get("server=fx"); s != "fx/master" {
		t.Fatalf("self: %s", s)
	}
	if s := get("limit=2"); s != "isif/new,isif/other" {
		t.Fatalf("limit: %s", s)
	}

	// 上报失败不前进，下次重发
	node.mu.Lock()
	node.cfg.Cluster.Token = "wrong"
	node.mu.Unlock()
	node.logs.add(LogEntry{Time: 300, Site: id, Path: "/retry"})
	if err := nc.report(); err == nil || nc.logSeq != 3 {
		t.Fatalf("failed report should not advance: %v %d", err, nc.logSeq)
	}
}

// 加权分配：权重都为 1 时与不加权完全一致；权重 3:1 时约 75% 的 IP 归权重高的；调高权重只会把 IP 拉过来，不会推走。
func TestWeightedOwner(t *testing.T) {
	owner := func(ms []Member, ip string) string {
		m := &Manager{cfg: &Config{Cluster: ClusterConfig{Role: "node", Sticky: true}, Members: ms}, downUntil: map[string]time.Time{}}
		o, _ := m.owner(ip)
		return o.Name
	}
	plain := func(ip string) string { // 原来的不加权算法
		best, bs := "", uint64(0)
		for _, n := range []string{"fx", "isif"} {
			sum := sha256.Sum256([]byte(n + "|" + ip))
			if s := binary.BigEndian.Uint64(sum[:8]); best == "" || s > bs {
				best, bs = n, s
			}
		}
		return best
	}
	eq := []Member{{Name: "fx"}, {Name: "isif", Weight: 1}}
	w31 := []Member{{Name: "fx", Weight: 3}, {Name: "isif", Weight: 1}}
	w51 := []Member{{Name: "fx", Weight: 5}, {Name: "isif", Weight: 1}}
	fx := 0
	const n = 4000
	for i := 0; i < n; i++ {
		ip := fmt.Sprintf("198.51.%d.%d", i/250, i%250)
		if a, b := owner(eq, ip), plain(ip); a != b {
			t.Fatalf("equal weights changed assignment for %s: %s vs %s", ip, a, b)
		}
		o3 := owner(w31, ip)
		if o3 == "fx" {
			fx++
		}
		if o3 == "fx" && owner(w51, ip) != "fx" {
			t.Fatalf("raising fx weight moved %s away from fx", ip)
		}
	}
	if r := float64(fx) / n; r < 0.70 || r > 0.80 {
		t.Fatalf("weight 3:1 share = %.2f, want ~0.75", r)
	}
}

func TestCalcWeights(t *testing.T) {
	gb := int64(1 << 30)
	u := func(fx, isif int64, isifReported bool) map[string]*balanceNode {
		return map[string]*balanceNode{"fx": {Name: "fx", Bytes: fx, Reported: true}, "isif": {Name: "isif", Bytes: isif, Reported: isifReported}}
	}
	if w := calcWeights(u(0, 0, true), nil, nil); w["fx"] != 1 || w["isif"] != 1 {
		t.Fatalf("equal usage: %v", w)
	}
	// 平均 15G，平滑 10G：fx (25/10)=2.5，isif (25/40)=0.625→0.63
	if w := calcWeights(u(0, 30*gb, true), nil, nil); w["fx"] != 2.5 || w["isif"] != 0.63 {
		t.Fatalf("unequal usage: %v", w)
	}
	// 手动系数
	if w := calcWeights(u(0, 30*gb, true), map[string]float64{"fx": 0.5}, nil); w["fx"] != 1.25 {
		t.Fatalf("factor: %v", w)
	}
	// 没有用量数据的节点保持原权重；平均值只按有数据的算
	if w := calcWeights(u(5*gb, 0, false), nil, map[string]float64{"isif": 0.7}); w["isif"] != 0.7 || w["fx"] != 1 {
		t.Fatalf("unreported: %v", w)
	}
	// 上限；两台时用得多的那台最低到 0.5
	if w := calcWeights(u(0, 100000*gb, true), map[string]float64{"fx": 10}, nil); w["fx"] != weightMax || w["isif"] != 0.5 {
		t.Fatalf("clamp: %v", w)
	}
}

// 本月流量：本机处理和转发都计入；跨月清零；保存后重启能恢复。
func TestMonthTraffic(t *testing.T) {
	var tr monthTraffic
	may := time.Date(2026, 5, 31, 23, 0, 0, 0, time.Local)
	tr.add(40) // 启动后第一次读取之前的流量不能丢
	if mo, n := tr.get(may); mo != "2026-05" || n != 40 {
		t.Fatal(mo, n)
	}
	tr.add(60)
	tr.add(-5)
	if _, n := tr.get(may); n != 100 {
		t.Fatal(n)
	}
	if mo, n := tr.get(may.Add(2 * time.Hour)); mo != "2026-06" || n != 0 {
		t.Fatalf("month rollover: %s %d", mo, n)
	}
	var nilTr *monthTraffic
	nilTr.add(1) // countWriter 没设置时不崩

	m, entry, _, _ := setup(t, Site{Mode: "path", Path: "hk", StreamMode: "proxy"})
	get(t, entry.URL+"/hk/emby/System/Info/Public")
	waitIdle(m.stats["s1"])
	_, n := m.traffic.get(time.Now())
	if n <= 0 || n != m.stats["s1"].Bytes.Load() {
		t.Fatalf("traffic %d, site bytes %d", n, m.stats["s1"].Bytes.Load())
	}
	m.saveTraffic()
	m2 := NewManager(m.path, &Config{})
	if _, n2 := m2.traffic.get(time.Now()); n2 != n {
		t.Fatalf("reload traffic: %d want %d", n2, n)
	}
}

// 主节点：从节点上报本月流量 → 立即重新分配 → 权重写入配置并随节点列表下发；手动系数校验。
func TestRebalance(t *testing.T) {
	master, c, do, _, _ := clusterMaster(t)
	if !master.cfg.Cluster.Balance {
		t.Fatal("balance should default on for a new master")
	}
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx","advertise_addr":"203.0.113.1","factors":{"isif":20}}`); code != 400 {
		t.Fatalf("factor out of range should be rejected: %d %s", code, out)
	}
	c.mu.Lock()
	c.nodes["isif"] = &nodeInfo{Name: "isif", Addr: "203.0.113.2", LastSeen: time.Now().Unix()}
	c.mu.Unlock()
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx","advertise_addr":"203.0.113.1"}`); code != 200 {
		t.Fatal(out)
	}
	month, _ := master.traffic.get(time.Now())
	body, _ := json.Marshal(map[string]any{"name": "isif", "sites": map[string]StatsView{}, "traffic": trafficInfo{month, 30 << 30}})
	if code, out := do("POST", "/api/cluster/report", string(body), "Authorization", "Bearer "+master.cfg.Cluster.Token); code != 200 {
		t.Fatal(out)
	}
	code, out := do("POST", "/api/cluster/rebalance", "")
	if code != 200 {
		t.Fatal(out)
	}
	w := master.cfg.Cluster.Weights
	if w["fx"] <= 2 || w["isif"] >= 1 || master.cfg.Cluster.LastBalance == "" {
		t.Fatalf("weights: %v (%s)", w, out)
	}
	ms := master.members()
	if len(ms) != 2 || ms[0].Name != "fx" || ms[0].Weight != w["fx"] || ms[1].Weight != w["isif"] {
		t.Fatalf("members should carry weights: %+v", ms)
	}
	data, _, _ := c.payload()
	var p syncPayload
	json.Unmarshal(data, &p)
	if len(p.Members) != 2 || p.Members[1].Weight != w["isif"] {
		t.Fatalf("payload members: %+v", p.Members)
	}
	// 面板能看到各台本月流量和权重
	_, out = do("GET", "/api/state", "")
	var st struct {
		Cluster clusterView `json:"cluster"`
	}
	json.Unmarshal([]byte(out), &st)
	if bn := st.Cluster.BalanceNodes; len(bn) != 2 || bn[1].Name != "isif" || bn[1].Bytes != 30<<30 || !bn[1].Reported || bn[1].Weight != w["isif"] {
		t.Fatalf("balance view: %+v", bn)
	}
	// 关闭流量均衡：权重不再下发（全按 1）
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx","advertise_addr":"203.0.113.1","balance":false}`); code != 200 {
		t.Fatal(out)
	}
	for _, mb := range master.members() {
		if mb.Weight != 0 {
			t.Fatalf("weights should not be used when balance is off: %+v", mb)
		}
	}
	if code, _ := do("POST", "/api/cluster/rebalance", ""); code != http.StatusConflict {
		t.Fatalf("rebalance with balance off: %d", code)
	}
}

// 每天 balanceHour 点重算一次，同一天不重复。
func TestBalanceTick(t *testing.T) {
	master, c, _, _, _ := clusterMaster(t)
	day := time.Date(2026, 10, 16, balanceHour, 3, 0, 0, time.Local)
	c.balanceTick(day.Add(-time.Hour))
	if master.cfg.Cluster.LastBalance != "" {
		t.Fatal("should not run outside the hour")
	}
	c.balanceTick(day)
	if master.cfg.Cluster.LastBalance != "2026-10-16" {
		t.Fatalf("should run at %d:00: %q", balanceHour, master.cfg.Cluster.LastBalance)
	}
	master.mu.Lock()
	master.cfg.Cluster.Weights = map[string]float64{"x": 9}
	master.mu.Unlock()
	c.balanceTick(day.Add(30 * time.Minute))
	if master.cfg.Cluster.Weights["x"] != 9 {
		t.Fatal("should not run twice the same day")
	}
}
