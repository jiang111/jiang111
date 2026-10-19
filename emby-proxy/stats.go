package main

import (
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// playLinger：某部片子最后一个视频请求结束后，仍算作「播放中」的时间（HLS 分段、Range 重连之间的空档）
const playLinger = 15 * time.Second

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
	plays     map[string]*playSess // 客户端 IP + 片子 → 播放会话
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

func (s *Stats) begin() {
	s.Requests.Add(1)
	s.Active.Add(1)
	s.LastAccess.Store(s.clock().Unix())
}

// end 结束一个本机处理的请求；play 是 startPlay 用的 key（不是视频请求则为空）。
func (s *Stats) end(play string) {
	s.Active.Add(-1)
	if play != "" {
		s.endPlay(play)
	}
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

// prunePlays 删掉已结束超过 playLinger 的会话。调用方持有 s.mu。
func (s *Stats) prunePlays(now time.Time) {
	for k, p := range s.plays {
		if p.n <= 0 && now.Sub(p.last) >= playLinger {
			delete(s.plays, k)
		}
	}
}

// streams 返回去重后的播放会话数（顺带清理过期会话）。
func (s *Stats) streams() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prunePlays(s.clock())
	return int64(len(s.plays))
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
	Requests      int64   `json:"requests"`
	Bytes         int64   `json:"bytes"`
	Active        int64   `json:"active"`
	Streams       int64   `json:"streams"` // 去重后的播放会话数
	Errors        int64   `json:"errors"`
	LastAccess    int64   `json:"last_access"`
	RelayRequests int64   `json:"relay_requests"`
	RelayBytes    int64   `json:"relay_bytes"`
	Series        []int64 `json:"series"`
}

func (s *Stats) view() StatsView {
	return StatsView{
		Requests: s.Requests.Load(), Bytes: s.Bytes.Load(), Active: s.Active.Load(),
		Streams: s.streams(), Errors: s.Errors.Load(), LastAccess: s.LastAccess.Load(),
		RelayRequests: s.RelayRequests.Load(), RelayBytes: s.RelayBytes.Load(),
		Series: s.series(),
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
