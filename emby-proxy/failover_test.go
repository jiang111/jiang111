package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 模拟 Cloudflare DNS API：只有一个域名的 A 记录。
type fakeCF struct {
	mu    sync.Mutex
	recs  map[string]string // 记录 ID → IP
	next  int
	calls int
}

func newFakeCF(t *testing.T, ips ...string) *fakeCF {
	f := &fakeCF{recs: map[string]string{}}
	for _, ip := range ips {
		f.add(ip)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.calls++
		if r.Header.Get("Authorization") != "Bearer good-token" {
			json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []map[string]string{{"message": "Invalid API Token"}}})
			return
		}
		base := "/zones/zone1/dns_records"
		switch {
		case r.Method == "GET" && r.URL.Path == base:
			if r.URL.Query().Get("name") != "emby.example.com" || r.URL.Query().Get("type") != "A" {
				t.Errorf("bad list query: %s", r.URL.RawQuery)
			}
			rs := []cfRecord{}
			for id, ip := range f.recs {
				rs = append(rs, cfRecord{ID: id, Content: ip})
			}
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": rs})
		case r.Method == "POST" && r.URL.Path == base:
			var b struct {
				Type, Name, Content string
				TTL                 int
				Proxied             bool
			}
			json.NewDecoder(r.Body).Decode(&b)
			if b.Type != "A" || b.Name != "emby.example.com" || b.TTL != dnsTTL || b.Proxied {
				t.Errorf("bad create: %+v", b)
			}
			f.add(b.Content)
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, base+"/"):
			delete(f.recs, strings.TrimPrefix(r.URL.Path, base+"/"))
			json.NewEncoder(w).Encode(map[string]any{"success": true, "result": map[string]any{}})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	t.Cleanup(srv.Close)
	old := cfAPI
	cfAPI = srv.URL
	t.Cleanup(func() { cfAPI = old })
	return f
}

func (f *fakeCF) add(ip string) {
	f.next++
	f.recs[fmt.Sprint("r", f.next)] = ip
}

func (f *fakeCF) ips() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, ip := range f.recs {
		out = append(out, ip)
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := range s {
		for j := i + 1; j < len(s); j++ {
			if s[j] < s[i] {
				s[i], s[j] = s[j], s[i]
			}
		}
	}
}

// foNode 是一台从节点：本机名 self，成员 A、B，有一个独立端口站点（供探测地址用）。
func foNode(t *testing.T, self string, dns *DNSFailover) *Manager {
	cfg := &Config{
		Cluster: ClusterConfig{Role: "node", NodeName: self, Sticky: true, AdvertiseAddr: map[string]string{"A": "203.0.113.1", "B": "203.0.113.2"}[self]},
		Members: []Member{{Name: "A", Addr: "203.0.113.1"}, {Name: "B", Addr: "203.0.113.2"}},
		Sites:   []*Site{{ID: "s1", Name: "x", Enabled: true, Mode: "port", Port: 18001, PublicURL: "https://emby.example.com:8001", Upstream: "https://u.example.net"}},
		DNS:     dns,
	}
	return NewManager(filepath.Join(t.TempDir(), "c.json"), cfg)
}

// 健康检查地址：直接返回 ok，不访问上游、不计统计。
func TestPeerHealthPath(t *testing.T) {
	m, entry, _, _ := setup(t, Site{Mode: "path", Path: "hk", StreamMode: "proxy"})
	if _, got := get(t, entry.URL+"/hk"+peerHealthPath); got != "ok" {
		t.Fatalf("health = %q", got)
	}
	if v := m.stats["s1"].view(); v.Requests != 0 {
		t.Fatalf("health probe counted: %+v", v)
	}
}

