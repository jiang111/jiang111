# Emby 反代面板

一台服务器反代多个 Emby 服务器，网页上增删改，适配常见 Emby 客户端
（SenPlayer / Infuse / Forward / VidHub / Hills / Yamby / Emby 官方等）。
Go 编写的单个程序，无第三方依赖，Docker 部署。

**主要功能**

- 一个服务一个端口（也支持路径前缀 / 独立域名），nginx 做 HTTPS，配置由面板一键生成
- WebSocket、Range 拖动、推流分离自动识别与中转、302 / PlaybackInfo / m3u8 地址改写
- 网页上传证书，支持忽略证书过期；配置导入 / 导出
- 实时连接数、播放数、流量图、访问日志

---

## 教程：Docker + nginx，一个服务一个端口

最终效果：客户端填 `https://emby.example.com:8001` 连到服务器 A，`:8002` 连到服务器 B……
nginx 在 8001、8002 上做 HTTPS，转发给只在本机监听的 18001、18002（反代程序），再由程序转发到各个 Emby 服务器。

```
客户端 ──HTTPS──▶ nginx :8001 ──▶ 127.0.0.1:18001 (emby-proxy) ──▶ 上游 Emby A
客户端 ──HTTPS──▶ nginx :8002 ──▶ 127.0.0.1:18002 (emby-proxy) ──▶ 上游 Emby B
```

### 准备

- 一台 Linux 服务器（Debian / Ubuntu / CentOS 均可），**服务器 IP 要符合各家 Emby 的规则**（有的禁止日本 IP、机房 IP 等）
- 一个解析到服务器的域名，以及它的证书（`fullchain.pem` + `privkey.pem`，或 `.crt` + `.key`）
- 已安装 Docker 和 nginx：
  ```bash
  curl -fsSL https://get.docker.com | sh     # Docker
  apt install -y nginx                        # Debian/Ubuntu；CentOS 用 yum install -y nginx
  ```

### 第 1 步：安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/install.sh)
```

首次会在本机构建镜像（一两分钟），结束后打印**面板地址和密码**。数据都在 `/opt/emby-proxy/data`。
密码忘了可以看 `/opt/emby-proxy/data/config.json` 里的 `admin_password`。

防火墙 / 云服务器安全组先临时放行 `55660`，浏览器打开 `http://服务器IP:55660` 登录。

### 第 2 步：面板基础设置

「设置」页 → **Nginx（独立端口 + HTTPS）**：

1. **独立端口监听地址** 选「仅本机 127.0.0.1」，保存。
   这样 18001 这些端口只有本机 nginx 能访问，外面无法绕过 HTTPS。
2. **HTTPS 证书**：分别选择电脑上的证书文件和私钥文件，点「上传证书」。
   - 会校验证书和私钥是否匹配；上传成功后显示域名和到期时间。
   - 证书已过期但仍想用：勾选「允许上传已过期的证书」。
   - 服务器上已有 certbot 自动续期的证书：展开「改用服务器上已有的证书路径」，填 `/etc/letsencrypt/live/域名/fullchain.pem` 等路径，保存。

### 第 3 步：添加站点

「站点」页 → **添加站点**：

| 字段 | 填什么 |
| --- | --- |
| 名称 | 随意，如「A 服」 |
| 上游 Emby 地址 | 你在客户端里原本填的地址，如 `https://emby-a.example.net`（不要带 `/emby`），点「测试」应显示服务器名和版本 |
| 接入方式 | 独立端口，端口填 `18001` |
| 推流 | 默认「经本机中转」即可（见下文「推流分离」） |
| 高级选项 → 客户端访问地址 | `https://emby.example.com:8001`（你的域名 + nginx 对外端口） |

> nginx 对外端口和站点端口**不能相同**（Linux 上会端口冲突）。建议对外 `8001` 对应站点 `18001`，`8002` 对应 `18002`，依此类推。

保存后卡片上的地址就是客户端要填的地址。其它服务器重复添加即可。

### 第 4 步：生成并启用 nginx 配置

「设置」页 → **生成 nginx 配置** → 复制，保存到服务器：

```bash
nano /etc/nginx/conf.d/emby-proxy.conf   # 粘贴后保存
nginx -t && nginx -s reload
```

