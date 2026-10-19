# Emby 保号

> 用 [emby-proxy](../emby-proxy) 的话，面板「保号」页已内置同样的功能（账号存在站点里），不需要再单独部署这个。

每 10 天（`interval_days`）在台湾时间凌晨 4 点（`run_at`）自动登录每个 Emby 账号，随机选一部电影/剧集，模拟播放 `watch_minutes` 分钟
（向服务器上报 开始播放 → 每 10 秒进度 → 停止播放），在服务端的「活动/播放记录」中留下观看记录。

## 安装（1Panel）

1. 1Panel → 终端（或 SSH）执行：
   ```bash
   bash <(curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-keepalive/install.sh)
   ```
2. 1Panel → 文件 → 打开 `/opt/emby-keepalive/config.json`，填入账号。
3. 先测试一次（会真的看 6~9 分钟，结束后打印结果）：
   ```bash
   docker run --rm -v /opt/emby-keepalive:/data python:3.12-alpine python3 -u /data/keepalive.py --once
   ```
4. 测试成功后启动常驻容器：
   ```bash
   cd /opt/emby-keepalive && docker compose up -d
   ```
   也可以在 1Panel → 容器 → 编排 → 创建编排，粘贴 `docker-compose.yml` 的内容。

日志：`docker logs -f emby-keepalive`（或在 1Panel 容器列表里点日志）。

## 网页面板

浏览器打开 `http://服务器IP:55656`，用户名随便填，密码是 config.json 里的 `web.password`
（安装时自动生成并打印）。可以看到上次成功/下次执行时间、实时日志，并有「立即执行」按钮。

- 需要在 1Panel → 主机 → 防火墙 放行 55656 端口；也可以不开端口，在 1Panel → 网站 里用反向代理 `http://127.0.0.1:55656` 挂上 HTTPS 域名。
- `web.password` 为空或 `CHANGE_ME` 时不启用面板。

## 抓取客户端身份

有的服务器只允许特定客户端登录（其他客户端能登、脚本却提示 401）。这时在手机客户端（如 SenPlayer）里添加一个
Emby 服务器，地址填 `http://服务器IP:55656/capture`，用户名密码随便填并点登录。网页面板底部会显示该客户端真实发送的
User-Agent 和 X-Emby-Authorization，并生成对应的 `client` 配置，复制到 config.json 的 `settings` 里即可。
（抓取接口不记录密码。）

## 手动执行

网页上点「立即执行」，或者命令行（成功后下次自动执行从今天重新算 `interval_days` 天）：
```bash
docker exec emby-keepalive python3 -u /data/keepalive.py --once
```
也可以在 1Panel → 容器 → `emby-keepalive` → 终端 里执行 `python3 -u /data/keepalive.py --once`。

## 配置说明

| 字段 | 说明 |
| --- | --- |
| `run_at` | 几点开始执行，默认 `"04:00"` |
| `interval_days` | 每几天执行一次，默认 10（要求 15 天，留出余量） |
| `utc_offset` | 时区，默认 8（台湾 UTC+8） |
| `watch_minutes` | 每次观看时长范围（分钟），在范围内随机 |
| `report_interval` | 进度上报间隔（秒） |
| `stream_kbps` | >0 时同时真实拉取视频流（kbps），服务器按流量判定时再开，如 `500` |
| `retries` | 失败重试次数；整轮失败后 2 小时再试 |
| `verify_ssl` | 服务器证书有问题时改为 `false` |
| `client` | 伪装的客户端身份（Client / Version / Device / User-Agent），默认 SenPlayer；也可以写在单个账号里覆盖 |
| `web` | 网页面板端口和密码 |
| `telegram` | 填 bot_token 和 chat_id 后每次执行完推送结果 |
| `accounts[].server` | 服务器地址，带端口，如 `https://emby.xxx.com:443`；如果登录 404，试试在末尾加 `/emby` |
| `accounts[].enabled` | `false` 可临时停用某个账号 |

修改 config.json 后无需重启，下一轮自动读取。执行状态保存在 `state.json`；首次启动（没有 state.json）会立即执行一轮，之后按 `run_at` 执行。删除它并重启容器即可立即重新执行一轮。
