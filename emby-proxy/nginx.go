package main

import (
	"bytes"
	"crypto/sha256"
	"crypto/x509"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// nginxConfig 为「独立端口」站点生成 nginx 配置：nginx 在公网端口终结 HTTPS，转发到本机的站点端口。
// 站点需要填写「客户端访问地址」（如 https://emby.example.com:8001），据此决定 server_name 和对外端口。
// cert/key 为证书路径（空则用 cfg 里的），leaf 为当前证书（可为 nil），用于提示证书是否覆盖站点域名。
func nginxConfig(cfg *Config, cert, key string, leaf *x509.Certificate) string {
	if cert == "" {
		cert, key = cfg.NginxCert, cfg.NginxKey
	}
	if cert == "" {
		cert = "/etc/nginx/ssl/fullchain.pem"
	}
	if key == "" {
		key = "/etc/nginx/ssl/privkey.pem"
	}
	backend := cfg.PortBind
	if backend == "" || backend == "0.0.0.0" || backend == "::" {
		backend = "127.0.0.1"
	}
	if strings.Contains(backend, ":") {
		backend = "[" + backend + "]"
	}

	var b strings.Builder
	b.WriteString(`# 由 Emby 反代面板自动生成，请勿手动修改（修改会被覆盖）
# 每个站点一个端口：nginx 在对外端口做 HTTPS，转发给本机的站点端口
`)
	if leaf != nil {
		sum := sha256.Sum256(leaf.Raw)
		fmt.Fprintf(&b, "# 证书：%s，到期 %s，指纹 %x\n", strings.Join(leaf.DNSNames, ","), leaf.NotAfter.Format("2006-01-02"), sum[:8])
	}
	b.WriteString(`

map $http_upgrade $emby_proxy_connection {
    default upgrade;
    ''      close;
}

ssl_session_cache   shared:EmbyProxySSL:10m;
ssl_session_timeout 1d;
`)
	// 本机直连域名（视频直连时其它节点把推流请求 302 到这里）：加到每个端口站点的 server_name
	direct := cfg.Cluster.DirectHost
	if direct != "" && leaf != nil && leaf.VerifyHostname(direct) != nil {
		fmt.Fprintf(&b, "\n# 注意：证书不包含本机直连域名 %s，视频直连时客户端会报证书错误\n", direct)
	}
	used := map[string]string{}
	for _, s := range cfg.Sites {
		if s.Mode != "port" {
			continue
		}
		fmt.Fprintf(&b, "\n# %s → %s\n", oneLine(s.Name), oneLine(s.Upstream))
		u, err := url.Parse(s.PublicURL)
		if s.PublicURL == "" || err != nil || u.Scheme != "https" {
			b.WriteString("# 跳过：请在站点「高级选项 → 客户端访问地址」填写 https://域名:对外端口\n")
			continue
		}
		host, port := u.Hostname(), u.Port()
		if port == "" {
			port = "443"
		}
		if prev, ok := used[port]; ok && prev != host {
			fmt.Fprintf(&b, "# 注意：端口 %s 已被其它域名使用，同端口多域名需要证书都覆盖这些域名\n", port)
		}
		used[port] = host
		if leaf != nil && leaf.VerifyHostname(host) != nil {
			fmt.Fprintf(&b, "# 注意：证书不包含域名 %s，客户端会报证书错误\n", host)
		}
		if port == fmt.Sprint(s.Port) {
			b.WriteString("# 跳过：对外端口不能和站点端口相同（Linux 上会端口冲突），例如对外 8001、站点 18001\n")
			continue
		}
		if !s.Enabled {
			b.WriteString("# 注意：该站点当前已停用\n")
		}
		names := host
		if direct != "" && !strings.EqualFold(direct, host) {
			names += " " + direct
		}
		fmt.Fprintf(&b, `server {
    listen %[1]s ssl;
    %[7]slisten [::]:%[1]s ssl;%[8]s
    server_name %[2]s;

    ssl_certificate     %[3]s;
    ssl_certificate_key %[4]s;
    ssl_protocols       TLSv1.2 TLSv1.3;

    client_max_body_size 0;

    location / {
        proxy_pass http://%[5]s:%[6]d;
        proxy_http_version 1.1;
        proxy_set_header Host $host:$server_port;
        proxy_set_header X-Forwarded-Host $host:$server_port;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header Upgrade $http_upgrade;
        proxy_set_header Connection $emby_proxy_connection;

        proxy_buffering off;
        proxy_request_buffering off;
        proxy_max_temp_file_size 0;
        proxy_read_timeout 86400s;
        proxy_send_timeout 86400s;
    }
}
`, port, names, cert, key, backend, s.Port, ipv6Prefix(cfg), ipv6Note(cfg))
	}

	adminPort := listenPort(cfg.AdminListen)
	if u, err := url.Parse(cfg.PanelURL); cfg.PanelURL != "" && err == nil && u.Scheme == "https" && u.Hostname() != "" {
		port := u.Port()
		if port == "" {
			port = "443"
		}
		if _, clash := used[port]; clash {
			fmt.Fprintf(&b, "\n# 跳过面板：端口 %s 已被站点使用，请在「设置」里给面板换一个端口\n", port)
		} else {
			if leaf != nil && leaf.VerifyHostname(u.Hostname()) != nil {
				fmt.Fprintf(&b, "\n# 注意：证书不包含面板域名 %s", u.Hostname())
			}
			fmt.Fprintf(&b, `
# 面板（多服务器同步时，从节点通过这里连接主节点）
server {
    listen %[1]s ssl;
    server_name %[2]s;
    ssl_certificate     %[3]s;
    ssl_certificate_key %[4]s;
    ssl_protocols       TLSv1.2 TLSv1.3;
    client_max_body_size 8m;
    location / {
        proxy_pass http://127.0.0.1:%[5]d;
        proxy_set_header Host $host:$server_port;
        proxy_set_header X-Forwarded-Host $host:$server_port;
        proxy_set_header X-Forwarded-Proto https;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
    }
}
`, port, u.Hostname(), cert, key, adminPort)
		}
	} else {
		fmt.Fprintf(&b, `
# 面板 HTTPS：在面板「设置 → 多服务器」填写「面板 HTTPS 地址」后自动生成，并把 config.json 的 admin_listen 改为 127.0.0.1:%d
`, adminPort)
	}
	return b.String()
}

func ipv6Prefix(cfg *Config) string {
	if cfg.NginxIPv6 {
		return ""
	}
	return "# "
}

func ipv6Note(cfg *Config) string {
	if cfg.NginxIPv6 {
		return ""
	}
	return "   # 服务器有 IPv6 时在面板「设置 → Nginx」勾选「监听 IPv6」"
}

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func validBind(s string) bool {
	return s == "" || net.ParseIP(s) != nil
}

// nginxConf 生成当前配置对应的 nginx 配置。没有手动指定证书路径但上传过证书时，自动使用上传的证书。
func (m *Manager) nginxConf() string {
	leaf := m.uploadedCert()
	m.mu.RLock()
	defer m.mu.RUnlock()
	hostCert, hostKey := filepath.Join(m.hostSSLDir(), "fullchain.pem"), filepath.Join(m.hostSSLDir(), "privkey.pem")
	cert, key := m.cfg.NginxCert, m.cfg.NginxKey
	if cert == "" && leaf != nil {
		cert, key = hostCert, hostKey
	}
	if cert != hostCert {
		leaf = nil // 用的是手动填写的路径，程序在容器里读不到，无法确认证书内容
	}
	return nginxConfig(m.cfg, cert, key, leaf)
}

func (m *Manager) nginxConfPath() string {
	return filepath.Join(filepath.Dir(m.path), "nginx", "emby-proxy.conf")
}

// writeNginxConf 把 nginx 配置写到数据目录（内容变化时才写）。宿主机上的定时任务发现变化后 nginx -t && reload。
func (m *Manager) writeNginxConf() {
	conf := []byte(m.nginxConf())
	p := m.nginxConfPath()
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, conf) {
		return
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		log.Printf("写入 nginx 配置失败: %v", err)
		return
	}
	if err := os.WriteFile(p+".tmp", conf, 0o644); err != nil {
		log.Printf("写入 nginx 配置失败: %v", err)
		return
	}
	if err := os.Rename(p+".tmp", p); err != nil {
		log.Printf("写入 nginx 配置失败: %v", err)
	}
}

// NginxStatus 是宿主机 reload 脚本写回的结果（data/nginx/reload.log）。
type NginxStatus struct {
	Installed bool   `json:"installed"` // 宿主机上装了自动 reload
	OK        bool   `json:"ok"`
	Time      int64  `json:"time"`
	Applied   bool   `json:"applied"` // 当前配置已经被 nginx 加载
	Output    string `json:"output,omitempty"`
}

func (m *Manager) nginxStatus() NginxStatus {
	var st NginxStatus
	data, err := os.ReadFile(filepath.Join(filepath.Dir(m.nginxConfPath()), "reload.log"))
	if err != nil {
		return st
	}
	st.Installed = true
	lines := strings.SplitN(string(data), "\n", 4)
	if len(lines) >= 3 {
		st.OK = strings.TrimSpace(lines[0]) == "ok"
		fmt.Sscan(lines[1], &st.Time)
		if conf, err := os.ReadFile(m.nginxConfPath()); err == nil {
			sum := sha256.Sum256(conf)
			st.Applied = st.OK && strings.TrimSpace(lines[2]) == fmt.Sprintf("%x", sum)
		}
	}
	if len(lines) == 4 {
		st.Output = strings.TrimSpace(lines[3])
	}
	return st
}