- 每次新增 / 修改站点后，重新生成一次并 reload。
- 服务器有 IPv6 的话，把配置里 `# listen [::]:端口 ssl;` 的注释去掉。
- 配置里如果出现「跳过」或「注意」开头的注释，按提示修改站点（比如没填客户端访问地址、证书不含该域名）。

### 第 5 步：防火墙

只放行 nginx 的对外端口，**不要**放行 18001 这些站点端口：

```bash
# ufw（Debian/Ubuntu）
ufw allow 8001/tcp && ufw allow 8002/tcp
# firewalld（CentOS）
firewall-cmd --permanent --add-port=8001-8002/tcp && firewall-cmd --reload
```

云服务器还要在控制台的安全组里放行同样的端口。

### 第 6 步：客户端

在 SenPlayer / Infuse 等客户端里添加服务器：

- 地址：`https://emby.example.com`，端口：`8001`（或直接填 `https://emby.example.com:8001`）
- 用户名、密码：和原来一样

登录后测试 **开始播放、连续播放、拖动进度条**。面板「访问日志」里播放请求返回 **206** 就说明正常。

### 第 7 步（建议）：收起面板端口

面板配置完成后，不必再对外开放 55660：

1. 编辑 `/opt/emby-proxy/data/config.json`，把 `"admin_listen": ":55660"` 改成 `"admin_listen": "127.0.0.1:55660"`
2. `docker restart emby-proxy`，防火墙关掉 55660
3. 以后用 SSH 隧道访问面板：
   ```bash
   ssh -L 55660:127.0.0.1:55660 root@服务器IP
   # 然后本地浏览器打开 http://127.0.0.1:55660
   ```
   或者把生成的 nginx 配置末尾注释掉的「面板」server 块打开，用 HTTPS 访问。

### 更新、备份、卸载

```bash
# 更新：重新执行安装命令，配置保留
bash <(curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/install.sh)

# 查看日志
docker logs -f emby-proxy

# 卸载
cd /opt/emby-proxy && docker compose down && rm -rf /opt/emby-proxy
rm /etc/nginx/conf.d/emby-proxy.conf && nginx -s reload
```

**备份 / 迁移**：「设置 → 导入 / 导出配置」导出 JSON，新服务器装好后导入即可（「合并」按站点 ID 覆盖、其余追加；「替换全部」删除现有站点）。
导入前会整体校验，有问题不会改动任何配置。导出文件**不含面板密码**，但**包含各服务器的上游地址，请勿公开分享**；证书不在导出范围内，迁移后要重新上传。

---

## 常见问题

| 现象 | 排查 |
| --- | --- |
| 面板「测试」失败：证书校验失败 / 已过期 | 站点「高级选项」勾选「上游证书过期仍然连接」（仍校验域名和签发机构）；自签证书才用「完全不校验」 |
| 测试提示「被重定向到 …」 | 上游拦截了本机 IP 或 User-Agent；确认服务器 IP 符合对方规则，「覆盖 User-Agent」留空 |
| 能登录、能看海报，但不能播放 | 看访问日志里播放请求的状态码：403 多为 IP 规则；502 为连不上推流；该服务器如果有自带的推流路径，见下文「服务器自带推流路径」 |
| 播放几秒卡住 / 不能拖动 | nginx 配置是否为面板生成（必须有 `proxy_buffering off`）；日志里播放请求应为 206 |
| 登录返回 429 | 对方按 IP 限制登录次数，经反代所有人共用服务器 IP，别反复输错密码，稍后再试 |
| nginx -t 报 `Address already in use` | 对外端口和站点端口用了同一个数字，改成不同的 |
| nginx -t 报 `Address family not supported` | 服务器没有 IPv6，保持 `listen [::]` 那行注释 |
| 浏览器打开反代地址被拦 / 跳错误页 | 部分服务器禁止网页端，只允许 App，属正常 |

---

## 参考

### 三种接入方式

| 方式 | 客户端地址示例 | 说明 |
| --- | --- | --- |
| 独立端口（推荐） | `https://emby.example.com:8001` | 兼容性最好；配合 nginx 做 HTTPS，见上面教程 |
| 路径前缀 | `https://emby.example.com/hk` | 一个 443 端口反代多个服务器 |
| 独立域名 | `https://hk.example.com` | 每个服务器一个子域名 |

