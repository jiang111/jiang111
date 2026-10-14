# Emby 反代面板

一台服务器反代多个 Emby 服务器，网页上增删改，适配所有常见 Emby 客户端
（Infuse / SenPlayer / Forward / VidHub / Hills / Yamby / Emby 官方 / 网页版等）。
用 Go 写的单个程序，没有第三方依赖。

## 安装（1Panel）

1. 1Panel → 终端（或 SSH）执行：
   ```bash
   bash <(curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/install.sh)
   ```
   第一次安装会在本机构建镜像，结束后打印面板地址和密码。再执行一次就是更新（配置保留在 `/opt/emby-proxy/data`）。
2. 1Panel → 主机 → 防火墙 放行 `55660`（面板）、`55661`（入口），以及给站点分配的独立端口。
3. 浏览器打开 `http://服务器IP:55660`，添加站点，把卡片上的地址填进客户端，用户名密码和原来一样。

不用 Docker 也可以直接编译运行：`go build -o emby-proxy . && ./emby-proxy -config data/config.json`。

## 三种接入方式

| 方式 | 客户端地址示例 | 适合 |
| --- | --- | --- |
| 独立端口 | `http://IP:18001` | 兼容性最好，电视盒子等老客户端 |
| 路径前缀 | `https://emby.mydomain.com/hk` | 一个域名 + 一张证书反代多个服务器 |
| 独立域名 | `https://hk.mydomain.com` | 每个服务器一个子域名 |

路径前缀和独立域名共用入口端口 `55661`。

### 套 HTTPS

1Panel → 网站 → 创建网站 → 反向代理，代理地址填 `http://127.0.0.1:55661`（或某个站点的独立端口），申请证书即可。
1Panel 默认会传 `Host`、`X-Forwarded-Proto` 和 WebSocket 头，程序据此自动生成 `https://` 的地址；建议在反代配置里加：

```nginx
proxy_buffering off;
proxy_request_buffering off;
proxy_read_timeout 86400s;
```

然后在面板「设置」里把「入口公网地址」填成 `https://你的域名`，卡片上显示的客户端地址就对了。

## 推流分离

很多服务器把 API 和视频流放在不同域名（主站 302 跳到推流域名）。每个站点可选：

- **经本机中转**（默认）：自动识别推流域名，把 302 的 `Location`、`PlaybackInfo`、m3u8 里的推流地址改写成
  `本机地址/__ep/https/推流域名/443/...`，视频流也经过本机。隐藏真实 IP、走你服务器的线路，但消耗服务器流量。
  只有上游跳转过的域名（或手动加入白名单的）才允许中转，不会变成开放代理。
- **客户端直连推流**：只代理 API，播放时让客户端直接连推流服务器，省服务器流量。

## 做了哪些处理

参考了 Nginx 反代、Cloudflare Worker 反代脚本以及 [emby-reverse-proxy-go](https://github.com/Gsy-allen/emby-reverse-proxy-go) 等常见做法：

- WebSocket（`/embywebsocket`）、`Range` 断点续传/拖动进度条、无缓冲边收边发，长时间暂停不断开。
- `Location` / `Content-Location` 改写；相对跳转自动加上路径前缀。
- JSON / m3u8 / 文本响应里的上游地址改写成反代地址（gzip 也能处理，改写后再压缩给客户端）。
- `/System/Info`、`/System/Info/Public` 的 `LocalAddress` / `WanAddress` 改成反代地址，避免官方客户端绕过反代直连。
- `Host`、`Origin`、`Referer` 改成上游；默认去掉 `X-Forwarded-For`、`X-Real-IP`、`CF-Connecting-IP` 等，上游只看到本机 IP（可按站点打开「传真实 IP」）。
- 可按站点覆盖 User-Agent、添加自定义请求头、跳过证书校验。
- 面板：站点启停、上游连通测试（返回服务器名/版本/延迟）、实时连接数/播放数/流量、60 分钟流量图、访问日志（`api_key` 等已打码）。

## 配置文件

`/opt/emby-proxy/data/config.json`，一般在面板里改即可。

| 字段 | 说明 |
| --- | --- |
| `listen` | 入口端口，默认 `:55661` |
| `admin_listen` | 面板端口，默认 `:55660` |
| `admin_password` | 面板密码，忘了就看这里 |
| `entry_url` / `public_host` | 仅用于面板显示客户端地址 |
| `sites` | 站点列表 |

改 `listen` / `admin_listen` 后需要 `docker restart emby-proxy`。

## 开发

```bash
go test ./...
```
