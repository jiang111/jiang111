package main

import (
	"encoding/json"
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
	if rep == nil || rep.Sites[id].Requests != 3 {
		t.Fatalf("report not recorded: %+v", rep)
	}

	node.mu.Lock()
	node.cfg.Cluster.MasterURL = old.URL
	node.mu.Unlock()
	if err := nc.report(); err != errReportUnsupported {
		t.Fatalf("old master: %v", err)
	}
}