// 探测：连续 3 次失败判定失联（分配不再选它），连续 2 次成功恢复。
func TestProbePeers(t *testing.T) {
	m := foNode(t, "A", nil)
	now := time.Unix(1_000_000, 0)
	fail := func(string, string) error { return errors.New("dial tcp: i/o timeout") }
	ok := func(target, addr string) error {
		if target != "https://emby.example.com:8001"+peerHealthPath || addr != "203.0.113.2" {
			t.Errorf("probe %s via %s", target, addr)
		}
		return nil
	}
	ownedByB := func() int {
		n := 0
		for i := 0; i < 200; i++ {
			if o, _ := m.owner(fmt.Sprintf("198.51.100.%d", i)); o.Name == "B" {
				n++
			}
		}
		return n
	}
	if ownedByB() == 0 {
		t.Fatal("B should own some clients while healthy")
	}
	for i := 0; i < 2; i++ {
		m.probePeers(now, fail)
	}
	if m.healthDown("B") || ownedByB() == 0 {
		t.Fatal("2 failures should not mark B down")
	}
	m.probePeers(now, fail)
	if !m.healthDown("B") || ownedByB() != 0 {
		t.Fatalf("3 failures: down=%v ownedByB=%d", m.healthDown("B"), ownedByB())
	}
	m.probePeers(now, ok)
	if !m.healthDown("B") {
		t.Fatal("1 success should not recover")
	}
	m.probePeers(now.Add(10*time.Second), ok)
	if m.healthDown("B") || ownedByB() == 0 {
		t.Fatal("2 successes should recover B")
	}
	if v := m.failoverView(); len(v.Peers) != 1 || v.Peers[0].Name != "B" || v.Peers[0].Down || v.Peers[0].Since != now.Add(10*time.Second).Unix() {
		t.Fatalf("view: %+v", v)
	}
	// 视频直连：负责节点失联时不会 302 过去（分配已经不选它）
	for i := 0; i < 3; i++ {
		m.probePeers(now, fail)
	}
	for i := 0; i < 50; i++ {
		if o, _ := m.owner(fmt.Sprintf("198.51.100.%d", i)); o.Name != "A" {
			t.Fatal("down peer chosen as owner")
		}
	}
}

func TestDNSFailover(t *testing.T) {
	cf := newFakeCF(t, "203.0.113.1", "203.0.113.2")
	dns := &DNSFailover{Token: "good-token", ZoneID: "zone1", Name: "emby.example.com"}
	m := foNode(t, "A", dns)
	fail := func(string, string) error { return errors.New("refused") }
	ok := func(string, string) error { return nil }
	t0 := time.Unix(2_000_000, 0)
	for i := 0; i < 3; i++ {
		m.probePeers(t0, fail) // B 在 t0 失联
	}
	m.dnsFailover(t0.Add(30 * time.Second))
	if got := cf.ips(); len(got) != 2 {
		t.Fatalf("should wait %v before removing: %v", dnsDelay, got)
	}
	m.dnsFailover(t0.Add(61 * time.Second))
	if got := cf.ips(); len(got) != 1 || got[0] != "203.0.113.1" {
		t.Fatalf("B should be removed: %v", got)
	}
	if v := m.failoverView(); len(v.DNSLog) != 1 || !strings.Contains(v.DNSLog[0], "已从 emby.example.com 删除 203.0.113.2") || *v.Peers[0].InDNS {
		t.Fatalf("view: %+v", v)
	}
	// 删除后的下一轮核对一次；之后仍失联、已删掉，不再反复查询 Cloudflare
	m.dnsFailover(t0.Add(71 * time.Second))
	calls := cf.calls
	m.dnsFailover(t0.Add(81 * time.Second))
	if cf.calls != calls {
		t.Fatal("should not query Cloudflare when nothing to do")
	}
	// B 恢复：满 1 分钟后加回
	t1 := t0.Add(5 * time.Minute)
	m.probePeers(t1, ok)
	m.probePeers(t1, ok)
	m.dnsFailover(t1.Add(30 * time.Second))
	if got := cf.ips(); len(got) != 1 {
		t.Fatalf("should wait before re-adding: %v", got)
	}
	m.dnsFailover(t1.Add(61 * time.Second))
	if got := cf.ips(); len(got) != 2 {
		t.Fatalf("B should be re-added: %v", got)
	}
}