路径前缀和独立域名共用入口端口 `55661`：nginx 一个 server 反代到 `http://127.0.0.1:55661`
（`proxy_pass` 末尾不要加 `/`，location 里的请求头和超时设置照抄生成的配置），
面板「设置」里把入口公网地址填成 `https://你的域名`。

### 推流分离

很多服务器把 API 和视频流放在不同域名（播放时 302 跳到推流域名）。每个站点可选：

- **经本机中转**（默认）：自动识别推流域名，把 302、PlaybackInfo、m3u8 里的推流地址改写成
  `本机地址/__ep/https/推流域名/443/...`，视频流也经过本机。隐藏真实 IP、走你服务器的线路，但消耗服务器流量。
  只有上游跳转过的域名（或手动加入白名单的）才允许中转，不会变成开放代理。
  播放请求遇到 30x 跳转时由本机直接跟随，客户端只拿到 200/206，避免部分播放器跟随跳转时丢 Range
  导致「播放几秒卡住 / 无法拖动 / Exo 提示不支持」。跳到无关的第三方域名时不会带上 Emby 凭据。
- **客户端直连推流**：只代理 API，视频流让客户端直接连推流服务器，省服务器流量。

### 服务器自带推流路径

有的服务器提供 nginx 反代模板，要求带特殊请求头，并把某个路径转发到推流节点，例如：

```nginx
proxy_set_header MagpieCDN "$scheme://$server_name:$server_port";
location /magpiecdn { proxy_pass https://$arg_load.example.net; ... }
```

在站点「高级选项」里对应填：

- 自定义请求头：`MagpieCDN: {public_base}`
- 额外转发规则：`/magpiecdn => https://{arg.load}.example.net`

`{public_base}` 会替换成客户端实际访问的地址；`{arg.load}` 取请求参数，只允许字母数字和 `-`，不能借此转发到任意主机。
这类服务器建议用独立端口方式，`{public_base}` 就是 `https://域名:端口`，与对方模板完全一致。

### 服务器有 IP 规则时

默认上游看到的是**反代服务器的 IP**（会去掉 `X-Forwarded-For` 等头），所以服务器 IP 本身必须符合对方规则。
对方的官方反代示例如果会传 `X-Forwarded-For`，可以在站点高级选项里打开「传真实 IP」。

### 证书过期

- **上游证书过期**：站点勾选「上游证书过期仍然连接」，只忽略有效期，仍校验域名和签发机构。
- **自己的证书过期**：上传时勾选「允许上传已过期的证书」。nginx 能用，但客户端可能提示证书过期，尽快续期。

### 做了哪些处理

参考了 Nginx 反代、Cloudflare Worker 反代脚本以及 [emby-reverse-proxy-go](https://github.com/Gsy-allen/emby-reverse-proxy-go) 等常见做法：

- WebSocket（`/embywebsocket`）、`Range` 断点续传 / 拖动、无缓冲边收边发，长时间暂停不断开。
- `Location` / `Content-Location` 改写；相对跳转自动加上路径前缀。
- JSON / m3u8 / 文本响应里的上游地址改写成反代地址（gzip 解压改写后再压缩，其它类型保持压缩原样透传）。
- `/System/Info(/Public)` 的内外网地址改成反代地址，避免客户端绕过反代直连。
- `Host`、`Origin`、`Referer` 改成上游；默认去掉 `X-Forwarded-For`、`X-Real-IP`、`CF-Connecting-IP` 等。
- 面板登录 Cookie 不会转发给上游；面板写接口有 CSRF 防护和登录限流。

### 配置文件

`/opt/emby-proxy/data/config.json`，一般在面板里改即可。

| 字段 | 说明 |
| --- | --- |
| `listen` | 入口端口（路径 / 域名模式），默认 `:55661` |
| `admin_listen` | 面板端口，默认 `:55660` |
| `admin_password` | 面板密码 |
| `port_bind` | 独立端口站点的监听地址，`127.0.0.1` 表示仅本机 |
| `nginx_cert` / `nginx_key` | 生成 nginx 配置用的证书路径 |
| `sites` | 站点列表 |

改 `listen` / `admin_listen` 后需要 `docker restart emby-proxy`。

不用 Docker 也可以直接运行：`go build -o emby-proxy . && ./emby-proxy -config data/config.json`。

### 开发

```bash
go test ./...
```
