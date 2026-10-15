# Emby 反代面板

一台服务器反代多个 Emby 服务器，网页上增删改，适配常见 Emby 客户端
（SenPlayer / Infuse / Forward / VidHub / Hills / Yamby / Emby 官方等）。
Go 编写的单个程序，无第三方依赖，Docker 部署。

**主要功能**

- 一个服务一个端口（也支持路径前缀 / 独立域名），nginx 做 HTTPS，配置自动生成并自动应用
- 多台服务器用同一个域名负载均衡：主节点自动同步站点、设置和证书到各从节点
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

### 第 4 步：nginx 配置（自动）

安装脚本检测到 nginx 和 systemd 时，会自动开启「nginx 自动应用」：

- 面板里每次新增 / 修改站点、更换证书，程序都会重新生成 nginx 配置；
- 宿主机上的定时任务约 15 秒内执行 `nginx -t`，**通过才 reload**；不通过就保留上一份配置，nginx 照常运行，面板「设置 → Nginx」里会显示错误原因。

所以这一步什么都不用做。「设置 → Nginx → 自动应用」显示「nginx 已自动应用最新配置」即可。

- 服务器有 IPv6：在「设置 → Nginx」勾选「同时监听 IPv6」。
- 配置里出现「跳过」或「注意」开头的注释（「生成 nginx 配置」按钮可查看），按提示修改站点，比如没填客户端访问地址、证书不含该域名。
- 安装时还没装 nginx：装好后重新执行一次安装命令，就会开启自动应用。
- 不想让程序管 nginx：安装前执行 `export EMBY_PROXY_NO_NGINX=1`，再用「生成 nginx 配置」手动复制到 `/etc/nginx/conf.d/`。

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
systemctl disable --now emby-proxy-nginx.timer
rm -f /etc/systemd/system/emby-proxy-nginx.{service,timer} /etc/nginx/conf.d/emby-proxy.conf
systemctl daemon-reload && nginx -s reload
cd /opt/emby-proxy && docker compose down && cd / && rm -rf /opt/emby-proxy
```

**备份 / 迁移**：「设置 → 导入 / 导出配置」导出 JSON，新服务器装好后导入即可（「合并」按站点 ID 覆盖、其余追加；「替换全部」删除现有站点）。
导入前会整体校验，有问题不会改动任何配置。导出文件**不含面板密码**，但**包含各服务器的上游地址，请勿公开分享**；证书不在导出范围内，迁移后要重新上传。

---

## 教程：多台服务器 + 一个域名负载均衡（Cloudflare）

客户端都填 `https://emby.example.com:8001`，DNS 把用户分到不同的服务器上；一台做**主节点**管理配置，其余**从节点**自动同步。

```
                        ┌─▶ 服务器 1（主节点）─┐
emby.example.com ──DNS──┼─▶ 服务器 2（从节点）─┼──▶ 上游 Emby
                        └─▶ 服务器 3（从节点）─┘
master.example.com ─────▶ 服务器 1 的面板（从节点从这里同步）
```

同步内容：站点、设置、推流签名密钥、上传的证书、节点列表。各节点独立：面板密码、监听端口、统计和日志。

**同一用户固定同一台（默认开启）**：DNS 可能把同一个用户的 API 请求和推流请求分到不同服务器，上游就会看到
同一账号来自多个 IP（不少服会当作共享账号限制或封号）。开启后按**客户端 IP** 为每个用户算出唯一的负责节点，
各节点算出的结果相同；请求落到别的节点时，由它转给负责节点，**API 和推流都只经过负责节点访问上游**，
上游看到的 IP 数和用户直连时一样。

```
用户 X ──DNS 分到服务器 2──▶ 服务器 2 ──转发──▶ 服务器 1（X 的负责节点）──▶ 上游
用户 X ──DNS 分到服务器 1─────────────────────▶ 服务器 1（X 的负责节点）──▶ 上游
```

- 负责节点失联（超过 90 秒没同步）会被移出分配，只有它的用户换到其它节点；转发时连不上也会就地处理，不中断播放。
- 被转发的请求会多走一段服务器之间的流量（转发节点进出各一份）。多数播放器在一次播放中会复用连接和 DNS 结果，转发比例不高；
  访问日志的「节点」列能看到 `→ 名称`（转给了谁）和 `← 名称`（谁转来的）。
- 推流中转地址带签名，任何节点生成的地址在其它节点都能用。

> ⚠️ 每台服务器的 IP 都必须符合对方规则（比如禁止日本 IP、机房 IP）。
> 同一用户换了网络（如手机切到 4G）IP 变了，可能会分到另一台，这和直连时换网络一样。

### 第 1 步：每台服务器都安装

每台都按上面单机教程的「准备」装好 Docker 和 nginx，然后执行同一条安装命令。

### 第 2 步：申请通配符证书（主节点）

多台服务器共用一个域名，Let's Encrypt 的 HTTP 验证可能被别的服务器接到，所以用 **Cloudflare DNS 验证**申请
`example.com` + `*.example.com` 的通配符证书，并直接装到程序的数据目录，之后自动续期、自动同步到所有从节点：

1. Cloudflare 控制台 → 我的个人资料 → API 令牌 → 创建令牌 → 模板「编辑区域 DNS」，区域选你的域名，复制令牌。
2. 在主节点执行（把邮箱、域名、令牌换成你的）：
   ```bash
   curl https://get.acme.sh | sh -s email=you@example.com
   export CF_Token="你的 Cloudflare API 令牌"
   ~/.acme.sh/acme.sh --issue --dns dns_cf -d example.com -d '*.example.com' --server letsencrypt
   mkdir -p /opt/emby-proxy/data/ssl
   ~/.acme.sh/acme.sh --install-cert -d example.com --ecc \
     --fullchain-file /opt/emby-proxy/data/ssl/fullchain.pem \
     --key-file /opt/emby-proxy/data/ssl/privkey.pem
   ```
