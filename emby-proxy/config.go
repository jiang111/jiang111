package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Site 是一个反代站点：把一台上游 Emby 服务器通过 端口 / 路径 / 域名 暴露给客户端。
type Site struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Upstream string `json:"upstream"` // 上游地址，如 https://emby.example.com 或 http://1.2.3.4:8096

	Mode string `json:"mode"`           // port | path | host
	Port int    `json:"port,omitempty"` // mode=port：独立监听端口
	Path string `json:"path,omitempty"` // mode=path：入口端口下的路径前缀（不带斜杠）
	Host string `json:"host,omitempty"` // mode=host：绑定的域名

	PublicURL string `json:"public_url,omitempty"` // 可选：客户端实际访问地址，留空自动识别

	StreamMode  string   `json:"stream_mode"`            // proxy：推流也走本机；direct：推流 302 让客户端直连
	StreamHosts []string `json:"stream_hosts,omitempty"` // 推流分离域名（自动学习 + 手动添加）

	ForwardIP   bool              `json:"forward_ip"`   // 把客户端真实 IP 传给上游
	InsecureTLS bool              `json:"insecure_tls"` // 不校验上游证书
	UserAgent   string            `json:"user_agent,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Remark      string            `json:"remark,omitempty"`
}

type Config struct {
	Listen        string  `json:"listen"`       // 路径/域名模式共用的入口
	AdminListen   string  `json:"admin_listen"` // 管理面板
	AdminPassword string  `json:"admin_password"`
	Secret        string  `json:"secret"`
	EntryURL      string  `json:"entry_url,omitempty"`   // 入口的公网地址（仅用于面板显示客户端地址）
	PublicHost    string  `json:"public_host,omitempty"` // 端口模式显示用的服务器 IP/域名
	Sites         []*Site `json:"sites"`
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func randPassword(n int) string {
	const chars = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = chars[int(b[i])%len(chars)]
	}
	return string(b)
}

// LoadConfig 读取配置，不存在时生成默认配置（随机密码）。
func LoadConfig(path string) (*Config, bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		cfg := &Config{
			Listen:        envOr("LISTEN", ":55661"),
			AdminListen:   envOr("ADMIN_LISTEN", ":55660"),
			AdminPassword: envOr("ADMIN_PASSWORD", randPassword(12)),
			Secret:        randHex(32),
			Sites:         []*Site{},
		}
		return cfg, true, SaveConfig(path, cfg)
	}
	if err != nil {
		return nil, false, err
	}
	cfg := &Config{}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, false, fmt.Errorf("解析 %s 失败: %w", path, err)
	}
	changed := false
	if cfg.Listen == "" {
		cfg.Listen, changed = ":55661", true
	}
	if cfg.AdminListen == "" {
		cfg.AdminListen, changed = ":55660", true
	}
	if cfg.AdminPassword == "" {
		cfg.AdminPassword, changed = randPassword(12), true
	}
	if cfg.Secret == "" {
		cfg.Secret, changed = randHex(32), true
	}
	for _, s := range cfg.Sites {
		if s.ID == "" {
			s.ID, changed = randHex(4), true
		}
		normalizeSite(s)
	}
	if changed {
		err = SaveConfig(path, cfg)
	}
	return cfg, false, err
}

func SaveConfig(path string, cfg *Config) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func listenPort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	p, _ := strconv.Atoi(addr[i+1:])
	return p
}

var pathRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
var hostRe = regexp.MustCompile(`^[a-z0-9.-]{1,253}$`)

func normalizeSite(s *Site) {
	s.Name = strings.TrimSpace(s.Name)
	s.Upstream = strings.TrimSpace(s.Upstream)
	if s.Upstream != "" && !strings.Contains(s.Upstream, "://") {
		s.Upstream = "http://" + s.Upstream
	}
	s.Mode = strings.ToLower(strings.TrimSpace(s.Mode))
	if s.Mode == "" {
		s.Mode = "port"
	}
	s.Path = strings.Trim(strings.TrimSpace(s.Path), "/")
	s.Host = strings.ToLower(strings.TrimSpace(s.Host))
	if i := strings.Index(s.Host, "://"); i >= 0 {
		s.Host = s.Host[i+3:]
	}
	s.Host = strings.TrimRight(s.Host, "/")
	if h, _, ok := strings.Cut(s.Host, ":"); ok {
		s.Host = h
	}
	s.PublicURL = strings.TrimRight(strings.TrimSpace(s.PublicURL), "/")
	if s.StreamMode != "direct" {
		s.StreamMode = "proxy"
	}
	hosts := s.StreamHosts[:0]
	seen := map[string]bool{}
	for _, h := range s.StreamHosts {
		h = strings.ToLower(strings.TrimRight(strings.TrimSpace(h), "/"))
		if h != "" && !seen[h] {
			seen[h] = true
			hosts = append(hosts, h)
		}
	}
	s.StreamHosts = hosts
	for k, v := range s.Headers {
		if strings.TrimSpace(k) == "" {
			delete(s.Headers, k)
			continue
		}
		s.Headers[k] = strings.TrimSpace(v)
	}
}

// validateSite 校验站点，others 为其他站点（用于检查端口/路径/域名冲突）。
func validateSite(cfg *Config, s *Site) error {
	normalizeSite(s)
	if s.Name == "" {
		return errors.New("请填写名称")
	}
	if _, err := parseUpstream(s.Upstream); err != nil {
		return err
	}
	if s.PublicURL != "" {
		u, err := url.Parse(s.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return errors.New("客户端访问地址格式不正确，应为 http(s)://域名[:端口][/路径]")
		}
	}
	for _, o := range cfg.Sites {
		if o.ID == s.ID {
			continue
		}
		switch {
		case s.Mode == "port" && o.Mode == "port" && o.Port == s.Port:
			return fmt.Errorf("端口 %d 已被站点「%s」使用", s.Port, o.Name)
		case s.Mode == "path" && o.Mode == "path" && strings.EqualFold(o.Path, s.Path):
			return fmt.Errorf("路径 /%s 已被站点「%s」使用", s.Path, o.Name)
		case s.Mode == "host" && o.Mode == "host" && o.Host == s.Host:
			return fmt.Errorf("域名 %s 已被站点「%s」使用", s.Host, o.Name)
		}
	}
	switch s.Mode {
	case "port":
		if s.Port < 1 || s.Port > 65535 {
			return errors.New("端口需在 1-65535 之间")
		}
		if s.Port == listenPort(cfg.Listen) || s.Port == listenPort(cfg.AdminListen) {
			return fmt.Errorf("端口 %d 与入口/面板端口冲突", s.Port)
		}
		s.Path, s.Host = "", ""
	case "path":
		if !pathRe.MatchString(s.Path) || strings.HasPrefix(s.Path, "__") {
			return errors.New("路径只能包含字母、数字、_ . -，且不能以 __ 开头")
		}
		switch strings.ToLower(s.Path) {
		case "emby", "web", "system", "users", "items", "videos", "audio", "sessions", "socket", "embywebsocket":
			return fmt.Errorf("路径 /%s 与 Emby 自身路径冲突，请换一个", s.Path)
		}
		s.Port, s.Host = 0, ""
	case "host":
		if !hostRe.MatchString(s.Host) {
			return errors.New("域名格式不正确")
		}
		s.Port, s.Path = 0, ""
	default:
		return errors.New("未知的接入方式")
	}
	return nil
}
