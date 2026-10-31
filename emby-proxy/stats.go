package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// playLinger：某部片子最后一个视频请求结束后，仍算作「播放中」的时间（HLS 分段、Range 重连之间的空档）
const playLinger = 15 * time.Second

// ipRecent：访问过的客户端 IP 保留多久（面板「连接」悬停列出最近在用的 IP，不只是此刻有请求在途的）
const ipRecent = 10 * time.Minute

// playReportTTL：客户端停发播放进度上报（/Sessions/Playing/Progress）多久后不再算播放中。
// SenPlayer 等 10 秒报一次，有的客户端间隔更长，留足余量；收到 /Sessions/Playing/Stopped 立即清掉。
const playReportTTL = 70 * time.Second

type Stats struct {
	Requests      atomic.Int64
	Bytes         atomic.Int64
	Active        atomic.Int64
	Errors        atomic.Int64
	LastAccess    atomic.Int64
	RelayRequests atomic.Int64 // 转发给其它节点处理的请求（不计入上面几项）
	RelayBytes    atomic.Int64

	now func() time.Time // 测试可注入时钟，nil 用 time.Now

	mu        sync.Mutex
	buckets   [60]int64
	bucketMin [60]int64
	plays     map[string]*playSess // 客户端 IP + 片子 → 播放会话（按经过本机的视频响应判断）
	reports   map[string]time.Time // 客户端 IP + 片子 → 最近一次播放上报（推流不经过本机时也能判断）
	seen      map[string]*ipSeen   // 客户端 IP → 在途请求数和最后访问时间（面板「连接」悬停显示）
	seenPrune time.Time
}

type ipSeen struct {
	n    int64     // 在途请求数
	last time.Time // 最后一次请求开始或结束的时间
}

type playSess struct {
	n    int       // 进行中的视频请求数
	last time.Time // 最后一个请求结束的时间
}

func (s *Stats) clock() time.Time {
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

// begin 开始一个本机处理的请求，ip 是客户端真实 IP（转来的请求为原始 IP）。
func (s *Stats) begin(ip string) {
	s.Requests.Add(1)
	s.Active.Add(1)
	s.LastAccess.Store(s.clock().Unix())
	now := s.clock()
	s.mu.Lock()
	if s.seen == nil {
		s.seen = map[string]*ipSeen{}
	}
	if now.Sub(s.seenPrune) >= time.Minute { // 面板没打开、也不上报时，靠这里让表不无限增长
		s.pruneSeen(now)
		s.prunePlays(now) // 只有播放上报、没有视频经过本机时，播放表也要有人清
		s.seenPrune = now
	}
	c := s.seen[ip]
	if c == nil {
		c = &ipSeen{}
		s.seen[ip] = c
	}
	c.n++
	c.last = now
	s.mu.Unlock()
}

// end 结束一个本机处理的请求；play 是 startPlay 用的 key（不是视频请求则为空）。
func (s *Stats) end(ip, play string) {
	s.Active.Add(-1)
	s.mu.Lock()
	if c := s.seen[ip]; c != nil {
		c.n--
		c.last = s.clock()
	}
	s.mu.Unlock()
	if play != "" {
		s.endPlay(play)
	}
}

// ConnIP 是最近 ipRecent 内访问过的一个客户端 IP。
type ConnIP struct {
	IP   string `json:"ip"`
	N    int64  `json:"n"`    // 此刻在途的请求数
	Last int64  `json:"last"` // 最后访问时间（Unix 秒）
	Play bool   `json:"play"` // 正在播放
}

// sortConnIPs 排序：正在播放的在前，其次在途请求多的，再按最后访问时间从近到远。
func sortConnIPs(ips []ConnIP) {
	sort.Slice(ips, func(i, j int) bool {
		a, b := ips[i], ips[j]
		switch {
		case a.Play != b.Play:
			return a.Play
		case a.N != b.N:
			return a.N > b.N
		case a.Last != b.Last:
			return a.Last > b.Last
		}
		return a.IP < b.IP
	})
}

// pruneSeen 删掉没有在途请求、且超过 ipRecent 没再访问的 IP。调用方持有 s.mu。
func (s *Stats) pruneSeen(now time.Time) {
	for ip, c := range s.seen {
		if c.n <= 0 && now.Sub(c.last) >= ipRecent {
			delete(s.seen, ip)
		}
	}
}

func (s *Stats) connIPs() []ConnIP {
	now := s.clock()
	s.mu.Lock()
	s.pruneSeen(now)
	s.prunePlays(now)
	playing := map[string]bool{}
	for k := range s.plays {
		ip, _, _ := strings.Cut(k, "|")
		playing[ip] = true
	}
	for k := range s.reports {
		ip, _, _ := strings.Cut(k, "|")
		playing[ip] = true
	}
	out := make([]ConnIP, 0, len(s.seen))
	for ip, c := range s.seen {
		out = append(out, ConnIP{IP: ip, N: max(c.n, 0), Last: c.last.Unix(), Play: playing[ip]})
	}
	s.mu.Unlock()
	sortConnIPs(out)
	return out
}

// playKey 生成播放去重的 key：客户端 IP + 片子。
// 片子优先取路径 /videos/{id}/ 里的 id（可带 /emby 前缀），取不到用整个路径（p 已小写、已解码、不含 query）。
func playKey(ip, p string) string {
	q := strings.TrimPrefix(p, "/emby")
	if rest, ok := strings.CutPrefix(q, "/videos/"); ok {
		if id, _, ok := strings.Cut(rest, "/"); ok && id != "" {
			return ip + "|" + id
		}
	}
	return ip + "|" + p
}

func (s *Stats) startPlay(key string) {
	s.mu.Lock()
	if s.plays == nil {
		s.plays = map[string]*playSess{}
	}
	s.prunePlays(s.clock())
	p := s.plays[key]
	if p == nil {
		p = &playSess{}
		s.plays[key] = p
	}
	p.n++
	s.mu.Unlock()
}

func (s *Stats) endPlay(key string) {
	s.mu.Lock()
	if p := s.plays[key]; p != nil {
		p.n--
		p.last = s.clock()
	}
	s.mu.Unlock()
}

// playReportPath 判断是否为客户端的播放上报：POST /Sessions/Playing（开始）、/Playing/Progress（进度）、
// /Playing/Stopped（结束）。p 是小写、已解码的路径，可带 /emby 前缀。stop 表示播放结束。
func playReportPath(method, p string) (stop, ok bool) {
	if method != http.MethodPost {
		return false, false
	}
	switch strings.TrimPrefix(p, "/emby") {
	case "/sessions/playing", "/sessions/playing/progress":
		return false, true
	case "/sessions/playing/stopped":
		return true, true
	}
	return false, false
}

// playReportItem 取播放上报里的片子 ID：先看 query 的 ItemId（不区分大小写），再看 JSON 请求体。
// 请求体只读前 64 KB，读过的部分接回去，反代时上游收到的内容不变。
func playReportItem(r *http.Request) string {
	for k, v := range r.URL.Query() {
		if strings.EqualFold(k, "itemid") && len(v) > 0 && v[0] != "" {
			return strings.ToLower(v[0])
		}
	}
	if r.Body == nil || r.Body == http.NoBody {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	r.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(b), r.Body), r.Body}
	var v struct{ ItemId any }
	if json.Unmarshal(b, &v) != nil || v.ItemId == nil {
		return ""
	}
	return strings.ToLower(fmt.Sprint(v.ItemId))
}

