# Emby 反代面板

一台或多台服务器反代多个 Emby 服务器，网页管理，适配常见 Emby 客户端
（SenPlayer / Infuse / Forward / VidHub / Hills / Yamby 等）。
Go 编写的单个程序，无第三方依赖，Docker 部署。

- 一个服务一个端口，nginx 做 HTTPS，nginx 配置自动生成、自动检查、自动生效
- 多台服务器用同一个域名负载均衡：主节点自动同步站点、设置、证书；同一用户的 API 和推流固定走同一台
- WebSocket、Range 拖动、推流分离自动识别与中转、302 / PlaybackInfo / m3u8 地址改写
- 网页上传证书（可忽略过期），配置导入 / 导出，流量和访问日志

> 需要按步骤用命令（或让 AI 助手）完成配置，看 **[配置手册](docs/配置手册.md)**：信息清单、端口规划、
> 每一步的命令、面板 API、验证和排查都在里面。也可以用**一键脚本** [`deploy/master.sh`](deploy/master.sh) /
> [`deploy/node.sh`](deploy/node.sh)，自动完成从安装到验证的全部步骤。

## 目录

1. [工作原理](#1-工作原理)
2. [部署前准备](#2-部署前准备)
3. [教程一：单台服务器](#3-教程一单台服务器)
4. [教程二：多台服务器负载均衡（Cloudflare）](#4-教程二多台服务器负载均衡cloudflare)
5. [日常维护](#5-日常维护)
6. [面板说明](#6-面板说明)
7. [特殊服务器的适配](#7-特殊服务器的适配)
8. [常见问题](#8-常见问题)
9. [参考](#9-参考)

---

## 1. 工作原理

**单台服务器**：每个上游 Emby 占一个端口。nginx 在对外端口做 HTTPS，转给只在本机监听的反代程序，再由程序访问上游。

```
客户端 ──HTTPS──▶ nginx :8001 ──▶ 127.0.0.1:18001 (emby-proxy) ──▶ 上游 Emby A
客户端 ──HTTPS──▶ nginx :8002 ──▶ 127.0.0.1:18002 (emby-proxy) ──▶ 上游 Emby B
```

**多台服务器**：同一个域名解析到多台服务器。一台做主节点（管理配置），其余是从节点（自动同步）。
每个用户按客户端 IP 固定分给一台「负责节点」，落到别的服务器上的请求会被转给它，
所以上游看到的 IP 数和用户直连时一样，不会因为多台服务器被当成共享账号。

```
                        ┌─▶ 服务器 1（主节点）─┐
emby.example.com ──DNS──┼─▶ 服务器 2（从节点）─┼──▶ 上游 Emby
                        └─▶ 服务器 3（从节点）─┘
master.example.com ─────▶ 服务器 1 的面板（从节点从这里同步配置）
```

## 2. 部署前准备

**每台服务器**：

- Linux（Debian / Ubuntu / CentOS），能用 root 登录
- **IP 符合各家 Emby 的规则**（有的禁止日本 IP、机房 IP、VPN）
- 装好 Docker 和 nginx：
  ```bash
  curl -fsSL https://get.docker.com | sh
  apt install -y nginx          # CentOS: yum install -y nginx && systemctl enable --now nginx
  ```

**域名（Cloudflare）**：域名的 DNS 由 Cloudflare 管理（免费版即可）。还没接入的话：

1. [dash.cloudflare.com](https://dash.cloudflare.com) →「Domains / 域」→「Onboard a domain / 添加域」，输入根域名 `example.com`，计划选 Free。
2. 到**买域名的注册商**后台：有 DNSSEC 的先关闭，再把名称服务器改成 Cloudflare 给的两个 `*.ns.cloudflare.com`。
3. 等 Cloudflare 显示「有效 / Active」（几分钟到几小时）。

**Cloudflare API 令牌**（用来自动申请证书；多服务器时也用来管理 DNS 记录）：

1. 右上角头像 →「My Profile / 我的个人资料」→「API Tokens」→「Create Token」→ 模板「**Edit zone DNS / 编辑区域 DNS**」。
2. Zone Resources 选 `Include` → `Specific zone` → 你的域名 →「Continue to summary」→「Create Token」，复制令牌（只显示一次）。
3. 回到域名的「Overview / 概述」页，右侧「API」区域复制 **Zone ID**。

**DNS 记录**：在域名 →「DNS」→「Records」添加 A 记录，**代理状态必须点成灰色云朵「仅 DNS」**
（小黄云不转发 8001 这类端口，也不允许用普通代理传视频）。单台服务器只需要一条：

| 类型 | 名称 | 内容 | 代理状态 | TTL |
| --- | --- | --- | --- | --- |
| A | `emby` | 服务器 IP | 仅 DNS（灰云） | 自动 |

多台服务器的记录见教程二第 5 步。更详细的说明（令牌验证、用命令管理记录、Load Balancing）见
[配置手册第 3 节](docs/配置手册.md#3-cloudflare-域名配置)。

**证书**：推荐用 acme.sh + 上面的令牌自动申请通配符证书并自动续期（教程二第 2 步，单台也适用）；也可以在面板上传现成的证书。

**端口规划**（约定，后面都按这个来）：

| 用途 | 端口 | 谁能访问 |
| --- | --- | --- |
| 站点对外端口（nginx，HTTPS） | `8001`、`8002`、`8003`… | 所有人（客户端填这个） |
| 站点程序端口 | `18001`、`18002`、`18003`…（对外端口 + 10000） | 仅本机 |
| 面板（程序） | `55660` | 配置完成后改为仅本机 |
| 面板 HTTPS（多服务器时主节点） | `8443` | 从节点和你自己 |
| 入口端口（路径 / 域名模式才用） | `55661` | 仅本机 |

> 对外端口和程序端口**不能相同**，Linux 上会端口冲突。

---

## 3. 教程一：单台服务器

### 第 1 步：安装

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/install.sh)
```

首次会在本机构建镜像（一两分钟），结束时打印**面板地址和密码**，并显示「nginx 自动应用：已开启」。
数据都在 `/opt/emby-proxy/data`，密码忘了看 `/opt/emby-proxy/data/config.json` 里的 `admin_password`。

防火墙 / 安全组临时放行 `55660`，浏览器打开 `http://服务器IP:55660` 登录。

### 第 2 步：基础设置

「设置」页 → **Nginx（独立端口 + HTTPS）**：

1. **独立端口监听地址** 选「仅本机 127.0.0.1」→ 保存。
2. **HTTPS 证书**：二选一
   - 自动申请（推荐）：在服务器上执行教程二第 2 步的 acme.sh 命令，一分钟内这里会显示证书；
   - 上传：选择证书文件（`fullchain.pem` / `.crt`）和私钥文件（`privkey.pem` / `.key`）→「上传证书」，成功会显示域名和到期时间。
   - 证书已过期但仍想用：勾选「允许上传已过期的证书」。
   - 服务器上用 certbot / acme.sh 自动续期：展开「改用服务器上已有的证书路径」填路径；
     或者像教程二第 2 步那样把证书装进 `/opt/emby-proxy/data/ssl/`（程序会自动识别并跟随续期）。
3. 服务器有 IPv6：勾选「同时监听 IPv6」→ 保存。

### 第 3 步：添加站点

「站点」页 →「添加站点」：

| 字段 | 填什么 |
| --- | --- |
| 名称 | 随意，如「A 服」 |
| 上游 Emby 地址 | 客户端里原本填的地址，如 `https://emby-a.example.net`（不要带 `/emby`）。点「测试」应显示服务器名和版本 |
| 接入方式 | 独立端口，填程序端口 `18001` |
| 推流 | 默认「经本机中转」 |
| 高级选项 → 客户端访问地址 | `https://emby.example.com:8001`（域名 + 对外端口） |
| 启用 | 勾选 |

保存后卡片上显示的地址就是客户端要填的地址。约 15 秒后「设置 → Nginx → 自动应用」显示
「nginx 已自动应用最新配置」，8001 端口就生效了。其它上游按 8002 / 18002、8003 / 18003… 继续添加。

### 第 4 步：防火墙

只放行对外端口，**不要**放行 18001 这些程序端口：

```bash
ufw allow 8001:8010/tcp                                                        # Debian/Ubuntu
firewall-cmd --permanent --add-port=8001-8010/tcp && firewall-cmd --reload     # CentOS
```

云服务器还要在控制台安全组放行同样的端口。

### 第 5 步：客户端

地址填 `https://emby.example.com`，端口 `8001`（或直接 `https://emby.example.com:8001`），用户名密码和原来一样。
测试**开始播放、连续播放、拖动进度条**；面板「访问日志」里播放请求返回 **206** 就正常。

### 第 6 步：收起面板端口（建议）

```bash
sed -i 's/"admin_listen": ":55660"/"admin_listen": "127.0.0.1:55660"/' /opt/emby-proxy/data/config.json
docker restart emby-proxy
```

然后防火墙关掉 55660。以后访问面板二选一：

- SSH 隧道：`ssh -L 55660:127.0.0.1:55660 root@服务器IP`，本地浏览器打开 `http://127.0.0.1:55660`
- HTTPS：「设置 → 多服务器同步 → 本机面板 HTTPS 地址」填 `https://emby.example.com:8443`（角色保持「单机」即可），
  nginx 会自动生成面板的配置，防火墙放行 8443

---

## 4. 教程二：多台服务器负载均衡（Cloudflare）

客户端都填 `https://emby.example.com:8001`；DNS 把用户分到不同服务器；主节点 `master.example.com` 管理配置。

**会同步的**：站点、设置、证书、推流签名密钥、节点列表。**各台独立的**：面板密码、监听端口、统计和日志。
从节点每 10 秒把本机统计上报给主节点，主节点面板显示全部节点之和（见[统计口径](#统计口径)）。

**同一用户固定同一台**（默认开启）：按客户端 IP 给每个用户算出唯一的负责节点（各台算出的结果一致），
API 和推流都只经负责节点访问上游；用户被 DNS 分到别的服务器时，由那台转给负责节点。

- 负责节点失联超过 90 秒会被移出分配，只有它的用户换到别的服务器；转发连不上时就地处理，不中断播放。
- 被转发的请求会多一段服务器之间的流量，面板单独显示为「节点间转发」。
  访问日志「节点」列：`→ 名称` 表示转给了谁，`← 名称` 表示谁转来的，`⇢ 名称 直连` 表示视频请求 302 给了谁。

**视频直连**（默认开启，需要每台填「本机直连域名」）：用户落到非负责节点时，API 照常转发，视频请求（直链、HLS、推流域名转发）
302 到负责节点的直连域名（如 `tokyo-1.example.com`，只解析到那台），视频只经过负责节点一台，不再两台各走一遍。
上游看到的仍只有负责节点的 IP。直连域名要被证书覆盖（通配符证书即可），各台 nginx 会自动把它加到站点的 `server_name`。
个别播放器跟随 302 后拖动有问题时，在主节点关掉「视频直连」，恢复为转发。
- 用户换网络（手机切 4G）IP 变了可能换一台，这和直连时换网络一样。

### 第 1 步：每台服务器都安装

按[部署前准备](#2-部署前准备)装好 Docker 和 nginx，然后每台执行同一条安装命令（教程一第 1 步）。

### 第 2 步：通配符证书（主节点）

多台服务器共用一个域名时，HTTP 验证可能被别的服务器接到，所以用 Cloudflare DNS 验证申请
`example.com` + `*.example.com`，直接装到程序数据目录。之后 acme.sh 自动续期，程序自动 reload nginx 并同步给从节点。

1. 准备好 Cloudflare API 令牌和 Zone ID（见[部署前准备](#2-部署前准备)）。
2. 在主节点执行：
   ```bash
   curl https://get.acme.sh | sh -s email=you@example.com
   export CF_Token="你的 Cloudflare API 令牌"
   export CF_Zone_ID="你的 Zone ID"
   ~/.acme.sh/acme.sh --issue --dns dns_cf -d example.com -d '*.example.com' --server letsencrypt
   mkdir -p /opt/emby-proxy/data/ssl
   ~/.acme.sh/acme.sh --install-cert -d example.com --ecc \
     --fullchain-file /opt/emby-proxy/data/ssl/fullchain.pem \
     --key-file /opt/emby-proxy/data/ssl/privkey.pem
   ```
3. 一分钟内主节点面板「设置 → Nginx → HTTPS 证书」显示这张证书。**不要**再填「服务器上已有的证书路径」。

也可以直接在主节点面板上传通配符证书，同样会同步给从节点。

### 第 3 步：配置主节点

主节点面板 →「设置」：

1. **Nginx**：独立端口监听地址选「仅本机 127.0.0.1」→ 保存。有 IPv6 就勾选「同时监听 IPv6」。
2. **多服务器同步**：
   - 本机角色：**主节点**
   - 本机名称：如 `hk-1`（每台不能重名）
   - 本机公网 IP：主节点公网 IP（主节点也接用户流量时必填；只做管理可以留空）
   - 本机面板 HTTPS 地址：`https://master.example.com:8443`
   - 本机直连域名：如 `hk-1.example.com`（DNS 只解析到这台）
   - 「同一用户固定同一台」「视频直连」保持勾选
   - 保存，记下页面上的**同步令牌**
3. 收起面板端口（教程一第 6 步的两条命令），之后用 `https://master.example.com:8443` 访问主节点面板。
4. 「站点」页按教程一第 3 步添加站点，客户端访问地址填 `https://emby.example.com:8001`、`:8002`…

### 第 4 步：配置从节点

每台从节点面板 →「设置 → 多服务器同步」：

- 本机角色：**从节点**
- 本机名称：如 `tokyo-1`（每台不能重名）
- 本机公网 IP：一般留空（自动用主节点看到的 IP）；多 IP 或 NAT 的服务器填它对外的 IP
- 本机直连域名：如 `tokyo-1.example.com`（DNS 只解析到这台）
- 主节点面板地址：`https://master.example.com:8443`
- 同步令牌：第 3 步记下的令牌

保存后几秒内站点和证书同步过来，nginx 自动生效。主节点「多服务器同步」里能看到各从节点「已是最新」，
以及「参与分配的节点」。从节点之后只读，修改都在主节点上做。从节点也照教程一第 6 步收起面板端口。

### 第 5 步：Cloudflare DNS

Cloudflare → 域名 →「DNS」→「Records」→「Add record」，按下表添加。**每条都要把橙色云朵点成灰色**（仅 DNS）：

| 类型 | 名称 | 内容 | 代理状态 | TTL |
| --- | --- | --- | --- | --- |
| A | `master` | 主节点 IP | 仅 DNS（灰云） | 自动 |
| A | `emby` | 服务器 1 IP | 仅 DNS（灰云） | 1 分钟 |
| A | `emby` | 服务器 2 IP | 仅 DNS（灰云） | 1 分钟 |
| A | `emby` | 服务器 3 IP | 仅 DNS（灰云） | 1 分钟 |

- 同一个名称 `emby` 加多条 A 记录，Cloudflare 会轮流返回，用户就分到不同服务器。主节点也接流量的话也加进 `emby`。
- `master` 只指向主节点，要在第 4 步（从节点加入）之前生效。
- **`emby` 不要加 AAAA（IPv6）记录**：同一个客户端可能时而走 IPv4 时而走 IPv6，会被当成两个用户分到不同服务器。
- 域名如果有 CAA 记录，要包含 `letsencrypt.org`，否则申请证书会失败。
- 「SSL/TLS」页面的设置只对小黄云生效，灰云下不用改。
- 免费版没有健康检查：某台服务器挂了，要手动删掉它的那条 `emby` 记录，恢复后再加回（TTL 1 分钟，几分钟内生效）。
  也可以用命令管理（[配置手册 3.5](docs/配置手册.md#35-用-api-管理记录)），或开通付费的 Load Balancing 自动摘除
  （[配置手册 3.7](docs/配置手册.md#37-可选自动摘除故障服务器cloudflare-load-balancing)）。

验证解析：`dig +short emby.example.com @1.1.1.1` 应列出每台服务器的 IP。

### 第 6 步：防火墙和验证

- 每台放行站点对外端口（8001…），服务器之间转发也走这些端口
- 主节点放行 8443（更稳妥：只对各从节点和你自己的 IP 放行）

逐台验证（`1.2.3.4` 换成每台 IP）：

```bash
curl --resolve emby.example.com:8001:1.2.3.4 https://emby.example.com:8001/emby/System/Info/Public
```

每台都返回服务器信息 JSON 即正常。再用客户端实际播放、拖动，看各台访问日志的「节点」列确认转发情况。

---

## 5. 日常维护

```bash
# 更新（配置保留；多服务器时每台都执行）
bash <(curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/install.sh)

# 程序日志
docker logs -f --tail 100 emby-proxy

# nginx 自动应用的结果
cat /opt/emby-proxy/data/nginx/reload.log

# 卸载
systemctl disable --now emby-proxy-nginx.timer
rm -f /etc/systemd/system/emby-proxy-nginx.service /etc/systemd/system/emby-proxy-nginx.timer /etc/nginx/conf.d/emby-proxy.conf
systemctl daemon-reload && nginx -s reload
cd /opt/emby-proxy && docker compose down && cd / && rm -rf /opt/emby-proxy
```

**备份 / 迁移**：「设置 → 导入 / 导出配置」。导出文件不含面板密码，但**含各服务器上游地址，勿公开分享**；
证书不在导出范围内。导入可选「合并」（同 ID 覆盖、其余追加）或「替换全部」，导入前整体校验，有问题不改动任何配置。

---

## 6. 面板说明

### 站点字段

| 字段 | 说明 |
| --- | --- |
| 上游 Emby 地址 | 只写域名会补 `https://`；IP 或非 443 端口补 `http://`。带 `/emby` 会自动去掉 |
| 接入方式 | 独立端口（推荐）/ 路径前缀 / 独立域名，见[参考](#接入方式) |
| 推流 | 经本机中转（默认）/ 客户端直连推流，见[推流分离](#推流分离) |
| 推流域名白名单 | 播放时自动学习，一般不用填 |
| 客户端访问地址 | nginx 对外的地址，如 `https://emby.example.com:8001`。生成 nginx 配置和多服务器转发都依赖它 |
| 覆盖 User-Agent | 留空则透传客户端的 UA（推荐） |
| 自定义请求头 | 每行 `名称: 值`，值可用 `{public_base}`、`{public_origin}`、`{client_ip}` |
| 额外转发规则 | 每行 `路径 => 目标`，见[服务器自带推流路径](#服务器自带推流路径) |
| 传真实 IP | 把客户端 IP 用 `X-Forwarded-For` / `X-Real-IP` 传给上游，默认不传 |
| 上游证书过期仍然连接 | 只忽略有效期，仍校验域名和签发机构 |
| 完全不校验上游证书 | 自签证书等情况才用 |

卡片上：绿点运行中、灰点停用、红点出错（原因显示在卡片上）；「测试上游」请求上游 `/System/Info/Public`（模拟播放器 UA）。

### 统计口径

- **播放中**：按「客户端 IP + 片子」去重的播放会话数。片子取路径 `/videos/{id}/` 里的 id，取不到时按整个路径；
  一部片子的多个并发 Range 请求、HLS 分段算 1 个，最后一个视频请求结束后再保留 15 秒。
  多台设备在同一个 NAT 后面（同一个公网 IP）看同一部片子会算成 1 个。
- **连接**：本机正在处理的请求数，不含转发给其它节点的请求。
- **流量 / 请求**：只算本机实际访问上游的请求。转发给负责节点的请求由负责节点计数，
  转发这一段单独记为「节点间转发」（`relay_requests`、`relay_bytes`），各台相加不会重复。
- **主节点汇总**：从节点每 10 秒上报一次（`POST /api/cluster/report`，用同步令牌鉴权），主节点卡片和顶部数字显示本机 + 各在线节点之和，
  卡片下方小字列出各节点的播放和连接；超过 45 秒没上报的节点不计入。从节点面板只显示本机。
  主节点「清零统计」会以各节点当时的上报值为起点，之后显示增量；主节点重启后汇总的累计值会回到各节点的全量。

### 设置页

| 区块 | 作用 |
| --- | --- |
| 客户端地址显示 | 只影响卡片上显示的地址（路径 / 域名模式用） |
| 修改面板密码 | 每台服务器独立 |
| Nginx | 程序端口监听地址、IPv6、证书上传、自动应用状态、查看生成的配置 |
| 多服务器同步 | 角色、本机名称、公网 IP 和直连域名、面板 HTTPS 地址、同步令牌、从节点列表、同一用户固定同一台、视频直连 |
| 导入 / 导出 | 备份与迁移 |

---

## 7. 特殊服务器的适配

### 推流分离

很多服务器播放时 302 跳到另一个推流域名。

- **经本机中转**（默认）：自动识别推流域名，把跳转和 PlaybackInfo、m3u8 里的推流地址改写成经本机的地址
  （`/__ep/签名/https/推流域名/443/...`），视频流也走本机。只有上游跳转过或手动加入白名单的域名才允许中转，不会成为开放代理；
  地址带签名，多服务器时任何一台都能识别。播放请求遇到跳转由本机直接跟随，客户端只拿到 200/206，避免部分播放器丢 Range
  （症状：播放几秒卡住、不能拖动、Exo 提示不支持）。跳到无关的第三方域名时不带 Emby 凭据。
- **客户端直连推流**：只代理 API，视频流客户端直连推流服务器，省服务器流量，但播放线路和 IP 不经过你的服务器。

### 服务器自带推流路径

有的服务器给的 nginx 模板要求带特殊请求头，并把某个路径转到推流节点，例如：

```nginx
proxy_set_header MagpieCDN "$scheme://$server_name:$server_port";
location /magpiecdn { proxy_pass https://$arg_load.example.net; }
```

站点「高级选项」对应填：

- 自定义请求头：`MagpieCDN: {public_base}`
- 额外转发规则：`/magpiecdn => https://{arg.load}.example.net`

`{public_base}` 是客户端访问的地址；`{arg.load}` 取请求参数，只允许字母数字和 `-`。这类服务器用独立端口方式最稳。

### 按 IP 限制的服务器

上游默认看到的是**服务器的 IP**，所以服务器 IP 必须符合对方规则。对方官方反代示例会传 `X-Forwarded-For` 的，可以开「传真实 IP」。
有的服务器按 IP 限制登录次数：经反代所有用户共用服务器 IP，别反复输错密码。

### 按 User-Agent 拦截的服务器

有的服务器拦截浏览器 / curl，只放行播放器。反代原样透传客户端 UA，不受影响；「覆盖 User-Agent」请留空。
用浏览器打开反代地址被拦是正常的。

### 证书过期

- 上游证书过期：站点勾选「上游证书过期仍然连接」。
- 自己的证书过期：上传时勾选「允许上传已过期的证书」，客户端可能提示证书过期，尽快续期。

---

## 8. 常见问题

| 现象 | 排查 |
| --- | --- |
| 「测试」提示证书校验失败 / 已过期 | 勾选「上游证书过期仍然连接」；自签证书才用「完全不校验」 |
| 「测试」提示「被重定向到 …」 | 上游拦了服务器 IP 或 UA：确认 IP 符合对方规则，「覆盖 User-Agent」留空 |
| 能登录、能看海报，但不能播放 | 看访问日志播放请求的状态码：403 多为 IP 规则；502 为连不上推流；该服有自带推流路径见第 7 节 |
| 播放几秒卡住 / 不能拖动 | 日志里播放请求应为 206；nginx 配置必须是自动生成的（含 `proxy_buffering off`） |
| 登录返回 429 | 对方按 IP 限制登录次数，稍后再试 |
| 卡片红点「端口监听失败」 | 程序端口被占用，或与面板 / 入口端口冲突，换一个程序端口 |
| 「nginx 检查未通过」 | 看下方错误：`Address family not supported` = 没有 IPv6 却勾了 IPv6；`cannot load certificate` = 证书路径不对；`Address already in use` = 对外端口被占用或和程序端口相同。nginx 会继续用上一份配置 |
| 客户端连不上，`dig +short emby.example.com` 没有结果或不是服务器 IP | DNS 记录没加对或还没生效；确认是灰云 |
| 浏览器 / 客户端报证书错误 | 证书不包含 `emby.example.com`（看「设置 → Nginx → HTTPS 证书」显示的域名） |
| 「自动应用」显示未安装 | 安装时没有 nginx 或 systemd：装好 nginx 后重新执行安装命令 |
| 从节点「同步失败」 | 从节点上 `curl https://master.example.com:8443` 是否通；8443 是否放行；令牌是否被重新生成过 |
| 日志「→ 某节点 失联，本机处理」 | 从本机连不上那台的对外端口：查它的防火墙、nginx、「本机公网 IP」 |
| 浏览器打开反代地址被拦 / 跳错误页 | 部分服务器只允许 App，正常 |

---

## 9. 参考

### 接入方式

| 方式 | 客户端地址示例 | 说明 |
| --- | --- | --- |
| 独立端口（推荐） | `https://emby.example.com:8001` | 兼容性最好，见教程 |
| 路径前缀 | `https://emby.example.com/hk` | 一个 443 端口反代多个服务器 |
| 独立域名 | `https://hk.example.com` | 每个服务器一个子域名 |

路径前缀和独立域名共用入口端口 `55661`：自己写一个 nginx server 反代到 `http://127.0.0.1:55661`
（`proxy_pass` 末尾不要加 `/`，请求头和超时设置照抄生成的配置），并在「设置」里把入口公网地址填成 `https://你的域名`。
多服务器的转发和自动生成的 nginx 配置只覆盖独立端口方式。

### 服务器上的文件

| 路径 | 内容 |
| --- | --- |
| `/opt/emby-proxy/data/config.json` | 全部配置（含面板密码、密钥，权限 600） |
| `/opt/emby-proxy/data/ssl/` | 上传 / 同步来的证书 |
| `/opt/emby-proxy/data/nginx/emby-proxy.conf` | 程序生成的最新 nginx 配置 |
| `/opt/emby-proxy/data/nginx/applied.conf` | 通过检查、nginx 正在用的配置（`/etc/nginx/conf.d/emby-proxy.conf` 指向它） |
| `/opt/emby-proxy/data/nginx/reload.log` | 最近一次自动应用的结果 |
| `/opt/emby-proxy/data/stats.json` | 累计流量 |
| `/opt/emby-proxy/src/` | 源码（安装 / 更新时下载） |
| `/etc/systemd/system/emby-proxy-nginx.{service,timer}` | 每 15 秒检查并应用 nginx 配置 |

`config.json` 的完整字段和面板 API 见[配置手册](docs/配置手册.md#10-参考字段和接口)。

### 做了哪些处理

参考了 nginx 反代、Cloudflare Worker 反代脚本和 [emby-reverse-proxy-go](https://github.com/Gsy-allen/emby-reverse-proxy-go) 等常见做法：

- WebSocket（`/embywebsocket`）、Range 断点续传、无缓冲边收边发，长时间暂停不断开
- `Location` / `Content-Location` 改写；JSON / m3u8 / 文本里的上游地址改写（gzip 解压改写后再压缩，其它类型压缩原样透传）
- `/System/Info(/Public)` 的内外网地址改成反代地址，避免客户端绕过反代
- `Host` / `Origin` / `Referer` 改成上游；默认去掉 `X-Forwarded-For`、`X-Real-IP`、`CF-Connecting-IP` 等
- 面板登录 Cookie 不转发给上游；面板写接口有 CSRF 防护和登录限流

### 开发

```bash
go test ./...                                          # 测试
go build -o emby-proxy . && ./emby-proxy -config data/config.json   # 不用 Docker 直接运行
```
