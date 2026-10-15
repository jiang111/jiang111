package main

import (
	"sync"
	"sync/atomic"
	"time"
)

type Stats struct {
	Requests   atomic.Int64
	Bytes      atomic.Int64
	Active     atomic.Int64
	Streams    atomic.Int64
	Errors     atomic.Int64
	LastAccess atomic.Int64

	mu        sync.Mutex
	buckets   [60]int64
	bucketMin [60]int64
}

func (s *Stats) begin() {
	s.Requests.Add(1)
	s.Active.Add(1)
	s.LastAccess.Store(time.Now().Unix())
}

func (s *Stats) end(streaming bool) {
	s.Active.Add(-1)
	if streaming {
		s.Streams.Add(-1)
	}
}

func (s *Stats) addBytes(n int64) {
	if n <= 0 {
		return
	}
	s.Bytes.Add(n)
	min := time.Now().Unix() / 60
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
	now := time.Now().Unix() / 60
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
	Requests   int64   `json:"requests"`
	Bytes      int64   `json:"bytes"`
	Active     int64   `json:"active"`
	Streams    int64   `json:"streams"`
	Errors     int64   `json:"errors"`
	LastAccess int64   `json:"last_access"`
	Series     []int64 `json:"series"`
}

func (s *Stats) view() StatsView {
	return StatsView{
		Requests: s.Requests.Load(), Bytes: s.Bytes.Load(), Active: s.Active.Load(),
		Streams: s.Streams.Load(), Errors: s.Errors.Load(), LastAccess: s.LastAccess.Load(),
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