3. 一分钟内面板「设置 → Nginx → HTTPS 证书」会显示这张证书（不要再填「服务器上已有的证书路径」）。
   acme.sh 每 60 天自动续期并写回这两个文件，程序发现后自动 reload nginx，并同步给从节点。

不想用 acme.sh 的话，也可以在面板里手动上传通配符证书，同样会同步给从节点。

### 第 3 步：配置主节点

主节点面板（`http://主节点IP:55660`）→「设置」：

1. **Nginx**：独立端口监听地址选「仅本机 127.0.0.1」，保存。
2. **多服务器同步**：
   - 本机角色选「主节点」
   - 本机名称：如 `hk-1`（各台不能重名）
   - 本机公网 IP：主节点的公网 IP（主节点也放进 `emby` 的 DNS 记录时必填；只做管理不接流量可以留空）
   - 本机面板 HTTPS 地址：`https://master.example.com:8443`
   - 「同一用户固定同一台」保持勾选

   保存后页面会显示**同步令牌**，复制备用。
3. 编辑 `/opt/emby-proxy/data/config.json`，把 `"admin_listen": ":55660"` 改成 `"admin_listen": "127.0.0.1:55660"`，
   执行 `docker restart emby-proxy`。之后主节点面板改用 `https://master.example.com:8443` 访问。
4. 「站点」页按单机教程第 3 步添加站点，客户端访问地址填 `https://emby.example.com:8001`、`:8002`……

### 第 4 步：配置从节点

每台从节点面板（`http://从节点IP:55660`）→「设置 → 多服务器同步」：

- 本机角色：从节点
- 本机名称：如 `tokyo-1`（各台不能重名）
- 本机公网 IP：一般留空（自动用主节点看到的 IP）；服务器有多个 IP 或经过 NAT 时填它对外的 IP
- 主节点面板地址：`https://master.example.com:8443`
- 同步令牌：第 3 步复制的令牌

保存后几秒内站点、证书就同步过来，nginx 自动应用。主节点「多服务器同步」里能看到每个从节点、转发地址和「已是最新」状态，
下面「参与分配的节点」列出所有接流量的服务器。
之后从节点的站点和设置只能查看，修改都在主节点上做。从节点面板也建议照第 3 步第 3 点改成只监听本机。

### 第 5 步：Cloudflare DNS

Cloudflare → 你的域名 → DNS → 记录：

| 类型 | 名称 | 内容 | 代理状态 | TTL |
| --- | --- | --- | --- | --- |
| A | `master` | 主节点 IP | **仅 DNS（灰云）** | 自动 |
| A | `emby` | 服务器 1 IP | **仅 DNS（灰云）** | 1 分钟 |
| A | `emby` | 服务器 2 IP | **仅 DNS（灰云）** | 1 分钟 |
| A | `emby` | 服务器 3 IP | **仅 DNS（灰云）** | 1 分钟 |

- 同一个名称加多条 A 记录，Cloudflare 会轮流返回，用户就被分到不同服务器上。主节点也可以同时作为普通节点放进 `emby` 记录。
- **必须是灰云**：小黄云只代理 443、8443、2053 等少数端口，8001 这类端口过不去，而且 Cloudflare 条款限制普通代理传大量视频。
- 免费版没有健康检查：某台服务器挂了，要手动删掉它的 A 记录（TTL 1 分钟，几分钟内生效）。
  需要自动摘除故障节点，可以用 Cloudflare 付费的 **Load Balancing**：健康检查选 HTTPS、端口 `8001`、
  路径 `/emby/System/Info/Public`、期望 200，池里加入各节点 IP，代理状态保持「仅 DNS」。

### 第 6 步：防火墙和验证

每台服务器放行 nginx 对外端口（8001、8002…），服务器之间转发也走这些端口；主节点另外放行 8443（面板有密码、同步接口有令牌；更稳妥的做法是只对各从节点 IP 和你自己常用的 IP 放行）。

逐台验证（把 `1.2.3.4` 换成每台的 IP）：

```bash
curl --resolve emby.example.com:8001:1.2.3.4 https://emby.example.com:8001/emby/System/Info/Public
```

每台都返回服务器信息 JSON 就说明正常，然后在客户端里实际播放、拖动测试。
播放时看各台面板的访问日志：同一个客户端 IP 的请求，要么由负责节点直接处理，要么在别的节点上显示 `→ 负责节点`。

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
| 面板显示「nginx 检查未通过」 | 看下面的错误输出：`Address family not supported` 是没有 IPv6 却勾选了「监听 IPv6」；`cannot load certificate` 是证书路径不对。nginx 会继续用上一份配置 |
| 从节点「同步失败」 | 主节点面板地址能否从从节点访问（`curl https://master.example.com:8443`）、8443 是否放行、令牌是否已重新生成 |
| 日志里出现「→ 某节点 失联，本机处理」 | 该节点的对外端口从本机连不上：检查它的防火墙、nginx 是否正常、「本机公网 IP」是否填对 |
| 从节点站点显示「端口监听失败」 | 从节点上该端口被其它程序占用，或与本机面板 / 入口端口冲突 |
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
