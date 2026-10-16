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
	Node     string `json:"node,omitempty"` // 「→ 名称」转发给了该节点；「← 名称」由该节点转来
}

type LogRing struct {
	mu   sync.Mutex
	buf  []LogEntry
	next int
	full bool
}

func newLogRing(n int) *LogRing { return &LogRing{buf: make([]LogEntry, n)} }

func (l *LogRing) add(e LogEntry) {
	l.mu.Lock()
	l.buf[l.next] = e
	l.next = (l.next + 1) % len(l.buf)
	if l.next == 0 {
		l.full = true
	}
	l.mu.Unlock()
}

// list 返回最新在前的日志。
func (l *LogRing) list(site string, limit int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := l.next
	if l.full {
		n = len(l.buf)
	}
	out := make([]LogEntry, 0, min(limit, n))
	for k := 0; k < n && len(out) < limit; k++ {
		i := (l.next - 1 - k + len(l.buf)) % len(l.buf)
		if site == "" || l.buf[i].Site == site {
			out = append(out, l.buf[i])
		}
	}
	return out
}
