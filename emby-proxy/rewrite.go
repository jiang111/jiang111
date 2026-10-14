package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
)

const maxRewrite = 16 << 20

var absURLRe = regexp.MustCompile(`(?i)https?://[^\s"'<>\\]+`)

func bodyKind(ct, path string) string {
	switch {
	case strings.Contains(ct, "mpegurl") || strings.HasSuffix(path, ".m3u8"):
		return "m3u8"
	case strings.Contains(ct, "json"):
		return "json"
	case strings.HasPrefix(ct, "text/") && !strings.Contains(ct, "event-stream"):
		return "text"
	case strings.Contains(ct, "xml"):
		return "text"
	}
	return ""
}

func isPlaybackInfo(p string) bool {
	return strings.HasSuffix(p, "/playbackinfo") || strings.Contains(p, "/playbackinfo/")
}

func isSystemInfo(p string) bool {
	return strings.HasSuffix(p, "/system/info") || strings.HasSuffix(p, "/system/info/public")
}

type readCloser struct {
	io.Reader
	io.Closer
}

// rewriteBody 改写 JSON / m3u8 / 文本响应里的上游地址。
// PlaybackInfo、m3u8 以及推流域名的响应额外允许改写外部推流域名（推流分离）。
func (rc *reqCtx) rewriteBody(resp *http.Response, ct string) error {
	if resp.Request.Method == http.MethodHead {
		return nil
	}
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotModified, http.StatusPartialContent, http.StatusSwitchingProtocols:
		return nil
	}
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if (enc != "" && enc != "gzip" && enc != "identity") || resp.ContentLength > maxRewrite {
		return nil
	}
	kind := bodyKind(ct, rc.matchPath)
	if kind == "" {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRewrite+1))
	if err != nil {
		return err
	}
	if len(raw) > maxRewrite {
		resp.Body = readCloser{io.MultiReader(bytes.NewReader(raw), resp.Body), resp.Body}
		return nil
	}
	resp.Body.Close()
	data := raw
	if enc == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(zr, 4*maxRewrite+1))
		}
		if err != nil || len(data) > 4*maxRewrite {
			// 解压失败或太大：原样转发
			resp.Body = io.NopCloser(bytes.NewReader(raw))
			return nil
		}
	}

	allowExt := kind == "m3u8" || rc.ep || isPlaybackInfo(rc.matchPath)
	out := data
	if kind != "m3u8" && isSystemInfo(rc.matchPath) {
		out = rc.fixSystemInfo(out)
	}
	needle := []byte(rc.s.upHost)
	if allowExt || bytes.Contains(out, needle) || bytes.Contains(bytes.ToLower(out), needle) {
		out = absURLRe.ReplaceAllFunc(out, func(m []byte) []byte {
			return []byte(rc.rewriteAbs(string(m), allowExt))
		})
	}

	h := resp.Header
	h.Del("Content-Encoding")
	if rc.acceptGzip && len(out) > 1400 {
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestSpeed)
		zw.Write(out)
		zw.Close()
		out = buf.Bytes()
		h.Set("Content-Encoding", "gzip")
		h.Add("Vary", "Accept-Encoding")
	}
	h.Del("Content-Md5")
	resp.Body = io.NopCloser(bytes.NewReader(out))
	resp.ContentLength = int64(len(out))
	h.Set("Content-Length", strconv.Itoa(len(out)))
	return nil
}

// fixSystemInfo 把 /System/Info(/Public) 里的内外网地址改成反代地址，
// 否则部分客户端（如官方 App）会尝试直连上游的局域网/公网地址。
func (rc *reqCtx) fixSystemInfo(data []byte) []byte {
	var m map[string]any
	if json.Unmarshal(data, &m) != nil {
		return data
	}
	changed := false
	for _, k := range []string{"LocalAddress", "WanAddress"} {
		if _, ok := m[k]; ok {
			m[k] = rc.publicBase
			changed = true
		}
	}
	for _, k := range []string{"LocalAddresses", "RemoteAddresses"} {
		if v, ok := m[k].([]any); ok && len(v) > 0 {
			m[k] = []string{rc.publicBase}
			changed = true
		}
	}
	if !changed {
		return data
	}
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}
