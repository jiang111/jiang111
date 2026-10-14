package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// Route 是站点的额外转发规则，用于服务器自带的推流路径。
// 例如某些服务器要求 /magpiecdn?load=节点 转发到 https://节点.example.com：
//
//	{"path": "/magpiecdn", "target": "https://{arg.load}.example.com"}
//
// 请求路径和参数原样转发（等同 nginx 的 proxy_pass 不带 URI）。
type Route struct {
	Path   string `json:"path"`
	Target string `json:"target"`
}

var (
	routeVarRe = regexp.MustCompile(`\{arg\.([A-Za-z0-9_-]+)\}`)
	labelRe    = regexp.MustCompile(`^[A-Za-z0-9-]{1,63}$`)
)

func validateRoutes(rs []Route) ([]Route, error) {
	out := []Route{}
	for _, r := range rs {
		r.Path, r.Target = strings.TrimSpace(r.Path), strings.TrimSpace(r.Target)
		if r.Path == "" && r.Target == "" {
			continue
		}
		if !strings.HasPrefix(r.Path, "/") || strings.HasPrefix(r.Path, epPrefix) {
			return nil, fmt.Errorf("路由路径 %q 必须以 / 开头", r.Path)
		}
		u, err := url.Parse(routeVarRe.ReplaceAllString(r.Target, "1"))
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, fmt.Errorf("路由目标 %q 格式不正确，应为 https://域名，可用 {arg.参数名} 取请求参数", r.Target)
		}
		out = append(out, r)
	}
	return out, nil
}

// matchRoute 返回匹配的转发目标（scheme://host[:port]）。
func (s *SiteRT) matchRoute(rest string, r *http.Request) (*url.URL, bool, error) {
	low := strings.ToLower(rest)
	for _, rt := range s.cfg.Routes {
		p := strings.ToLower(rt.Path)
		if low != p && !strings.HasPrefix(low, strings.TrimSuffix(p, "/")+"/") {
			continue
		}
		var bad error
		target := routeVarRe.ReplaceAllStringFunc(rt.Target, func(m string) string {
			v := r.URL.Query().Get(routeVarRe.FindStringSubmatch(m)[1])
			// 只允许单个域名标签，防止通过参数把请求转发到任意主机
			if !labelRe.MatchString(v) {
				bad = errors.New("bad route argument")
			}
			return v
		})
		if bad != nil {
			return nil, true, bad
		}
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return nil, true, errors.New("bad route target")
		}
		h, p2 := splitHP(u.Host, u.Scheme)
		return &url.URL{Scheme: u.Scheme, Host: joinHP(u.Scheme, h, p2)}, true, nil
	}
	return nil, false, nil
}

// expandHeader 替换自定义请求头里的占位符。
func (rc *reqCtx) expandHeader(v, ip string) string {
	if !strings.Contains(v, "{") {
		return v
	}
	origin := rc.publicBase
	if i := strings.Index(origin, "://"); i >= 0 {
		if j := strings.IndexByte(origin[i+3:], '/'); j >= 0 {
			origin = origin[:i+3+j]
		}
	}
	return strings.NewReplacer("{public_base}", rc.publicBase, "{public_origin}", origin, "{client_ip}", ip).Replace(v)
}
