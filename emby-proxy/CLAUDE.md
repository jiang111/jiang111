# emby-proxy

Emby 反代面板（Go，无第三方依赖，Docker 部署）。用户的服务器部署和配置按 **docs/配置手册.md** 执行。

## 帮用户配置服务器时

- 先按手册第 1 节收集信息（服务器 IP 和 SSH、角色、域名、Cloudflare 令牌和 Zone ID、证书方式、各上游地址和端口），缺的问用户，不要猜。
- Cloudflare（域名接入、令牌、DNS 记录）按手册第 3 节；DNS 记录可以用 3.5 的 `cf_*` 函数通过 API 添加。
- 命令都在服务器上执行；面板操作用手册第 4 节的 `api` 函数调用本机面板 API，不需要浏览器。
- **不要把上游地址、面板密码、同步令牌、Cloudflare 令牌、证书私钥写进本仓库**（仓库公开，上游地址各家要求保密），
  也不要贴到 Issue / PR / 提交信息里。
- 每一步做完用手册第 8 节验证；出问题看第 9 节。
- 多服务器时站点和设置只在主节点改。

## 开发

- `go test ./...`（加 `-race -count=3` 检查并发）。修改后同步更新 README.md 和 docs/配置手册.md。
- 主要文件：`proxy.go`（反代核心、地址改写、推流签名）、`rewrite.go`（响应体改写）、`follow.go`（推流跳转跟随）、
  `route.go`（额外转发规则）、`sticky.go`（同一用户固定同一台、节点间转发）、`cluster.go`（主从同步）、
  `nginx.go`（生成 nginx 配置）、`cert.go`、`backup.go`、`admin.go`（面板 API）、`web/index.html`（面板前端）、
  `install.sh`、`nginx-reload.sh`（宿主机自动应用 nginx 配置）。