// 只剩一条记录时不删；记录被删光时把自己加回；名字靠后的节点晚 20 秒动手；令牌错误显示出来。
func TestDNSFailoverSafety(t *testing.T) {
	dns := &DNSFailover{Token: "good-token", ZoneID: "zone1", Name: "emby.example.com"}
	fail := func(string, string) error { return errors.New("refused") }
	t0 := time.Unix(3_000_000, 0)

	cf := newFakeCF(t, "203.0.113.1") // 只剩 A（B 的记录已被别人删了），B 认为 A 失联
	b := foNode(t, "B", dns)
	for i := 0; i < 3; i++ {
		b.probePeers(t0, fail)
	}
	b.dnsFailover(t0.Add(70 * time.Second)) // B 排第 2，要等 60+20 秒
	b.dnsFailover(t0.Add(90 * time.Second))
	if got := cf.ips(); len(got) != 1 || got[0] != "203.0.113.1" {
		t.Fatalf("must not delete the last record: %v", got)
	}

	cf.mu.Lock()
	cf.recs = map[string]string{}
	cf.mu.Unlock()
	b.fo.mu.Lock()
	b.fo.dnsTime = 0
	b.fo.mu.Unlock()
	b.dnsFailover(t0.Add(100 * time.Second))
	if got := cf.ips(); len(got) != 1 || got[0] != "203.0.113.2" {
		t.Fatalf("empty records: self should be added back: %v", got)
	}

	// 名字靠后的节点等 80 秒
	cf.mu.Lock()
	cf.recs = map[string]string{}
	cf.mu.Unlock()
	cf.add("203.0.113.1")
	cf.add("203.0.113.2")
	b2 := foNode(t, "B", dns)
	for i := 0; i < 3; i++ {
		b2.probePeers(t0, fail)
	}
	b2.dnsFailover(t0.Add(70 * time.Second))
	if got := cf.ips(); len(got) != 2 {
		t.Fatalf("second node should wait for stagger: %v", got)
	}
	b2.dnsFailover(t0.Add(81 * time.Second))
	if got := cf.ips(); len(got) != 1 || got[0] != "203.0.113.2" {
		t.Fatalf("A should be removed by B after stagger: %v", got)
	}

	// 两台同时把对方删掉：动过 DNS 后下一轮（10 秒后）马上核对，发现记录被删光就把自己加回
	cf.mu.Lock()
	cf.recs = map[string]string{}
	cf.mu.Unlock()
	cf.add("203.0.113.1")
	cf.add("203.0.113.2")
	a := foNode(t, "A", dns)
	for i := 0; i < 3; i++ {
		a.probePeers(t0, fail)
	}
	a.dnsFailover(t0.Add(61 * time.Second)) // A 删掉 B
	cf.mu.Lock()
	for id, ip := range cf.recs {
		if ip == "203.0.113.1" {
			delete(cf.recs, id) // 同一时刻 B 也把 A 删了
		}
	}
	cf.mu.Unlock()
	a.dnsFailover(t0.Add(71 * time.Second))
	if got := cf.ips(); len(got) != 1 || got[0] != "203.0.113.1" {
		t.Fatalf("both deleted: A should add itself back on the next tick: %v", got)
	}

	bad := foNode(t, "A", &DNSFailover{Token: "bad", ZoneID: "zone1", Name: "emby.example.com"})
	bad.dnsFailover(t0)
	if v := bad.failoverView(); !strings.Contains(v.DNSErr, "Invalid API Token") {
		t.Fatalf("dns error not shown: %+v", v)
	}
}

// 主节点设置自动改 DNS：先校验令牌；面板只显示令牌前 4 位；随同步下发；空令牌关闭。
func TestDNSFailoverSettings(t *testing.T) {
	newFakeCF(t, "203.0.113.1")
	master, c, do, _, _ := clusterMaster(t)
	if code, out := do("POST", "/api/sites", `{"name":"P","upstream":"https://p.example.net","mode":"port","port":18009,"public_url":"https://emby.example.com:8009","enabled":true}`); code != 200 {
		t.Fatal(out)
	}
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx","dns_token":"bad","dns_zone_id":"zone1"}`); code != 400 || !strings.Contains(out, "Invalid API Token") {
		t.Fatalf("bad token should be rejected: %d %s", code, out)
	}
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx","dns_token":"good-token","dns_zone_id":"zone1"}`); code != 200 {
		t.Fatal(out)
	}
	if d := master.cfg.DNS; d == nil || d.Name != "emby.example.com" || d.Token != "good-token" {
		t.Fatalf("dns config: %+v", d)
	}
	_, out := do("GET", "/api/state", "")
	if strings.Contains(out, "good-token") || !strings.Contains(out, `"dns_token":"good…"`) {
		t.Fatalf("token should be masked in state: %s", out)
	}
	data, _, _ := c.payload()
	var p syncPayload
	json.Unmarshal(data, &p)
	if p.DNS == nil || p.DNS.Token != "good-token" {
		t.Fatalf("dns not in payload: %+v", p.DNS)
	}
	// 其它设置不带 dns 字段：保持不变
	do("PUT", "/api/cluster", `{"role":"master","node_name":"fx"}`)
	if master.cfg.DNS == nil {
		t.Fatal("dns cleared by unrelated save")
	}
	if code, out := do("PUT", "/api/cluster", `{"role":"master","node_name":"fx","dns_token":""}`); code != 200 || master.cfg.DNS != nil {
		t.Fatalf("empty token should disable: %d %s %+v", code, out, master.cfg.DNS)
	}

}