// reportPlay 记录一次播放上报（key 见 playKey 的「IP|片子」格式）；stopped 为 true 表示播放结束。
func (s *Stats) reportPlay(key string, stopped bool) {
	s.mu.Lock()
	if stopped {
		delete(s.reports, key)
	} else {
		if s.reports == nil {
			s.reports = map[string]time.Time{}
		}
		s.reports[key] = s.clock()
	}
	s.mu.Unlock()
}

// prunePlays 删掉已结束超过 playLinger 的会话和超过 playReportTTL 没再上报的播放。调用方持有 s.mu。
func (s *Stats) prunePlays(now time.Time) {
	for k, p := range s.plays {
		if p.n <= 0 && now.Sub(p.last) >= playLinger {
			delete(s.plays, k)
		}
	}
	for k, t := range s.reports {
		if now.Sub(t) >= playReportTTL {
			delete(s.reports, k)
		}
	}
}

// streams 返回去重后的播放会话数（顺带清理过期会话）。
// 视频响应和播放上报是同一次播放的两路信号，两边的片子 key 不一定对得上（如推流走额外转发规则），
// 所以按客户端 IP 分组、每个 IP 取两路里较多的那个，同一次播放不会算两次。
func (s *Stats) streams() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunePlays(s.clock())
	ipOf := func(k string) string { ip, _, _ := strings.Cut(k, "|"); return ip }
	vids, reps := map[string]int64{}, map[string]int64{}
	for k := range s.plays {
		vids[ipOf(k)]++
	}
	for k := range s.reports {
		reps[ipOf(k)]++
	}
	var n int64
	for ip, v := range vids {
		n += max(v, reps[ip])
	}
	for ip, r := range reps {
		if _, ok := vids[ip]; !ok {
			n += r
		}
	}
	return n
}

