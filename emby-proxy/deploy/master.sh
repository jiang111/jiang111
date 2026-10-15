#!/bin/bash
# Emby 反代：主节点一键部署（也可用于单台服务器）
#
# 用法（参数用环境变量传入，不要把令牌、上游地址写进仓库）：
#   curl -fsSL https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy/deploy/master.sh -o /root/ep-master.sh
#   DOMAIN=example.com NODE_NAME=hk-1 PUBLIC_IP=1.2.3.4 \
#   CF_Token=xxx CF_Zone_ID=xxx \
#   SITES='A服=https://emby-a.example.net
#   B服=https://emby-b.example.net' \
#   bash /root/ep-master.sh
#
# 参数：
#   DOMAIN        根域名（必填），客户端用 emby.$DOMAIN，主节点面板用 master.$DOMAIN
#   NODE_NAME     本机名称（必填，各台唯一）
#   PUBLIC_IP     本机公网 IP（必填）
#   CF_Token / CF_Zone_ID  Cloudflare 令牌和 Zone ID，用 acme.sh 申请 *.$DOMAIN 通配符证书；不填则跳过（需自己在面板上传证书）
#   SITES         每行一个「名称=上游地址」，按顺序分配对外端口 FIRST_PORT、FIRST_PORT+1…（程序端口 +10000）
#   FIRST_PORT    第一个站点的对外端口，默认 8001
#   EMBY_HOST     客户端域名，默认 emby.$DOMAIN
#   MASTER_HOST   主节点面板域名，默认 master.$DOMAIN；设为空字符串则不配置多服务器（单台）
#   PANEL_PORT    主节点面板 HTTPS 端口，默认 8443
#   IPV6=1        nginx 同时监听 IPv6（所有服务器都有 IPv6 时才开）
#   SKIP_INSTALL=1 / SKIP_ACME=1 / SKIP_FIREWALL=1  跳过对应步骤（重复执行时用）
#
# 可以重复执行：已存在的同端口站点会跳过。

set -euo pipefail

