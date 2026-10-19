package main

import (
	"net"
	"net/http"
	"strings"
)

// followTransport 在「推流经本机中转」模式下，由服务端直接跟随推流请求的 30x 跳转，
// 客户端只看到一个 200/206 响应。
//
// 不少服务器（例如做了推流分离的公益服）对带 Range 的播放请求返回 307 跳到推流节点，
// 部分播放器（Exo 等）跟随跳转时会丢 Range 或处理不好 Location，表现为播放几秒卡住、无法拖动。
// 只对播放请求生效：m3u8 等需要相对路径解析的请求仍然把跳转交给客户端（Location 已被改写）。
type followTransport struct {
	base http.RoundTripper
	s    *SiteRT
	rc   *reqCtx
}

func isDirectStreamPath(p string) bool {
	p = strings.ToLower(p)
	if strings.Contains(p, ".m3u8") || strings.Contains(p, "/hls") {
		return false
	}
	if strings.Contains(p, "/videos/") || strings.Contains(p, "/audio/") {
		return strings.Contains(p, "/stream") || strings.Contains(p, "/original") || strings.Contains(p, "/download")
	}
	return strings.Contains(p, "/items/") && strings.HasSuffix(p, "/download")
}

// isMediaPath 判断是否为播放相关请求（直链、HLS 播放列表和分片、字幕等）。
func isMediaPath(p string) bool {
	p = strings.ToLower(p)
	if strings.Contains(p, "/videos/") || strings.Contains(p, "/audio/") || strings.Contains(p, "/hls") {
		return true
	}
	for _, ext := range []string{".m3u8", ".ts", ".m4s", ".mp4", ".mkv"} {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	return strings.Contains(p, "/items/") && strings.HasSuffix(p, "/download")
}

func shouldFollow(req *http.Request) bool {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		return false
	}
	return req.Header.Get("Range") != "" || isDirectStreamPath(req.URL.Path)
}

// parentDomain 取最后两段，用于判断跳转目标是否与上游同属一个域名（如 a.example.com 与 node1.example.com）。
func parentDomain(h string) string {
	if net.ParseIP(h) != nil {
		return h
	}
	parts := strings.Split(h, ".")
	n := 2
	// 国家域名下的二级后缀（a.com.cn、b.co.uk、c.com.tw）要多取一段，否则不同主体会被当成同一域名
	if len(parts) >= 3 && len(parts[len(parts)-1]) == 2 && secondLevel[parts[len(parts)-2]] {
		n = 3
	}
	if len(parts) <= n {
		return h
	}
	return strings.Join(parts[len(parts)-n:], ".")
}

var secondLevel = map[string]bool{
	"com": true, "net": true, "org": true, "edu": true, "gov": true, "co": true, "ac": true,
	"or": true, "ne": true, "go": true, "idv": true, "ltd": true, "plc": true, "me": true,
}

var credentialHeaders = []string{"Authorization", "X-Emby-Authorization", "X-Emby-Token", "X-Mediabrowser-Token", "Cookie"}

func (t *followTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.base.RoundTrip(req)
	if err != nil || !shouldFollow(req) {
		return resp, err
	}
	for hops := 0; hops < 5; hops++ {
		switch resp.StatusCode {
		case 301, 302, 303, 307, 308:
		default:
			return resp, nil
		}
		loc, err := req.URL.Parse(resp.Header.Get("Location"))
		if err != nil || (loc.Scheme != "http" && loc.Scheme != "https") || loc.Host == "" {
			return resp, nil
		}
		// 跳回本站自己的地址：在本机解析成真正的目标（目标来自站点配置，不做下面的内网地址限制，也不记为推流域名）
		self := false
		if u, ok := t.rc.resolveSelf(loc); ok {
			loc, self = u, true
		}
		host, port := splitHP(loc.Host, loc.Scheme)
		if !self {
			if t.rc.isSelf(loc.Scheme, host, port) { // 本站地址但解析不了（不在本站路径下、参数不合法）：交给客户端，不经公网打自己
				return resp, nil
			}
			if ip := net.ParseIP(host); ip != nil && (ip.IsLoopback() || ip.IsUnspecified() || ip.IsPrivate() || ip.IsLinkLocalUnicast()) {
				if up := net.ParseIP(t.s.upHost); up == nil || !(up.IsLoopback() || up.IsPrivate()) {
					return resp, nil
				}
			}
			if host == "localhost" {
				return resp, nil
			}
			if !t.s.isUpstream(loc.Scheme, host, port) {
				t.s.learnHost(hostKey(loc.Scheme, host, port))
			}
		}

		next := req.Clone(req.Context())
		next.URL = loc
		next.Host = loc.Host
		next.Body, next.ContentLength = nil, 0
		if resp.StatusCode == 303 {
			next.Method = http.MethodGet
		}
		// 跳到与上游无关的第三方域名（网盘直链等）时，不把 Emby 凭据带过去
		if parentDomain(host) != parentDomain(t.s.upHost) {
			for _, k := range credentialHeaders {
				next.Header.Del(k)
			}
		}
		next.Header.Del("Origin")
		next.Header.Del("Referer")
		resp.Body.Close()
		req = next
		if resp, err = t.base.RoundTrip(req); err != nil {
			return nil, err
		}
	}
	return resp, nil
}
