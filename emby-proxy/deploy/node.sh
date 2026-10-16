#!/bin/bash
# Emby 反代：从节点一键部署（主节点用 deploy/master.sh 部署后，它会打印这条命令）
#
#   curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/deploy/node.sh -o /root/ep-node.sh
#   DOMAIN=example.com TOKEN=主节点的同步令牌 NODE_NAME=tokyo-1 PUBLIC_IP=5.6.7.8 \
#   MASTER_URL=https://master.example.com:8443 bash /root/ep-node.sh
#
# 参数：DOMAIN、TOKEN、NODE_NAME、PUBLIC_IP 必填；MASTER_URL 默认 https://master.$DOMAIN:8443；
#       EMBY_HOST 默认 emby.$DOMAIN；DIRECT_HOST 本机直连域名，默认 $NODE_NAME.$DOMAIN（解析不到本机 IP 时不启用，设为空字符串关闭）；
#       SKIP_INSTALL=1 / SKIP_FIREWALL=1 跳过对应步骤。可以重复执行。

set -euo pipefail

DOMAIN=${DOMAIN:?请设置 DOMAIN}
TOKEN=${TOKEN:?请设置 TOKEN（主节点脚本最后打印的同步令牌）}
NODE_NAME=${NODE_NAME:?请设置 NODE_NAME（本机名称，各台唯一）}
PUBLIC_IP=${PUBLIC_IP:?请设置 PUBLIC_IP（本机公网 IP）}
MASTER_URL=${MASTER_URL:-https://master.$DOMAIN:8443}
EMBY_HOST=${EMBY_HOST:-emby.$DOMAIN}
DIRECT_HOST=${DIRECT_HOST-$NODE_NAME.$DOMAIN}
DIR=${EP_DIR:-/opt/emby-proxy}
P=${EP_PANEL:-http://127.0.0.1:55660}
RESTART_CMD=${RESTART_CMD:-docker restart emby-proxy}
REPO_RAW=https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; N='\033[0m'
step() { echo -e "\n${G}==== $* ====${N}"; }
warn() { echo -e "${Y}注意：$*${N}"; }
die() { echo -e "${R}失败：$*${N}" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "请用 root 执行"

install_deps() {
    step "安装依赖（curl、python3、nginx、Docker）"
    if command -v apt-get >/dev/null; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq curl python3 nginx >/dev/null
    elif command -v dnf >/dev/null || command -v yum >/dev/null; then
        PM=$(command -v dnf || command -v yum)
        $PM install -y -q epel-release >/dev/null 2>&1 || true
        $PM install -y -q curl python3 nginx >/dev/null
    else
        die "不支持的系统（需要 apt / dnf / yum）"
    fi
    systemctl enable --now nginx >/dev/null 2>&1 || true
    if ! command -v docker >/dev/null; then
        curl -fsSL https://get.docker.com | sh >/dev/null
    fi
    systemctl enable --now docker >/dev/null 2>&1 || true
    docker compose version >/dev/null 2>&1 || die "缺少 docker compose 插件"
}

wait_panel() {
    for _ in $(seq 1 60); do
        curl -sS --noproxy '*' -o /dev/null "$P/" 2>/dev/null && return 0
        sleep 2
    done
    die "面板没有启动，查看：docker logs emby-proxy"
}

COOKIE=$(mktemp)
trap 'rm -f "$COOKIE"' EXIT
api() { curl -sS --noproxy '*' -b "$COOKIE" -c "$COOKIE" -H 'X-Emby-Proxy: 1' -H 'Content-Type: application/json' -X "$1" "$P$2" ${3:+-d "$3"}; }
json() { python3 -c "import json,sys;d=json.load(sys.stdin);r=$1;print(r if not isinstance(r,(dict,list)) else json.dumps(r,ensure_ascii=False))"; }
must_ok() { local out; out=$(cat); echo "$out" | grep -q '"ok":true' || die "$1：$out"; }
resolves_to() { python3 -c "import socket,sys;sys.exit(0 if sys.argv[2] in {a[4][0] for a in socket.getaddrinfo(sys.argv[1],None,socket.AF_INET)} else 1)" "$1" "$2" 2>/dev/null; }
login() {
    local pw
    pw=$(sed -n 's/.*"admin_password": "\(.*\)".*/\1/p' "$DIR/data/config.json")
    [ -n "$pw" ] || die "读不到面板密码（$DIR/data/config.json）"
    api POST /api/login "{\"password\":\"$pw\"}" | must_ok "面板登录失败"
}

# ---------- 开始 ----------
if [ "${SKIP_INSTALL:-}" != 1 ]; then
    install_deps
    step "安装 / 更新 emby-proxy"
    bash <(curl -fsSL "$REPO_RAW/install.sh")
fi
wait_panel
login

step "检查能否连上主节点"
code=$(curl -sS -m 15 -o /dev/null -w '%{http_code}' "$MASTER_URL/api/cluster/sync" 2>&1) || true
case "$code" in
    401) echo "主节点 $MASTER_URL 可以连接" ;;
    *) die "连不上主节点 $MASTER_URL（返回：$code）。检查主节点是否已部署、master 域名解析、主节点防火墙是否放行面板端口" ;;
esac

step "加入集群（从节点）"
if [ -n "$DIRECT_HOST" ] && ! resolves_to "$DIRECT_HOST" "$PUBLIC_IP"; then
    warn "$DIRECT_HOST 没有解析到 $PUBLIC_IP，不启用本机直连域名（其它服务器会把视频请求转发过来，而不是 302）"
    DIRECT_HOST=""
fi
api PUT /api/cluster "{\"role\":\"node\",\"node_name\":\"$NODE_NAME\",\"advertise_addr\":\"$PUBLIC_IP\",\"master_url\":\"$MASTER_URL\",\"token\":\"$TOKEN\",\"direct_host\":\"$DIRECT_HOST\"}" \
    | must_ok "设置从节点"
echo "本机直连域名：${DIRECT_HOST:-未启用}"

echo "等待同步…"
for _ in $(seq 1 30); do
    st=$(api GET /api/state)
    if echo "$st" | json "d['cluster'].get('status',{}).get('last_ok',0)" | grep -qv '^0$' \
       && [ "$(echo "$st" | json "len(d['sites'])")" -gt 0 ]; then
        break
    fi
    sleep 2
done
st=$(api GET /api/state)
err=$(echo "$st" | json "d['cluster'].get('status',{}).get('error','')")
[ -z "$err" ] || die "同步失败：$err"
echo "已同步 $(echo "$st" | json "len(d['sites'])") 个站点；证书：$(echo "$st" | json "', '.join((d['settings'].get('cert') or {}).get('names',[])) or '无'")"
echo "参与分配的节点：$(echo "$st" | json "', '.join(m['name']+'('+m['addr']+')' for m in d['cluster'].get('members') or [])")"

step "防火墙"
mapfile -t ports < <(echo "$st" | python3 -c "
import json,sys,urllib.parse
for s in json.load(sys.stdin)['sites']:
    u=urllib.parse.urlparse(s.get('public_url') or '')
    if u.port: print(u.port)")
if [ "${SKIP_FIREWALL:-}" != 1 ] && [ ${#ports[@]} -gt 0 ]; then
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        for p in "${ports[@]}"; do ufw allow "$p/tcp" >/dev/null; done; echo "ufw 已放行：${ports[*]}"
    elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
        for p in "${ports[@]}"; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
        firewall-cmd --reload >/dev/null; echo "firewalld 已放行：${ports[*]}"
    else
        echo "未检测到启用的 ufw / firewalld，跳过（如果服务商有安全组，请在控制台放行：${ports[*]}）"
    fi
fi

step "等待 nginx 自动应用配置"
applied=0
for _ in $(seq 1 45); do
    api GET /api/state | json "d['nginx']" | grep -q '"applied": true' && { applied=1; break; }
    sleep 2
done
[ "$applied" = 1 ] && echo "nginx 已应用最新配置" || warn "nginx 还没应用成功：$(head -c 600 "$DIR/data/nginx/reload.log" 2>/dev/null)"

step "面板改为只监听本机"
if ! grep -q '"admin_listen": "127.0.0.1:55660"' "$DIR/data/config.json"; then
    sed -i 's/"admin_listen": ":55660"/"admin_listen": "127.0.0.1:55660"/' "$DIR/data/config.json"
    $RESTART_CMD >/dev/null
    sleep 2; wait_panel; login
fi
echo "面板只监听 127.0.0.1:55660"

step "逐个验证站点（经本机 nginx HTTPS）"
fail=0
while IFS='|' read -r name port; do
    [ -n "$port" ] || continue
    out=$(curl -sS --noproxy '*' -m 20 -A "SenPlayer/6.2.1" --resolve "$EMBY_HOST:$port:127.0.0.1" \
        "https://$EMBY_HOST:$port/emby/System/Info/Public" 2>&1) || true
    if echo "$out" | grep -q '"ServerName"'; then
        echo -e "  ${G}✓${N} $name  https://$EMBY_HOST:$port  →  $(echo "$out" | json "d.get('ServerName','')+' '+d.get('Version','')")"
    else
        echo -e "  ${R}✗${N} $name  https://$EMBY_HOST:$port  →  ${out:0:200}"; fail=1
    fi
done < <(api GET /api/state | python3 -c "
import json,sys,urllib.parse
for s in json.load(sys.stdin)['sites']:
    u=urllib.parse.urlparse(s.get('public_url') or '')
    if s.get('enabled') and u.port: print(s['name']+'|'+str(u.port))")

step "完成"
[ "$fail" = 0 ] || { warn "有站点验证失败，见上方 ✗"; exit 1; }
echo "从节点 $NODE_NAME 已加入，配置由主节点同步"
