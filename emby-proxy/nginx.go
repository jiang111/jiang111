package main

import (
	"crypto/x509"
	"fmt"
	"net"
	"net/url"
	"strings"
)

// nginxConfig 为「独立端口」站点生成 nginx 配置：nginx 在公网端口终结 HTTPS，转发到本机的站点端口。
// 站点需要填写「客户端访问地址」（如 https://emby.example.com:8001），据此决定 server_name 和对外端口。
// leaf 为当前证书（可为 nil），用于提示证书是否覆盖站点域名。
func nginxConfig(cfg *Config, leaf *x509.Certificate) string {
	cert, key := cfg.NginxCert, cfg.NginxKey
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
	b.WriteString(`# 由 Emby 反代面板生成，放到 /etc/nginx/conf.d/emby-proxy.conf，然后 nginx -t && nginx -s reload
# 每个站点一个端口：nginx 在对外端口做 HTTPS，转发给本机的站点端口

map $http_upgrade $emby_proxy_connection {
    default upgrade;
    ''      close;
}

ssl_session_cache   shared:EmbyProxySSL:10m;
ssl_session_timeout 1d;
`)
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
		fmt.Fprintf(&b, `server {
    listen %[1]s ssl;
    # listen [::]:%[1]s ssl;   # 服务器有 IPv6 时取消注释
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
`, port, host, cert, key, backend, s.Port)
	}

	adminPort := listenPort(cfg.AdminListen)
	fmt.Fprintf(&b, `
# 面板（可选）：把下面的 8443 换成你想要的端口后取消注释，并把 config.json 的 admin_listen 改为 127.0.0.1:%[1]d
# server {
#     listen 8443 ssl;
#     server_name 你的域名;
#     ssl_certificate     %[2]s;
#     ssl_certificate_key %[3]s;
#     location / {
#         proxy_pass http://127.0.0.1:%[1]d;
#         proxy_set_header Host $host;
#         proxy_set_header X-Forwarded-Proto https;
#         proxy_set_header X-Real-IP $remote_addr;
#     }
# }
`, adminPort, cert, key)
	return b.String()
}

func oneLine(s string) string {
	return strings.NewReplacer("\n", " ", "\r", " ").Replace(s)
}

func validBind(s string) bool {
	return s == "" || net.ParseIP(s) != nil
}
