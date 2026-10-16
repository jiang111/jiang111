package main

import (
	"encoding/json"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// 流量均衡：每台记录本月经反代的流量（本机处理 + 节点间转发，按发给客户端的字节算），从节点随统计上报。
// 主节点每天凌晨 balanceHour 点按各台本月用量重算分配权重：用得少的权重高，分到的客户端 IP 多。
// 权重随节点列表同步给从节点，各台用加权 rendezvous 哈希算出一致的负责节点；
// 权重全为 1 时与不加权的结果完全相同。只在凌晨调整，避免看片中途换节点（上游会看到 IP 变化）。

const (
	balanceHour = 5            // 每天几点（本地时间，容器 TZ）重算权重
	balanceBase = 10 << 30     // 平滑量：月初用量都很小时不至于权重悬殊
	weightMin   = 0.05         // 权重下限、上限
	weightMax   = 20.0         //
	factorMin   = 0.1          // 手动系数范围
	factorMax   = 10.0         //
	monthLayout = "2006-01"    // 按自然月统计，每月 1 号清零
	dayLayout   = "2006-01-02" //
)

// monthTraffic 是本机本月的反代流量。
type monthTraffic struct {
	mu    sync.Mutex
	month string
	bytes atomic.Int64
}

func (t *monthTraffic) add(n int64) {
	if t != nil && n > 0 {
		t.bytes.Add(n)
	}
}

// get 返回当前月份和本月流量；跨月时清零（月份为空是刚启动、还没有记录，不清零）。
func (t *monthTraffic) get(now time.Time) (string, int64) {
	cur := now.Format(monthLayout)
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.month != cur {
		if t.month != "" {
			t.bytes.Store(0)
		}
		t.month = cur
	}
	return cur, t.bytes.Load()
}

type trafficInfo struct {
	Month string `json:"month"`
	Bytes int64  `json:"bytes"`
}

func (m *Manager) trafficPath() string { return filepath.Join(filepath.Dir(m.path), "traffic.json") }

func (m *Manager) loadTraffic() {
	var v trafficInfo
	if data, err := os.ReadFile(m.trafficPath()); err == nil && json.Unmarshal(data, &v) == nil {
		m.traffic.month = v.Month
		m.traffic.bytes.Store(v.Bytes)
	}
}

func (m *Manager) saveTraffic() {
	month, n := m.traffic.get(time.Now())
	data, _ := json.Marshal(trafficInfo{month, n})
	writeFileAtomic(m.trafficPath(), data)
}

// weightOf 返回成员的分配权重（0 视为 1）。
func weightOf(mb Member) float64 {
	if mb.Weight > 0 {
		return mb.Weight
	}
	return 1
}

// hrwScore 是加权 rendezvous 哈希的得分：h 映射到 (0,1)，得分 -w/ln(h)。权重相同时与直接比较 h 的顺序一致。
func hrwScore(sum uint64, w float64) float64 {
	h := (float64(sum>>11) + 0.5) / (1 << 53)
	return -w / math.Log(h)
}

type balanceNode struct {
	Name     string  `json:"name"`
	Bytes    int64   `json:"bytes"`    // 本月流量
	Weight   float64 `json:"weight"`   // 当前生效的权重
	Factor   float64 `json:"factor"`   // 手动系数
	Reported bool    `json:"reported"` // 有本月用量数据（从节点 45 秒内上报过）
}

// balanceUsage 收集各成员本月用量：本机读计数器，从节点用最近一次在线上报。拿不到的标记为未上报。
func (c *Cluster) balanceUsage(members []Member, self string, now time.Time) map[string]*balanceNode {
	month, local := c.m.traffic.get(now)
	out := map[string]*balanceNode{}
	c.mu.Lock()
	for _, mb := range members {
		n := &balanceNode{Name: mb.Name}
		if mb.Name == self {
			n.Bytes, n.Reported = local, true
		} else if rep := c.reports[mb.Name]; rep != nil && rep.Traffic != nil && now.Sub(time.Unix(rep.Time, 0)) <= reportMaxAge {
			if rep.Traffic.Month == month {
				n.Bytes = rep.Traffic.Bytes
			}
			n.Reported = true
		}
		out[mb.Name] = n
	}
	c.mu.Unlock()
	return out
}

// calcWeights 按本月用量算权重：(平均 + 平滑量) / (本台 + 平滑量) × 手动系数。没有用量数据的节点保持原权重。
func calcWeights(usage map[string]*balanceNode, factors, old map[string]float64) map[string]float64 {
	var sum float64
	var cnt int
	for _, n := range usage {
		if n.Reported {
			sum += float64(n.Bytes)
			cnt++
		}
	}
	out := map[string]float64{}
	for name, n := range usage {
		w, ok := old[name]
		if !ok || w <= 0 {
			w = 1
		}
		if n.Reported && cnt > 0 {
			w = (sum/float64(cnt) + balanceBase) / (float64(n.Bytes) + balanceBase)
			if f := factors[name]; f > 0 {
				w *= f
			}
		}
		w = math.Round(min(max(w, weightMin), weightMax)*100) / 100
		out[name] = w
	}
	return out
}

// rebalance 在主节点重算权重并保存（随节点列表同步给从节点）。
func (c *Cluster) rebalance(now time.Time) error {
	m := c.m
	m.mu.RLock()
	on := m.cfg.Cluster.Role == "master" && m.cfg.Cluster.Balance
	factors, old := cloneWeights(m.cfg.Cluster.Factors), cloneWeights(m.cfg.Cluster.Weights)
	m.mu.RUnlock()
	if !on {
		return nil
	}
	members := m.members()
	usage := c.balanceUsage(members, m.selfName(), now)
	w := calcWeights(usage, factors, old)
	m.mu.Lock()
	m.cfg.Cluster.Weights = w
	m.cfg.Cluster.LastBalance = now.Format(dayLayout)
	m.mu.Unlock()
	log.Printf("流量均衡：已按本月用量重算权重 %v", w)
	return m.save()
}

func cloneWeights(src map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

// balanceLoop：每分钟保存本月流量；主节点每天 balanceHour 点重算一次权重。
func (c *Cluster) balanceLoop() {
	for now := range time.Tick(time.Minute) {
		c.m.saveTraffic()
		c.balanceTick(now)
	}
}

func (c *Cluster) balanceTick(now time.Time) {
	m := c.m
	m.mu.RLock()
	due := m.cfg.Cluster.Role == "master" && m.cfg.Cluster.Balance && now.Hour() == balanceHour &&
		m.cfg.Cluster.LastBalance != now.Format(dayLayout)
	m.mu.RUnlock()
	if due {
		if err := c.rebalance(now); err != nil {
			log.Printf("流量均衡：保存失败: %v", err)
		}
	}
}

// balanceView 给面板显示：主节点列出各成员本月流量、当前权重和手动系数。
func (c *Cluster) balanceView(members []Member, self string) []balanceNode {
	m := c.m
	m.mu.RLock()
	factors := cloneWeights(m.cfg.Cluster.Factors)
	m.mu.RUnlock()
	usage := c.balanceUsage(members, self, time.Now())
	out := []balanceNode{}
	for _, mb := range members {
		n := *usage[mb.Name]
		n.Weight = weightOf(mb)
		n.Factor = factors[mb.Name]
		if n.Factor <= 0 {
			n.Factor = 1
		}
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// rebalanceNow 是面板的「立即重新分配」。
func (a *Admin) rebalanceNow(w http.ResponseWriter, r *http.Request) {
	if !a.m.isMaster() {
		writeErr(w, http.StatusConflict, "只有主节点能重新分配")
		return
	}
	a.m.mu.RLock()
	on := a.m.cfg.Cluster.Balance
	a.m.mu.RUnlock()
	if !on {
		writeErr(w, http.StatusConflict, "请先开启「流量均衡」")
		return
	}
	if err := a.cluster.rebalance(time.Now()); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	a.m.mu.RLock()
	weights := cloneWeights(a.m.cfg.Cluster.Weights)
	a.m.mu.RUnlock()
	writeJSON(w, 200, map[string]any{"ok": true, "weights": weights})
}

func (m *Manager) isMaster() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.cfg.Cluster.Role == "master"
}
