package main

import (
	"bytes"
	"container/list"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 海报 / 图片缓存：Emby 的图片地址带 tag（图片内容的指纹，图片换了 tag 就变），同一个地址的内容不会变，
// 可以放心缓存。客户端翻海报墙时大量请求 /Items/{id}/Images/Primary?tag=…，命中缓存后：
//   - 本机直接返回，不访问上游，翻页更快，也省上游的带宽和请求数；
//   - 多台服务器时，落到非负责节点的图片请求在本机命中就不必再转给负责节点。
//
// 只缓存：GET、不带 Range、路径含 /images/ 且参数里有 tag、上游返回 200 的 image/*、没有 Set-Cookie、
// 没有 Cache-Control: private / no-store、不超过 2MB。缓存 key 去掉 api_key 等凭据参数，不同用户共用。
// 只在内存里（LRU，默认 64MB，环境变量 IMAGE_CACHE_MB 调整，0 = 关闭），重启清空，各台服务器各自缓存。

const (
	imgMaxEntry = 2 << 20
	imgTTL      = 24 * time.Hour
)

func imageCacheBytes() int64 {
	if v := os.Getenv("IMAGE_CACHE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return int64(n) << 20
		}
	}
	return 64 << 20
}

type imgEntry struct {
	key  string
	body []byte
	hdr  http.Header
	exp  time.Time
}

type imageCache struct {
	max          int64
	mu           sync.Mutex
	size         int64
	ll           *list.List
	items        map[string]*list.Element
	hits, misses atomic.Int64
	saved        atomic.Int64 // 命中缓存省下的上游流量
}

func newImageCache(max int64) *imageCache {
	return &imageCache{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

// 不进缓存 key 的参数：凭据和设备信息（不同用户请求同一张图时共用缓存）
var imgSkipParams = map[string]bool{
	"api_key": true, "x-emby-token": true, "x-mediabrowser-token": true, "token": true, "access_token": true,
	"x-emby-client": true, "x-emby-client-version": true, "x-emby-device-id": true, "x-emby-device-name": true,
	"x-emby-language": true, "x-emby-authorization": true,
}

// imageKey 判断请求能否走图片缓存，能则返回缓存 key。matchPath 为小写、已解码、去掉前缀和 base 的路径。
func imageKey(site string, r *http.Request, matchPath string) string {
	if r.Method != http.MethodGet || r.Header.Get("Range") != "" || r.Header.Get("Upgrade") != "" {
		return ""
	}
	if !strings.Contains(matchPath, "/images/") || strings.HasPrefix(matchPath, epPrefix) {
		return ""
	}
	q, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return ""
	}
	keys := make([]string, 0, len(q))
	hasTag := false
	for k := range q {
		lk := strings.ToLower(k)
		if lk == "tag" {
			hasTag = true
		}
		if !imgSkipParams[lk] {
			keys = append(keys, k)
		}
	}
	if !hasTag {
		return ""
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(site + "|" + matchPath + "?")
	for _, k := range keys {
		for _, v := range q[k] {
			b.WriteString(strings.ToLower(k) + "=" + v + "&")
		}
	}
	return b.String()
}

func (c *imageCache) get(key string) *imgEntry {
	if c == nil || c.max <= 0 || key == "" {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el := c.items[key]
	if el == nil {
		c.misses.Add(1)
		return nil
	}
	e := el.Value.(*imgEntry)
	if time.Now().After(e.exp) {
		c.removeLocked(el)
		c.misses.Add(1)
		return nil
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	return e
}

func (c *imageCache) put(key string, body []byte, hdr http.Header) {
	if c == nil || c.max <= 0 || int64(len(body)) > c.max/4 {
		return
	}
	e := &imgEntry{key: key, body: body, hdr: hdr, exp: time.Now().Add(imgTTL)}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el := c.items[key]; el != nil {
		c.removeLocked(el)
	}
	c.items[key] = c.ll.PushFront(e)
	c.size += int64(len(body))
	for c.size > c.max {
		c.removeLocked(c.ll.Back())
	}
}

func (c *imageCache) removeLocked(el *list.Element) {
	e := el.Value.(*imgEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.size -= int64(len(e.body))
}

func (c *imageCache) clear() {
	c.mu.Lock()
	c.ll.Init()
	c.items = map[string]*list.Element{}
	c.size = 0
	c.mu.Unlock()
}

type imageCacheView struct {
	Enabled bool  `json:"enabled"`
	Entries int   `json:"entries"`
	Bytes   int64 `json:"bytes"`
	Max     int64 `json:"max"`
	Hits    int64 `json:"hits"`
	Misses  int64 `json:"misses"`
	Saved   int64 `json:"saved"`
}

func (c *imageCache) view() imageCacheView {
	c.mu.Lock()
	defer c.mu.Unlock()
	return imageCacheView{Enabled: c.max > 0, Entries: len(c.items), Bytes: c.size, Max: c.max,
		Hits: c.hits.Load(), Misses: c.misses.Load(), Saved: c.saved.Load()}
}

// serveHit 用缓存回应，支持 If-None-Match（客户端自己缓存过就回 304）。
func (e *imgEntry) serve(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	for k, v := range e.hdr {
		h[k] = v
	}
	h.Set("X-Cache", "HIT")
	if et := e.hdr.Get("Etag"); et != "" && r.Header.Get("If-None-Match") == et {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	h.Set("Content-Length", strconv.Itoa(len(e.body)))
	w.WriteHeader(http.StatusOK)
	w.Write(e.body)
}

// cacheable 判断上游响应能否进图片缓存。
func cacheableImage(resp *http.Response) bool {
	h := resp.Header
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(strings.ToLower(h.Get("Content-Type")), "image/") {
		return false
	}
	if enc := strings.ToLower(h.Get("Content-Encoding")); enc != "" && enc != "identity" {
		return false
	}
	cc := strings.ToLower(h.Get("Cache-Control"))
	if h.Get("Set-Cookie") != "" || strings.Contains(cc, "no-store") || strings.Contains(cc, "private") || h.Get("Vary") == "*" {
		return false
	}
	return resp.ContentLength <= imgMaxEntry
}

var imgKeepHeaders = []string{"Content-Type", "Etag", "Last-Modified", "Cache-Control", "Expires", "Age"}

// teeImage 边转发边收集图片内容，完整读完（且不超过上限）时放进缓存。
func (c *imageCache) tee(key string, resp *http.Response) {
	hdr := http.Header{}
	for _, k := range imgKeepHeaders {
		if v := resp.Header.Values(k); len(v) > 0 {
			hdr[k] = append([]string(nil), v...)
		}
	}
	hdr.Del("Age")
	resp.Body = &imgTee{ReadCloser: resp.Body, c: c, key: key, hdr: hdr, want: resp.ContentLength}
}

type imgTee struct {
	io.ReadCloser
	c    *imageCache
	key  string
	hdr  http.Header
	want int64
	buf  bytes.Buffer
	bad  bool
	done bool
}

func (t *imgTee) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	if !t.bad && n > 0 {
		if t.buf.Len()+n > imgMaxEntry {
			t.bad = true
			t.buf = bytes.Buffer{}
		} else {
			t.buf.Write(p[:n])
		}
	}
	if err == io.EOF && !t.bad && !t.done && (t.want < 0 || int64(t.buf.Len()) == t.want) {
		t.done = true
		t.c.put(t.key, bytes.Clone(t.buf.Bytes()), t.hdr)
	}
	return n, err
}