// clear 清零累计值（请求、流量、错误、转发）和流量曲线；连接数、播放中是实时值，不清。
func (s *Stats) clear() {
	s.Requests.Store(0)
	s.Bytes.Store(0)
	s.Errors.Store(0)
	s.RelayRequests.Store(0)
	s.RelayBytes.Store(0)
	s.mu.Lock()
	s.buckets = [60]int64{}
	s.mu.Unlock()
}

func (s *Stats) addBytes(n int64) {
	if n <= 0 {
		return
	}
	s.Bytes.Add(n)
	min := s.clock().Unix() / 60
	i := min % 60
	s.mu.Lock()
	if s.bucketMin[i] != min {
		s.bucketMin[i] = min
		s.buckets[i] = 0
	}
	s.buckets[i] += n
	s.mu.Unlock()
}

// series 返回最近 60 分钟每分钟的流量（旧 → 新）。
func (s *Stats) series() []int64 {
	now := s.clock().Unix() / 60
	out := make([]int64, 60)
	s.mu.Lock()
	for k := 0; k < 60; k++ {
		min := now - int64(59-k)
		i := min % 60
		if s.bucketMin[i] == min {
			out[k] = s.buckets[i]
		}
	}
	s.mu.Unlock()
	return out
}

type StatsView struct {
	Requests      int64    `json:"requests"`
	Bytes         int64    `json:"bytes"`
	Active        int64    `json:"active"`
	Streams       int64    `json:"streams"` // 去重后的播放会话数
	Errors        int64    `json:"errors"`
	LastAccess    int64    `json:"last_access"`
	RelayRequests int64    `json:"relay_requests"`
	RelayBytes    int64    `json:"relay_bytes"`
	Series        []int64  `json:"series"`
	IPs           []ConnIP `json:"ips"` // 最近 10 分钟访问过的客户端 IP
}

func (s *Stats) view() StatsView {
	return StatsView{
		Requests: s.Requests.Load(), Bytes: s.Bytes.Load(), Active: s.Active.Load(),
		Streams: s.streams(), Errors: s.Errors.Load(), LastAccess: s.LastAccess.Load(),
		RelayRequests: s.RelayRequests.Load(), RelayBytes: s.RelayBytes.Load(),
		Series: s.series(), IPs: s.connIPs(),
	}
}

type LogEntry struct {
	Time     int64  `json:"t"`
	Site     string `json:"site"`
	SiteName string `json:"site_name"`
	IP       string `json:"ip"`
	Method   string `json:"method"`
	Path     string `json:"path"`
	Status   int    `json:"status"`
	Bytes    int64  `json:"bytes"`
	Ms       int64  `json:"ms"`
	UA       string `json:"ua"`
	Err      string `json:"err,omitempty"`
	Node     string `json:"node,omitempty"`   // 「→ 名称」转发给了该节点；「← 名称」由该节点转来
	Server   string `json:"server,omitempty"` // 处理这个请求的服务器（主节点汇总各节点日志时填写）
	CC       string `json:"cc,omitempty"`     // 客户端 IP 的归属国家（有国家数据时）
}

// LogRing 是内存里的访问日志（满了覆盖最旧的）。第 k 条（从 0 计）存在 buf[k % len(buf)]。
type LogRing struct {
	mu    sync.Mutex
	buf   []LogEntry
	total int64 // 累计加入的条数
}

func newLogRing(n int) *LogRing { return &LogRing{buf: make([]LogEntry, n)} }

func (l *LogRing) add(e LogEntry) {
	l.mu.Lock()
	l.buf[l.total%int64(len(l.buf))] = e
	l.total++
	l.mu.Unlock()
}

// list 返回最新在前、满足 match（nil 表示全部）的日志，最多 limit 条。
func (l *LogRing) list(match func(*LogEntry) bool, limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := min(l.total, int64(len(l.buf)))
	out := make([]LogEntry, 0, min(int64(limit), n))
	for k := l.total - 1; k >= l.total-n && len(out) < limit; k-- {
		e := &l.buf[k%int64(len(l.buf))]
		if match == nil || match(e) {
			out = append(out, *e)
		}
	}
	return out
}

// since 返回序号 seq 之后新增的日志（按时间先后），最多 limit 条（更早的已被覆盖或超出的丢掉），以及新的序号。
// 从节点上报统计时用它把增量日志带给主节点。
func (l *LogRing) since(seq int64, limit int) ([]LogEntry, int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq > l.total { // 不会发生（序号和日志一起在内存里），防御一下
		seq = 0
	}
	start := max(seq, l.total-int64(len(l.buf)), l.total-int64(limit), 0)
	out := make([]LogEntry, 0, l.total-start)
	for k := start; k < l.total; k++ {
		out = append(out, l.buf[k%int64(len(l.buf))])
	}
	return out, l.total
}
