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

### 套 HTTPS：nginx + 一个服务一个端口（推荐）

nginx 在对外端口做 HTTPS，转发给本机的站点端口。对外端口和站点端口**不能相同**（Linux 上会端口冲突），
比如对外 `8001` → 站点 `18001`、对外 `8002` → 站点 `18002`。

1. 面板「设置 → Nginx」：
   - 独立端口监听地址选「仅本机 127.0.0.1」（站点端口只让 nginx 访问，外面不能绕过 HTTPS）
   - 「HTTPS 证书」选择电脑上的证书文件（fullchain.pem / .crt）和私钥文件（privkey.pem / .key）上传。
     会校验是否匹配、是否过期，保存在 `/opt/emby-proxy/data/ssl/`（权限 600），生成的 nginx 配置自动引用。
     证书到期后重新上传再 `nginx -s reload`；用 certbot 自动续期的话，展开「改用服务器上已有的证书路径」填路径即可。
2. 添加站点：接入方式「独立端口」填 `18001`；「高级选项 → 客户端访问地址」填 `https://你的域名:8001`。
3. 「设置 → 生成 nginx 配置」，保存为 `/etc/nginx/conf.d/emby-proxy.conf`，然后：
   ```bash
   nginx -t && nginx -s reload
   ```
   新增/修改站点后重新生成一次即可。服务器有 IPv6 的话把配置里 `listen [::]:端口` 那行的注释去掉。
4. 防火墙放行对外端口（8001、8002…），不要放行 18001 这些站点端口。
5. 客户端里填 `https://你的域名`，端口 `8001`（或直接 `https://你的域名:8001`）。

面板也可以只对本机开放：config.json 里 `admin_listen` 改成 `127.0.0.1:55660`，用 SSH 隧道
（`ssh -L 55660:127.0.0.1:55660 服务器`）或生成的配置里注释掉的面板 server 块访问。

### 套 HTTPS：路径前缀模式

nginx 一个 `server`（443）反代到入口端口 `http://127.0.0.1:55661`（`proxy_pass` 末尾不要加 `/`），
location 里的头和超时设置与上面生成的配置相同；然后面板「设置」里把入口公网地址填成 `https://你的域名`，
客户端地址为 `https://你的域名/路径`。

## 推流分离

很多服务器把 API 和视频流放在不同域名（主站 302 跳到推流域名）。每个站点可选：

- **经本机中转**（默认）：自动识别推流域名，把 302 的 `Location`、`PlaybackInfo`、m3u8 里的推流地址改写成
  `本机地址/__ep/https/推流域名/443/...`，视频流也经过本机。隐藏真实 IP、走你服务器的线路，但消耗服务器流量。
  只有上游跳转过的域名（或手动加入白名单的）才允许中转，不会变成开放代理。
  播放请求（带 `Range` 或 `/Videos/.../stream|original|download`）遇到 30x 跳转时由本机直接跟随，客户端只拿到一个 200/206，
  避免部分播放器跟随跳转时丢 Range 导致「播放几秒卡住 / 无法拖动 / Exo 提示不支持」。跳到与上游无关的第三方域名时不会带上 Emby 凭据。
- **客户端直连推流**：只代理 API，播放时让客户端直接连推流服务器，省服务器流量。

## 服务器自带推流路径（按 nginx 模板配置）

有的服务器提供 nginx 反代模板，要求带一个特殊请求头，并把某个路径转发到推流节点，例如：

```nginx
proxy_set_header MagpieCDN "$scheme://$server_name:$server_port";
location /magpiecdn { proxy_pass https://$arg_load.example.com; ... }
```

在站点「高级选项」里对应填：

- 自定义请求头：`MagpieCDN: {public_base}`
- 额外转发规则：`/magpiecdn => https://{arg.load}.example.com`

`{public_base}` 会替换成客户端实际访问的地址（https 由前置反代的 `X-Forwarded-Proto` 判断）；
`{arg.load}` 取请求参数，只允许字母数字和 `-`，不能借此转发到任意主机。

## 服务器有 IP 规则时

有的服务器按客户端 IP 做限制（例如禁止日本 IP / VPN），默认情况下上游看到的是**反代服务器的 IP**，
所以反代服务器本身的 IP 必须符合对方规则。对方的官方反代示例如果会传 `X-Forwarded-For`，可以在站点高级选项里打开「传真实 IP」。

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