DOMAIN=${DOMAIN:?请设置 DOMAIN，例如 DOMAIN=example.com}
NODE_NAME=${NODE_NAME:?请设置 NODE_NAME，例如 NODE_NAME=hk-1}
PUBLIC_IP=${PUBLIC_IP:?请设置 PUBLIC_IP（本机公网 IP）}
EMBY_HOST=${EMBY_HOST:-emby.$DOMAIN}
MASTER_HOST=${MASTER_HOST-master.$DOMAIN}
PANEL_PORT=${PANEL_PORT:-8443}
FIRST_PORT=${FIRST_PORT:-8001}
SITES=${SITES:-}
CF_Token=${CF_Token:-}
CF_Zone_ID=${CF_Zone_ID:-}
DIR=${EP_DIR:-/opt/emby-proxy}
P=${EP_PANEL:-http://127.0.0.1:55660}
RESTART_CMD=${RESTART_CMD:-docker restart emby-proxy}
REPO_RAW=https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy

G='\033[0;32m'; R='\033[0;31m'; Y='\033[0;33m'; N='\033[0m'
step() { echo -e "\n${G}==== $* ====${N}"; }
warn() { echo -e "${Y}注意：$*${N}"; }
die() { echo -e "${R}失败：$*${N}" >&2; exit 1; }

[ "$(id -u)" = 0 ] || die "请用 root 执行"

# ---------- 公共函数 ----------
install_deps() {
    step "安装依赖（curl、python3、cron、nginx、Docker）"
    if command -v apt-get >/dev/null; then
        export DEBIAN_FRONTEND=noninteractive
        apt-get update -qq
        apt-get install -y -qq curl python3 cron nginx >/dev/null
    elif command -v dnf >/dev/null || command -v yum >/dev/null; then
        PM=$(command -v dnf || command -v yum)
        $PM install -y -q epel-release >/dev/null 2>&1 || true
        $PM install -y -q curl python3 cronie nginx >/dev/null
        systemctl enable --now crond >/dev/null 2>&1 || true
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

install_proxy() {
    step "安装 / 更新 emby-proxy"
    bash <(curl -fsSL "$REPO_RAW/install.sh")
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
# json 'python 表达式'：对标准输入的 JSON 求值（变量 d），打印结果
json() { python3 -c "import json,sys;d=json.load(sys.stdin);r=$1;print(r if not isinstance(r,(dict,list)) else json.dumps(r,ensure_ascii=False))"; }
must_ok() { local out; out=$(cat); echo "$out" | grep -q '"ok":true' || die "$1：$out"; }

login() {
    local pw
    pw=$(sed -n 's/.*"admin_password": "\(.*\)".*/\1/p' "$DIR/data/config.json")
    [ -n "$pw" ] || die "读不到面板密码（$DIR/data/config.json）"
    api POST /api/login "{\"password\":\"$pw\"}" | must_ok "面板登录失败"
}

open_firewall() {
    [ "${SKIP_FIREWALL:-}" = 1 ] && return 0
    local ports=("$@")
    if command -v ufw >/dev/null && ufw status 2>/dev/null | grep -q "Status: active"; then
        for p in "${ports[@]}"; do ufw allow "$p/tcp" >/dev/null; done
        echo "ufw 已放行：${ports[*]}"
    elif command -v firewall-cmd >/dev/null && firewall-cmd --state >/dev/null 2>&1; then
        for p in "${ports[@]}"; do firewall-cmd --permanent --add-port="$p/tcp" >/dev/null; done
        firewall-cmd --reload >/dev/null
        echo "firewalld 已放行：${ports[*]}"
    else
        echo "未检测到启用的 ufw / firewalld，跳过（如果服务商有安全组，请在控制台放行：${ports[*]}）"
    fi
}

lock_panel() {
    step "面板改为只监听本机"
    if grep -q '"admin_listen": "127.0.0.1:55660"' "$DIR/data/config.json"; then
        echo "已经是只监听本机"
    else
        sed -i 's/"admin_listen": ":55660"/"admin_listen": "127.0.0.1:55660"/' "$DIR/data/config.json"
        $RESTART_CMD >/dev/null
        sleep 2
        wait_panel
        login
    fi
    if command -v ss >/dev/null; then
        ss -ltn | grep -q '0.0.0.0:55660\|\*:55660\|\[::\]:55660' && warn "55660 仍对外监听" || echo "面板只监听 127.0.0.1:55660"
    fi
}

wait_nginx() {
    step "等待 nginx 自动应用配置"
    for _ in $(seq 1 45); do
        local st
        st=$(api GET /api/state | json "d['nginx']")
        if echo "$st" | grep -q '"applied": true'; then
            echo "nginx 已应用最新配置"
            return 0
        fi
        if echo "$st" | grep -q '"installed": false'; then
            warn "宿主机没有开启 nginx 自动应用（缺 nginx 或 systemd），请检查后重新执行"
            return 1
        fi
        sleep 2
    done
    warn "nginx 还没应用成功，最近一次结果：$(head -c 600 "$DIR/data/nginx/reload.log" 2>/dev/null)"
    return 1
}

verify_sites() {
    step "逐个验证站点（经本机 nginx HTTPS）"
    local fail=0
    while IFS='|' read -r name port; do
        [ -n "$port" ] || continue
        local out
        out=$(curl -sS --noproxy '*' -m 20 -A "SenPlayer/6.2.1" --resolve "$EMBY_HOST:$port:127.0.0.1" \
            "https://$EMBY_HOST:$port/emby/System/Info/Public" 2>&1) || true
        if echo "$out" | grep -q '"ServerName"'; then
            echo -e "  ${G}✓${N} $name  https://$EMBY_HOST:$port  →  $(echo "$out" | json "d.get('ServerName','')+' '+d.get('Version','')")"
        else
            echo -e "  ${R}✗${N} $name  https://$EMBY_HOST:$port  →  ${out:0:200}"
            fail=1
        fi
    done < <(api GET /api/state | python3 -c "
import json,sys,urllib.parse
for s in json.load(sys.stdin)['sites']:
    u=urllib.parse.urlparse(s.get('public_url') or '')
    if s.get('enabled') and u.port: print(s['name']+'|'+str(u.port))")
    return $fail
}

# ---------- 开始 ----------
[ "${SKIP_INSTALL:-}" = 1 ] || { install_deps; install_proxy; }
wait_panel
login

step "基础设置"
api PUT /api/settings '{"port_bind":"127.0.0.1"}' | must_ok "设置监听地址"
if [ "${IPV6:-0}" = 1 ]; then v6=true; else v6=false; fi
api PUT /api/settings "{\"nginx_ipv6\":$v6}" | must_ok "设置 IPv6"
echo "站点端口只监听本机；IPv6：$v6"

if [ "${SKIP_ACME:-}" != 1 ] && [ -n "$CF_Token" ] && [ -n "$CF_Zone_ID" ]; then
    step "申请 *.$DOMAIN 通配符证书（acme.sh + Cloudflare DNS 验证）"
    if [ ! -x ~/.acme.sh/acme.sh ]; then
        curl -fsSL https://get.acme.sh | sh -s ${ACME_EMAIL:+email=$ACME_EMAIL} >/dev/null
    fi
    export CF_Token CF_Zone_ID
    ~/.acme.sh/acme.sh --issue --dns dns_cf -d "$DOMAIN" -d "*.$DOMAIN" --server letsencrypt --keylength ec-256 \
        || [ $? -eq 2 ]   # 2 = 证书还没到续期时间，已存在
    mkdir -p "$DIR/data/ssl"
    ~/.acme.sh/acme.sh --install-cert -d "$DOMAIN" --ecc \
        --fullchain-file "$DIR/data/ssl/fullchain.pem" \
        --key-file "$DIR/data/ssl/privkey.pem" >/dev/null
    api PUT /api/settings '{"nginx_cert":"","nginx_key":""}' | must_ok "使用数据目录里的证书"
    echo "证书已安装，等待程序识别…"
    for _ in $(seq 1 45); do
        api GET /api/state | grep -q "\"\\*.$DOMAIN\"" && break
        sleep 2
    done
    api GET /api/state | json "d['settings'].get('cert')" | grep -q "$DOMAIN" \
        && echo "程序已识别证书：$(api GET /api/state | json "', '.join(d['settings']['cert']['names'])+'，到期 '+__import__('time').strftime('%Y-%m-%d',__import__('time').localtime(d['settings']['cert']['not_after']))")" \
        || warn "程序还没识别到证书，稍后在面板「设置 → Nginx」查看"
elif [ "${SKIP_ACME:-}" != 1 ]; then
    warn "没有提供 CF_Token / CF_Zone_ID，跳过证书申请，请在面板「设置 → Nginx」上传证书"
fi

if [ -n "$MASTER_HOST" ]; then
    step "设为主节点"
    api PUT /api/cluster "{\"role\":\"master\",\"node_name\":\"$NODE_NAME\",\"advertise_addr\":\"$PUBLIC_IP\",\"panel_url\":\"https://$MASTER_HOST:$PANEL_PORT\",\"sticky\":true}" \
        | must_ok "设置主节点"
    echo "面板 HTTPS：https://$MASTER_HOST:$PANEL_PORT"
fi

step "添加站点"
port=$FIRST_PORT
fw_ports=()
existing=$(api GET /api/state | python3 -c "import json,sys;print(' '.join(str(s.get('port')) for s in json.load(sys.stdin)['sites']))")
while IFS= read -r line; do
    line=$(echo "$line" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
    [ -n "$line" ] || continue
    name=${line%%=*}; up=${line#*=}
    [ "$name" != "$line" ] || die "SITES 格式应为「名称=上游地址」：$line"
    inner=$((port + 10000))
    fw_ports+=("$port")
    if echo " $existing " | grep -q " $inner "; then
        echo "  - $name：端口 $inner 已有站点，跳过"
    else
        t=$(api POST /api/test "{\"upstream\":\"$up\"}")
        if echo "$t" | grep -q '"ok":true'; then
            echo "  测试上游 $name：$(echo "$t" | json "d['server_name']+' '+d['version']+'，'+str(d['ms'])+' ms'")"
        else
            warn "上游 $name 测试失败：$(echo "$t" | json "d.get('error')")（仍然添加，稍后排查）"
        fi
        body=$(python3 -c "import json,sys;print(json.dumps({'name':sys.argv[1],'upstream':sys.argv[2],'mode':'port','port':int(sys.argv[3]),'public_url':'https://%s:%s'%(sys.argv[4],sys.argv[5]),'stream_mode':'proxy','enabled':True},ensure_ascii=False))" \
            "$name" "$up" "$inner" "$EMBY_HOST" "$port")
        r=$(api POST /api/sites "$body")
        echo "$r" | grep -q '"ok":true' || die "添加站点 $name 失败：$r"
        err=$(echo "$r" | json "d.get('error') or ''")
        [ -z "$err" ] && echo -e "  ${G}+${N} $name  https://$EMBY_HOST:$port  （程序端口 $inner）" || warn "$name：$err"
    fi
    port=$((port + 1))
done <<< "$SITES"

step "防火墙"
[ -n "$MASTER_HOST" ] && fw_ports+=("$PANEL_PORT")
[ ${#fw_ports[@]} -gt 0 ] && open_firewall "${fw_ports[@]}"

wait_nginx || true
lock_panel
verify_sites && ok=1 || ok=0

step "完成"
if [ -n "$MASTER_HOST" ]; then
    TOKEN=$(sed -n 's/.*"token": "\(.*\)".*/\1/p' "$DIR/data/config.json")
    echo "主节点面板：https://$MASTER_HOST:$PANEL_PORT （密码在 $DIR/data/config.json 的 admin_password）"
    echo
    echo "在每台从节点上执行（把 NODE_NAME、PUBLIC_IP 换成那台的）："
    echo
    echo "curl -fsSL $REPO_RAW/deploy/node.sh -o /root/ep-node.sh && \\"
    echo "DOMAIN=$DOMAIN TOKEN=$TOKEN NODE_NAME=从节点名称 PUBLIC_IP=从节点IP \\"
    echo "MASTER_URL=https://$MASTER_HOST:$PANEL_PORT EMBY_HOST=$EMBY_HOST bash /root/ep-node.sh"
fi
[ "$ok" = 1 ] || { warn "有站点验证失败，见上方 ✗"; exit 1; }
