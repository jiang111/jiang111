#!/usr/bin/env bash
# ============================================================
#  cf-ddns.sh  —  Cloudflare DDNS 一键管理脚本 (Linux + macOS)
# ============================================================
#  功能：
#    - 配置 Cloudflare API Token + 域名，自动建立/更新 DNS 记录
#    - 多条记录管理：新增 / 编辑 / 删除 / 启用 / 停用
#    - 自定义检查间隔（分钟）
#    - 手动立即检查
#    - 开机自启动 / 暂停 / 启动
#        Linux: systemd timer
#        macOS: launchd LaunchAgent
#    - Telegram 通知（IP 变更/错误推送）
#    - 完全卸载
#
#  依赖：
#    通用:        bash, curl, jq
#    Linux:       systemd (systemctl)
#    macOS:       launchctl  (Homebrew 安装 jq:  brew install jq)
#
#  运行：
#    ./cf-ddns.sh         首次运行向导 / 管理菜单
#    ./cf-ddns.sh --run   供 systemd / launchd 调用
# ============================================================

set -uo pipefail

VERSION="2.2.0"
CONFIG_DIR="${CF_DDNS_DIR:-$HOME/.cf-ddns}"
RECORDS_DIR="$CONFIG_DIR/records"
CACHE_DIR="$CONFIG_DIR/cache"
GLOBAL_CONF="$CONFIG_DIR/config"
LOG_FILE="$CONFIG_DIR/ddns.log"
INSTALL_PATH="$CONFIG_DIR/cf-ddns.sh"

# 服务名 / 标识
SERVICE_NAME="cf-ddns"
LAUNCHD_LABEL="com.cf-ddns"

# 平台相关路径，detect_os 中填充
OS_TYPE=""
SYSTEMD_DIR="/etc/systemd/system"
LAUNCHD_DIR="$HOME/Library/LaunchAgents"
LAUNCHD_PLIST=""
SUDO=""

# -------------------- 颜色 / 日志 --------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'

log()  { echo "[$(date '+%F %T')] $*" >> "$LOG_FILE"; }
info() { echo -e "${BLUE}[*]${NC} $*"; }
ok()   { echo -e "${GREEN}[✓]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[✗]${NC} $*" >&2; }

# -------------------- 平台兼容层 --------------------
detect_os() {
    case "$(uname -s)" in
        Linux*)  OS_TYPE="linux"  ;;
        Darwin*) OS_TYPE="macos"  ;;
        *)
            err "不支持的系统: $(uname -s)（仅支持 Linux 和 macOS）"
            exit 1
            ;;
    esac
    LAUNCHD_PLIST="$LAUNCHD_DIR/$LAUNCHD_LABEL.plist"
    # 只有 Linux 上需要 sudo（macOS 用 user LaunchAgent）
    if [[ "$OS_TYPE" == "linux" && $EUID -ne 0 ]]; then
        SUDO="sudo"
    fi
}

# macOS 没有 readlink -f；做一个跨平台版本
resolve_path() {
    local target="$1"
    # 优先 readlink -f / greadlink -f
    if readlink -f / >/dev/null 2>&1; then
        readlink -f "$target"; return
    fi
    if command -v greadlink >/dev/null 2>&1; then
        greadlink -f "$target"; return
    fi
    # 纯 bash fallback：cd + pwd
    local dir base
    if [[ -d "$target" ]]; then
        ( cd "$target" && pwd )
    else
        dir=$(cd "$(dirname "$target")" 2>/dev/null && pwd) || { echo "$target"; return; }
        base=$(basename "$target")
        echo "$dir/$base"
    fi
}

# macOS 没有 sha256sum，用 shasum 替代；同时回退 md5
hash_stream() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum | awk '{print $1}'
    elif command -v shasum >/dev/null 2>&1; then
        shasum -a 256 | awk '{print $1}'
    else
        # 极端 fallback
        md5sum 2>/dev/null | awk '{print $1}' || md5 -q
    fi
}

# macOS BSD date 没有纳秒（%N）
new_id() {
    echo "$(date +%s)-$$-$RANDOM-$RANDOM" | hash_stream | head -c 8
}

SCRIPT_PATH="$(resolve_path "$0")"

# -------------------- 依赖检查 --------------------
check_deps() {
    local missing=()
    for c in curl jq; do command -v "$c" >/dev/null 2>&1 || missing+=("$c"); done
    if [[ ${#missing[@]} -gt 0 ]]; then
        err "缺少依赖: ${missing[*]}"
        case "$OS_TYPE" in
            linux)
                info "Debian/Ubuntu:  sudo apt install -y ${missing[*]}"
                info "RHEL/CentOS:    sudo yum install -y ${missing[*]}"
                info "Arch:           sudo pacman -S ${missing[*]}"
                ;;
            macos)
                info "macOS (Homebrew):  brew install ${missing[*]}"
                if ! command -v brew >/dev/null 2>&1; then
                    info "未检测到 Homebrew，请先安装: https://brew.sh"
                fi
                ;;
        esac
        exit 1
    fi

    case "$OS_TYPE" in
        linux)
            if ! command -v systemctl >/dev/null 2>&1; then
                err "未检测到 systemd (systemctl)，本脚本在 Linux 上需要 systemd"
                exit 1
            fi
            ;;
        macos)
            if ! command -v launchctl >/dev/null 2>&1; then
                err "未检测到 launchctl"
                exit 1
            fi
            ;;
    esac
}

ensure_dirs() {
    mkdir -p "$CONFIG_DIR" "$RECORDS_DIR" "$CACHE_DIR"
    chmod 700 "$CONFIG_DIR" "$RECORDS_DIR" "$CACHE_DIR"
    [[ -f "$LOG_FILE" ]] || touch "$LOG_FILE"
    [[ "$OS_TYPE" == "macos" ]] && mkdir -p "$LAUNCHD_DIR"
}

# -------------------- 全局配置 --------------------
load_config() {
    INTERVAL=5
    TG_BOT_TOKEN=""
    TG_CHAT_ID=""
    TG_NOTIFY_LEVEL="changes"   # off | errors | changes | all
    if [[ -f "$GLOBAL_CONF" ]]; then
        # shellcheck source=/dev/null
        source "$GLOBAL_CONF"
    fi
}

save_config() {
    {
        echo "INTERVAL=${INTERVAL:-5}"
        printf "TG_BOT_TOKEN=%q\n" "${TG_BOT_TOKEN:-}"
        printf "TG_CHAT_ID=%q\n"   "${TG_CHAT_ID:-}"
        echo "TG_NOTIFY_LEVEL=${TG_NOTIFY_LEVEL:-changes}"
    } > "$GLOBAL_CONF"
    chmod 600 "$GLOBAL_CONF"
}

get_interval() { load_config; echo "${INTERVAL:-5}"; }

set_interval() {
    load_config
    INTERVAL="$1"
    save_config
}

# -------------------- Cloudflare API --------------------
get_public_ip() {
    local type="${1:-A}" ip=""
    if [[ "$type" == "AAAA" ]]; then
        ip=$(curl -sS -6 --max-time 10 https://api64.ipify.org 2>/dev/null || true)
        [[ -z "$ip" ]] && ip=$(curl -sS -6 --max-time 10 https://ipv6.icanhazip.com 2>/dev/null || true)
    else
        ip=$(curl -sS -4 --max-time 10 https://api.ipify.org 2>/dev/null || true)
        [[ -z "$ip" ]] && ip=$(curl -sS -4 --max-time 10 https://ipv4.icanhazip.com 2>/dev/null || true)
    fi
    echo "${ip// /}"
}

cf_api() {
    local method="$1" path="$2" token="$3" data="${4:-}"
    if [[ -n "$data" ]]; then
        curl -sS --max-time 15 -X "$method" \
            -H "Authorization: Bearer $token" \
            -H "Content-Type: application/json" \
            --data "$data" "https://api.cloudflare.com/client/v4$path"
    else
        curl -sS --max-time 15 -X "$method" \
            -H "Authorization: Bearer $token" \
            -H "Content-Type: application/json" \
            "https://api.cloudflare.com/client/v4$path"
    fi
}

verify_token() {
    local token="$1" resp
    resp=$(cf_api GET "/user/tokens/verify" "$token")
    [[ "$(echo "$resp" | jq -r '.success')" == "true" ]]
}

# 根据完整域名找出对应的 zone（取最长后缀匹配）
# 旧实现：列出全部 zone 后本地过滤。需要账号级 Zone:Read，且写死 per_page=50。
# 留作 fallback。
find_zone() {
    local record="$1" token="$2"
    cf_api GET "/zones?per_page=50" "$token" | jq -r \
        --arg rec "$record" '
        .result
        | map(select($rec == .name or ($rec | endswith("." + .name))))
        | sort_by(.name | length) | reverse | .[0]
        | "\(.id) \(.name)"' 2>/dev/null
}

# 按域名后缀逐级用 /zones?name=X 探测 zone。
# 比 find_zone 更鲁棒：精确匹配单条 zone，不需要列表权限，对 zone-scoped Token 友好。
# 输出 "zone_id zone_name"，失败返回非 0。
auto_zone() {
    local record="$1" token="$2"
    local candidate="$record" resp id name
    while [[ "$candidate" == *.* ]]; do
        resp=$(cf_api GET "/zones?name=$candidate" "$token")
        id=$(echo "$resp" | jq -r '.result[0].id // empty' 2>/dev/null)
        name=$(echo "$resp" | jq -r '.result[0].name // empty' 2>/dev/null)
        if [[ -n "$id" && -n "$name" ]]; then
            echo "$id $name"
            return 0
        fi
        candidate="${candidate#*.}"
    done
    return 1
}

# -------------------- Telegram 通知 --------------------
tg_send() {
    local message="$1"
    load_config
    [[ -z "${TG_BOT_TOKEN:-}" || -z "${TG_CHAT_ID:-}" ]] && return 0
    curl -sS --max-time 10 \
        -X POST "https://api.telegram.org/bot${TG_BOT_TOKEN}/sendMessage" \
        -d "chat_id=${TG_CHAT_ID}" \
        --data-urlencode "text=${message}" \
        -d "parse_mode=HTML" \
        -d "disable_web_page_preview=true" \
        > /dev/null 2>&1 || true
}

tg_notify() {
    local level="$1" message="$2"   # level: error | change | info
    load_config
    case "${TG_NOTIFY_LEVEL:-changes}" in
        off)     return 0 ;;
        errors)  [[ "$level" == "error" ]] || return 0 ;;
        changes) [[ "$level" == "error" || "$level" == "change" ]] || return 0 ;;
        all)     ;;
        *)       [[ "$level" == "error" || "$level" == "change" ]] || return 0 ;;
    esac
    local hn; hn=$(hostname 2>/dev/null || echo "server")
    local full="$message

🖥 <code>$hn</code>  ⏰ $(date '+%F %T')"
    tg_send "$full"
}

# -------------------- 同步逻辑 --------------------
run_one_record() {
    local conf="$1"
    # shellcheck source=/dev/null
    source "$conf"

    local label="${NAME:-$(basename "$conf" .conf)}"

    if [[ "${ENABLED:-true}" != "true" ]]; then
        log "[$label] 已停用，跳过"
        return 0
    fi

    local current_ip
    current_ip=$(get_public_ip "${RECORD_TYPE:-A}")
    if [[ -z "$current_ip" ]]; then
        log "[$label] 获取公网 IP 失败"
        tg_notify error "❌ <b>DDNS 错误</b>
📍 <code>$RECORD_NAME</code>
原因: 获取公网 IP 失败"
        return 1
    fi

    # 命中本地缓存：上次成功同步后 IP 未变化，跳过 CF API
    # 避免在 IP 不变时无谓地调用 Cloudflare（也避免 token/zone 异常时反复刷错误推送）
    local cache_file="$CACHE_DIR/$ID.ip"
    if [[ -f "$cache_file" ]]; then
        local cached_ip
        cached_ip=$(cat "$cache_file" 2>/dev/null)
        if [[ -n "$cached_ip" && "$cached_ip" == "$current_ip" ]]; then
            log "[$label] IP 无变化 ($RECORD_NAME = $current_ip)，命中缓存，跳过 Cloudflare API"
            tg_notify info "ℹ️ <b>DDNS 检查</b>
📍 <code>$RECORD_NAME</code>
IP 未变化: <code>$current_ip</code>"
            return 0
        fi
    fi

    # 按 RECORD_NAME 后缀逐级探测 zone，不再依赖配置里存的 ZONE_NAME。
    # 老配置兼容：ZONE_NAME 字段仍保留写入，只是运行时不读。
    local zone_info zone_id zone_name
    zone_info=$(auto_zone "$RECORD_NAME" "$API_TOKEN")
    if [[ -z "$zone_info" ]]; then
        log "[$label] 自动识别 zone 失败: $RECORD_NAME"
        tg_notify error "❌ <b>DDNS 错误</b>
📍 <code>$RECORD_NAME</code>
原因: 无法识别 zone (Token 对该域名没有 Zone:Read 权限?)"
        return 1
    fi
    read -r zone_id zone_name <<< "$zone_info"

    local rec_resp rec_id rec_ip
    rec_resp=$(cf_api GET "/zones/$zone_id/dns_records?name=$RECORD_NAME&type=$RECORD_TYPE" "$API_TOKEN")
    rec_id=$(echo "$rec_resp" | jq -r '.result[0].id // empty')
    rec_ip=$(echo "$rec_resp" | jq -r '.result[0].content // empty')

    local data
    data=$(jq -nc \
        --arg type "$RECORD_TYPE" \
        --arg name "$RECORD_NAME" \
        --arg content "$current_ip" \
        --argjson proxied "${PROXIED:-false}" \
        '{type:$type, name:$name, content:$content, ttl:1, proxied:$proxied}')

    if [[ -z "$rec_id" ]]; then
        local resp; resp=$(cf_api POST "/zones/$zone_id/dns_records" "$API_TOKEN" "$data")
        if [[ "$(echo "$resp" | jq -r '.success')" == "true" ]]; then
            echo "$current_ip" > "$cache_file"
            log "[$label] 创建 $RECORD_NAME -> $current_ip"
            tg_notify change "✅ <b>DDNS 已创建</b>
📍 <code>$RECORD_NAME</code>
🆕 IP: <code>$current_ip</code>"
        else
            log "[$label] 创建失败: $(echo "$resp" | jq -c '.errors')"
            tg_notify error "❌ <b>DDNS 创建失败</b>
📍 <code>$RECORD_NAME</code>
目标 IP: <code>$current_ip</code>"
            return 1
        fi
        return 0
    fi

    if [[ "$rec_ip" == "$current_ip" ]]; then
        echo "$current_ip" > "$cache_file"
        log "[$label] 无变化 ($RECORD_NAME = $current_ip)"
        tg_notify info "ℹ️ <b>DDNS 检查</b>
📍 <code>$RECORD_NAME</code>
IP 未变化: <code>$current_ip</code>"
        return 0
    fi

    local resp; resp=$(cf_api PUT "/zones/$zone_id/dns_records/$rec_id" "$API_TOKEN" "$data")
    if [[ "$(echo "$resp" | jq -r '.success')" == "true" ]]; then
        echo "$current_ip" > "$cache_file"
        log "[$label] 更新 $RECORD_NAME: $rec_ip -> $current_ip"
        tg_notify change "🔄 <b>DDNS 已更新</b>
📍 <code>$RECORD_NAME</code>
旧: <code>$rec_ip</code>
新: <code>$current_ip</code>"
    else
        log "[$label] 更新失败: $(echo "$resp" | jq -c '.errors')"
        tg_notify error "❌ <b>DDNS 更新失败</b>
📍 <code>$RECORD_NAME</code>
当前: <code>$rec_ip</code> → 期望: <code>$current_ip</code>"
        return 1
    fi
}

run_all() {
    shopt -s nullglob
    local files=("$RECORDS_DIR"/*.conf)
    shopt -u nullglob
    if [[ ${#files[@]} -eq 0 ]]; then
        log "无任何记录配置"
        return 0
    fi
    local rc=0
    for conf in "${files[@]}"; do
        ( run_one_record "$conf" ) || { log "[$(basename "$conf" .conf)] 任务失败"; rc=1; }
    done
    return $rc
}

# -------------------- 记录文件管理 --------------------
write_record() {
    local id="$1" name="$2" enabled="$3" token="$4" zone="$5" rec="$6" type="$7" proxied="$8"
    local file="$RECORDS_DIR/$id.conf"
    {
        echo "ID=$id"
        printf "NAME=%q\n" "$name"
        echo "ENABLED=$enabled"
        printf "API_TOKEN=%q\n" "$token"
        printf "ZONE_NAME=%q\n" "$zone"
        printf "RECORD_NAME=%q\n" "$rec"
        echo "RECORD_TYPE=$type"
        echo "PROXIED=$proxied"
    } > "$file"
    chmod 600 "$file"
    # 配置变更后清掉 IP 缓存，强制下次同步走一次完整 CF 流程
    rm -f "$CACHE_DIR/$id.ip"
}

list_records() {
    shopt -s nullglob
    local files=("$RECORDS_DIR"/*.conf)
    shopt -u nullglob
    if [[ ${#files[@]} -eq 0 ]]; then
        echo "  (尚未配置任何记录)"
        return
    fi
    local i=0
    for conf in "${files[@]}"; do
        i=$((i+1))
        (
            # shellcheck source=/dev/null
            source "$conf"
            local status proxy
            [[ "${ENABLED:-true}" == "true" ]] && status="${GREEN}启用${NC}" || status="${YELLOW}停用${NC}"
            [[ "${PROXIED:-false}" == "true" ]] && proxy="(proxied)" || proxy=""
            printf "  ${CYAN}%d)${NC} %-20s [%b] %s %s %s\n" \
                "$i" "${NAME}" "$status" "${RECORD_NAME}" "${RECORD_TYPE}" "$proxy"
        )
    done
}

select_record() {
    shopt -s nullglob
    local files=("$RECORDS_DIR"/*.conf)
    shopt -u nullglob
    [[ ${#files[@]} -eq 0 ]] && return 1
    list_records >&2
    local choice
    read -rp "请输入记录编号: " choice
    [[ "$choice" =~ ^[0-9]+$ ]] || return 1
    local idx=$((choice - 1))
    [[ $idx -lt 0 || $idx -ge ${#files[@]} ]] && return 1
    echo "${files[$idx]}"
}

prompt_new_record() {
    local token domain rec_type proxied name zone_info zone_id zone_name

    read -rp "Cloudflare API Token: " token
    if [[ -z "$token" ]]; then err "Token 不能为空"; return 1; fi

    info "校验 Token..."
    if ! verify_token "$token"; then
        err "Token 无效，请确认权限包含 Zone:Read 与 DNS:Edit"
        return 1
    fi
    ok "Token 有效"

    read -rp "完整域名 (例如 ddns.example.com): " domain
    if [[ -z "$domain" || "$domain" != *.* ]]; then err "域名格式不正确"; return 1; fi

    read -rp "记录类型 [A/AAAA] (默认 A): " rec_type
    rec_type="${rec_type:-A}"
    [[ "$rec_type" != "A" && "$rec_type" != "AAAA" ]] && rec_type="A"

    read -rp "是否走 Cloudflare 代理 (橙云)? [y/N]: " p
    [[ "$p" =~ ^[yY]$ ]] && proxied="true" || proxied="false"

    info "自动识别 zone..."
    zone_info=$(auto_zone "$domain" "$token")
    if [[ -z "$zone_info" ]]; then
        # auto_zone 失败再退回 find_zone（账号 zone 列表）做最后一次兜底
        zone_info=$(find_zone "$domain" "$token")
    fi
    if [[ -z "$zone_info" || "$zone_info" == "null null" ]]; then
        err "未在你的账号下找到 $domain 对应的 zone"
        warn "请确认 Token 对该 zone 有 Zone:Read 权限"
        return 1
    fi
    read -r zone_id zone_name <<< "$zone_info"
    ok "已识别 zone: $zone_name"

    read -rp "为这条记录起个备注名 [默认: $domain]: " name
    name="${name:-$domain}"

    local id; id=$(new_id)
    write_record "$id" "$name" "true" "$token" "$zone_name" "$domain" "$rec_type" "$proxied"
    ok "已保存记录: $name ($domain)"
    return 0
}

# ============================================================
#  服务管理 - 平台抽象层
# ============================================================
#  统一接口（不论 Linux/macOS 都用这些）：
#    svc_install        生成 / 写入服务文件
#    svc_enable         开机自启动（载入并设为自动启动）
#    svc_disable        关闭开机自启动
#    svc_start          启动（含立即触发一次）
#    svc_stop           停止
#    svc_uninstall      移除服务文件
#    svc_is_active      服务是否正在跑（exit code）
#    svc_is_enabled     是否设为开机自启动（exit code）
#    svc_status_text    给菜单显示用的彩色状态文字
#    svc_autostart_text 给菜单显示用的"开机自启"彩色文字
# ============================================================

# ---------- Linux: systemd ----------
linux_install() {
    local interval; interval=$(get_interval)

    if [[ "$SCRIPT_PATH" != "$INSTALL_PATH" ]]; then
        cp "$SCRIPT_PATH" "$INSTALL_PATH"
        chmod +x "$INSTALL_PATH"
    fi

    info "安装 systemd 服务（需要 sudo）..."
    $SUDO tee "$SYSTEMD_DIR/$SERVICE_NAME.service" >/dev/null <<EOF
[Unit]
Description=Cloudflare DDNS updater
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=$USER
Environment=HOME=$HOME
Environment=CF_DDNS_DIR=$CONFIG_DIR
ExecStart=/bin/bash $INSTALL_PATH --run
EOF

    $SUDO tee "$SYSTEMD_DIR/$SERVICE_NAME.timer" >/dev/null <<EOF
[Unit]
Description=Run cf-ddns every $interval minute(s)
Requires=$SERVICE_NAME.service

[Timer]
OnBootSec=1min
OnUnitActiveSec=${interval}min
AccuracySec=30s
Unit=$SERVICE_NAME.service

[Install]
WantedBy=timers.target
EOF
    $SUDO systemctl daemon-reload
}

linux_enable()    { linux_install; $SUDO systemctl enable --now "$SERVICE_NAME.timer"; }
linux_disable()   { $SUDO systemctl disable --now "$SERVICE_NAME.timer" 2>/dev/null || true; }
linux_start()     { [[ -f "$SYSTEMD_DIR/$SERVICE_NAME.timer" ]] || linux_install
                    $SUDO systemctl start "$SERVICE_NAME.timer"; }
linux_stop()      { $SUDO systemctl stop "$SERVICE_NAME.timer" 2>/dev/null || true; }
linux_uninstall() { $SUDO systemctl disable --now "$SERVICE_NAME.timer" 2>/dev/null || true
                    $SUDO rm -f "$SYSTEMD_DIR/$SERVICE_NAME.service" "$SYSTEMD_DIR/$SERVICE_NAME.timer"
                    $SUDO systemctl daemon-reload; }
linux_is_active() { systemctl is-active --quiet "$SERVICE_NAME.timer" 2>/dev/null; }
linux_is_enabled(){ systemctl is-enabled --quiet "$SERVICE_NAME.timer" 2>/dev/null; }
linux_status_text() {
    if linux_is_active; then
        echo -e "${GREEN}运行中${NC}"
    elif [[ -f "$SYSTEMD_DIR/$SERVICE_NAME.timer" ]]; then
        if linux_is_enabled; then
            echo -e "${YELLOW}已安装但未运行${NC}"
        else
            echo -e "${YELLOW}已安装未启用${NC}"
        fi
    else
        echo -e "${RED}未安装${NC}"
    fi
}
linux_autostart_text() {
    if linux_is_enabled; then echo -e "${GREEN}已启用${NC}"
    else echo -e "${RED}未启用${NC}"; fi
}

# ---------- macOS: launchd ----------
macos_install() {
    local interval; interval=$(get_interval)
    local interval_sec=$(( interval * 60 ))

    if [[ "$SCRIPT_PATH" != "$INSTALL_PATH" ]]; then
        cp "$SCRIPT_PATH" "$INSTALL_PATH"
        chmod +x "$INSTALL_PATH"
    fi

    info "生成 LaunchAgent: $LAUNCHD_PLIST"

    # 如果已加载，先卸载，否则更新不生效
    if macos_is_loaded; then
        launchctl unload "$LAUNCHD_PLIST" 2>/dev/null || true
    fi

    # launchd 跑起来时 PATH 非常窄，必须把 Homebrew 路径写进去
    # 同时支持 Intel (/usr/local) 和 Apple Silicon (/opt/homebrew)
    cat > "$LAUNCHD_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>$LAUNCHD_LABEL</string>

    <key>ProgramArguments</key>
    <array>
        <string>/bin/bash</string>
        <string>$INSTALL_PATH</string>
        <string>--run</string>
    </array>

    <key>StartInterval</key>
    <integer>$interval_sec</integer>

    <key>RunAtLoad</key>
    <true/>

    <key>WorkingDirectory</key>
    <string>$HOME</string>

    <key>StandardOutPath</key>
    <string>$CONFIG_DIR/launchd.out.log</string>

    <key>StandardErrorPath</key>
    <string>$CONFIG_DIR/launchd.err.log</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>HOME</key>
        <string>$HOME</string>
        <key>CF_DDNS_DIR</key>
        <string>$CONFIG_DIR</string>
        <key>PATH</key>
        <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
    </dict>
</dict>
</plist>
EOF
    chmod 644 "$LAUNCHD_PLIST"
}

macos_is_loaded() {
    launchctl list 2>/dev/null | awk '{print $3}' | grep -qx "$LAUNCHD_LABEL"
}
macos_is_enabled() {
    # 在 macOS 上 "开机自启" 的定义就是：plist 已被 load
    # 因为 LaunchAgent 一旦 load -w，就会在用户登录时自动 load
    macos_is_loaded
}
macos_is_active() {
    # launchd 是 periodic 触发，没法判断"现在是不是正在跑"
    # 这里把"已 load 且 plist 存在" 视为运行中（和 systemd timer 等价）
    [[ -f "$LAUNCHD_PLIST" ]] && macos_is_loaded
}
macos_load()   { launchctl load -w "$LAUNCHD_PLIST" 2>/dev/null || true; }
macos_unload() { launchctl unload -w "$LAUNCHD_PLIST" 2>/dev/null || true; }

macos_enable() { macos_install; macos_load; }
macos_disable(){ macos_unload; }
macos_start()  {
    [[ -f "$LAUNCHD_PLIST" ]] || macos_install
    macos_is_loaded || macos_load
    # 立即触发一次（不影响后续 schedule）
    launchctl start "$LAUNCHD_LABEL" 2>/dev/null || true
}
macos_stop()   { macos_unload; }
macos_uninstall() {
    macos_unload
    rm -f "$LAUNCHD_PLIST"
}
macos_status_text() {
    if macos_is_active; then
        echo -e "${GREEN}运行中${NC}"
    elif [[ -f "$LAUNCHD_PLIST" ]]; then
        echo -e "${YELLOW}已安装但未启用${NC}"
    else
        echo -e "${RED}未安装${NC}"
    fi
}
macos_autostart_text() {
    if macos_is_enabled; then echo -e "${GREEN}已启用${NC}"
    else echo -e "${RED}未启用${NC}"; fi
}

# ---------- 统一调度 ----------
svc_install()        { case "$OS_TYPE" in linux) linux_install ;;        macos) macos_install ;;        esac; }
svc_enable()         { case "$OS_TYPE" in linux) linux_enable ;;         macos) macos_enable ;;         esac; }
svc_disable()        { case "$OS_TYPE" in linux) linux_disable ;;        macos) macos_disable ;;        esac; }
svc_start()          { case "$OS_TYPE" in linux) linux_start ;;          macos) macos_start ;;          esac; }
svc_stop()           { case "$OS_TYPE" in linux) linux_stop ;;           macos) macos_stop ;;           esac; }
svc_uninstall()      { case "$OS_TYPE" in linux) linux_uninstall ;;      macos) macos_uninstall ;;      esac; }
svc_is_active()      { case "$OS_TYPE" in linux) linux_is_active ;;      macos) macos_is_active ;;      esac; }
svc_is_enabled()     { case "$OS_TYPE" in linux) linux_is_enabled ;;     macos) macos_is_enabled ;;     esac; }
svc_status_text()    { case "$OS_TYPE" in linux) linux_status_text ;;    macos) macos_status_text ;;    esac; }
svc_autostart_text() { case "$OS_TYPE" in linux) linux_autostart_text ;; macos) macos_autostart_text ;; esac; }

# 旧名称的兼容包装（保持代码可读性）
enable_autostart()  { svc_enable;  ok "已开启开机自启动，每 $(get_interval) 分钟执行一次"; }
disable_autostart() { svc_disable; ok "已关闭开机自启动"; }
start_service()     { svc_start;   ok "服务已启动"; }
pause_service()     { svc_stop;    ok "服务已暂停"; }

# 间隔变更时需要重启服务
restart_if_running() {
    if [[ "$OS_TYPE" == "linux" ]]; then
        [[ -f "$SYSTEMD_DIR/$SERVICE_NAME.timer" ]] || return 0
        linux_install
        $SUDO systemctl restart "$SERVICE_NAME.timer" 2>/dev/null || true
    else
        [[ -f "$LAUNCHD_PLIST" ]] || return 0
        macos_install
        macos_load
    fi
}

# -------------------- 菜单动作 --------------------
action_add()    { prompt_new_record || true; }

action_edit() {
    local conf; conf=$(select_record) || { warn "无可编辑记录"; return; }
    # shellcheck source=/dev/null
    source "$conf"
    info "直接回车保留原值"
    local nname ntoken nrec ntype np
    read -rp "备注名 [$NAME]: " nname; nname="${nname:-$NAME}"
    read -rp "API Token [${API_TOKEN:0:6}...]: " ntoken; ntoken="${ntoken:-$API_TOKEN}"
    read -rp "完整域名 [$RECORD_NAME]: " nrec; nrec="${nrec:-$RECORD_NAME}"
    read -rp "类型 [$RECORD_TYPE]: " ntype; ntype="${ntype:-$RECORD_TYPE}"
    read -rp "Proxied [$PROXIED]: " np; np="${np:-$PROXIED}"

    # zone 由域名自动识别；失败则保留原 ZONE_NAME（不阻塞编辑）
    local nzone="$ZONE_NAME" zinfo
    if zinfo=$(auto_zone "$nrec" "$ntoken") && [[ -n "$zinfo" ]]; then
        read -r _ nzone <<< "$zinfo"
        ok "已识别 zone: $nzone"
    else
        warn "未能自动识别 zone，保留原值: $ZONE_NAME"
    fi

    write_record "$ID" "$nname" "${ENABLED:-true}" "$ntoken" "$nzone" "$nrec" "$ntype" "$np"
    ok "已更新"
}

action_delete() {
    local conf; conf=$(select_record) || { warn "无可删除记录"; return; }
    # shellcheck source=/dev/null
    source "$conf"
    read -rp "确认删除 '$NAME' ($RECORD_NAME)? [y/N]: " yn
    [[ "$yn" =~ ^[yY]$ ]] && { rm -f "$conf" "$CACHE_DIR/$ID.ip"; ok "已删除"; } || info "已取消"
}

action_toggle() {
    local conf; conf=$(select_record) || { warn "没有记录"; return; }
    # shellcheck source=/dev/null
    source "$conf"
    local new
    [[ "${ENABLED:-true}" == "true" ]] && new="false" || new="true"
    write_record "$ID" "$NAME" "$new" "$API_TOKEN" "$ZONE_NAME" "$RECORD_NAME" "$RECORD_TYPE" "$PROXIED"
    ok "ENABLED=$new"
}

action_set_interval() {
    local cur new; cur=$(get_interval)
    read -rp "检查间隔（分钟）[$cur]: " new
    new="${new:-$cur}"
    if ! [[ "$new" =~ ^[0-9]+$ ]] || [[ "$new" -lt 1 ]]; then err "无效"; return; fi
    set_interval "$new"
    restart_if_running
    ok "间隔已设为 $new 分钟"
}

action_manual_check() {
    info "立即执行一次同步..."
    run_all
    ok "完成。最近日志："
    tail -n 20 "$LOG_FILE" 2>/dev/null || true
}

action_configure_telegram() {
    info "配置 Telegram 通知"
    cat <<'EOF'

如何获取 Bot Token 和 Chat ID:
  1) 在 Telegram 中找 @BotFather，发 /newbot 创建一个 Bot，得到 Bot Token
  2) 给你刚创建的 Bot 发任意一条消息（必须先发，不然取不到 chat_id）
  3) 找 @userinfobot 或 @getidsbot 获取你的 Chat ID
     （群组通知则把 Bot 拉进群，Chat ID 形如 -100xxxxxxx）

EOF
    load_config
    local token chat
    if [[ -n "$TG_BOT_TOKEN" ]]; then
        read -rp "Bot Token [当前: ${TG_BOT_TOKEN:0:10}...，回车保持]: " token
        token="${token:-$TG_BOT_TOKEN}"
    else
        read -rp "Bot Token: " token
    fi
    if [[ -n "$TG_CHAT_ID" ]]; then
        read -rp "Chat ID [当前: $TG_CHAT_ID，回车保持]: " chat
        chat="${chat:-$TG_CHAT_ID}"
    else
        read -rp "Chat ID: " chat
    fi

    cat <<EOF

通知级别：
  1) all     - 所有事件（含每次无变化的检查，会很吵）
  2) changes - IP 变更 + 错误（推荐）
  3) errors  - 仅错误
  4) off     - 关闭
EOF
    read -rp "选择 [当前: ${TG_NOTIFY_LEVEL}]: " l
    local level
    case "$l" in
        1) level="all" ;;
        2) level="changes" ;;
        3) level="errors" ;;
        4) level="off" ;;
        "") level="$TG_NOTIFY_LEVEL" ;;
        *)  warn "无效选项，保持 $TG_NOTIFY_LEVEL"; level="$TG_NOTIFY_LEVEL" ;;
    esac

    TG_BOT_TOKEN="$token"
    TG_CHAT_ID="$chat"
    TG_NOTIFY_LEVEL="$level"
    save_config
    ok "Telegram 配置已保存（级别: $level）"

    if [[ -n "$token" && -n "$chat" && "$level" != "off" ]]; then
        read -rp "立即发送一条测试消息确认是否生效？[Y/n]: " yn
        [[ "$yn" =~ ^[nN]$ ]] || action_test_telegram
    fi
}

action_test_telegram() {
    load_config
    if [[ -z "$TG_BOT_TOKEN" || -z "$TG_CHAT_ID" ]]; then
        err "尚未配置 Bot Token 或 Chat ID"
        return
    fi
    info "正在发送测试消息..."
    local resp
    resp=$(curl -sS --max-time 10 \
        -X POST "https://api.telegram.org/bot${TG_BOT_TOKEN}/sendMessage" \
        -d "chat_id=${TG_CHAT_ID}" \
        --data-urlencode "text=🧪 <b>cf-ddns 测试通知</b>
来自: <code>$(hostname 2>/dev/null || echo server)</code> ($OS_TYPE)
时间: $(date '+%F %T')
通知级别: <code>${TG_NOTIFY_LEVEL}</code>" \
        -d "parse_mode=HTML" 2>&1) || true
    if [[ "$(echo "$resp" | jq -r '.ok' 2>/dev/null)" == "true" ]]; then
        ok "测试消息发送成功，请到 Telegram 查看"
    else
        err "发送失败：$(echo "$resp" | jq -r '.description // empty' 2>/dev/null || echo "$resp")"
        warn "请检查 Bot Token 是否正确，以及你是否已先给 Bot 发过消息"
    fi
}

action_show_log() {
    [[ -f "$LOG_FILE" ]] && tail -n 50 "$LOG_FILE" || info "暂无日志"
}

action_uninstall() {
    warn "这会删除所有配置和服务文件"
    read -rp "确定卸载？[y/N]: " yn
    if [[ "$yn" =~ ^[yY]$ ]]; then
        svc_uninstall
        rm -rf "$CONFIG_DIR"
        ok "卸载完成"
        exit 0
    else
        info "已取消"
    fi
}

# -------------------- 首次运行向导 --------------------
first_run() {
    cat <<EOF

============================================
  Cloudflare DDNS 一键脚本  v$VERSION
  平台: $OS_TYPE
============================================

第一次运行，请按提示完成配置：

EOF
    info "[ 1/3 ] 配置 Cloudflare API Token 与域名"
    until prompt_new_record; do
        warn "重新输入"
    done
    echo

    info "[ 2/3 ] 配置检查间隔"
    local mins
    read -rp "几分钟同步一次? (默认 5): " mins
    mins="${mins:-5}"
    if ! [[ "$mins" =~ ^[0-9]+$ ]] || [[ "$mins" -lt 1 ]]; then
        warn "输入无效，使用默认 5 分钟"
        mins=5
    fi
    set_interval "$mins"
    ok "间隔: $mins 分钟"
    echo

    info "[ 3/3 ] 开启开机自启动"
    enable_autostart
    echo

    info "立即执行第一次同步..."
    run_all
    echo

    read -rp "是否要配置 Telegram 通知（IP 变更时收到推送）？[y/N]: " want_tg
    if [[ "$want_tg" =~ ^[yY]$ ]]; then
        action_configure_telegram
    fi
    echo
    ok "全部完成！再次运行此脚本可进入管理菜单"
    echo
    info "脚本已安装到: $INSTALL_PATH"
    info "日志位置:     $LOG_FILE"
    info "配置目录:     $CONFIG_DIR"
    if [[ "$OS_TYPE" == "macos" ]]; then
        info "LaunchAgent:  $LAUNCHD_PLIST"
    fi
}

# -------------------- 菜单 --------------------
show_menu() {
    while true; do
        clear
        load_config
        local tg_status
        if [[ -n "$TG_BOT_TOKEN" && -n "$TG_CHAT_ID" && "$TG_NOTIFY_LEVEL" != "off" ]]; then
            tg_status="${GREEN}${TG_NOTIFY_LEVEL}${NC}"
        elif [[ -n "$TG_BOT_TOKEN" && "$TG_NOTIFY_LEVEL" == "off" ]]; then
            tg_status="${YELLOW}已配置但关闭${NC}"
        else
            tg_status="${RED}未配置${NC}"
        fi

        cat <<EOF
============================================
  Cloudflare DDNS 管理器  v$VERSION  [$OS_TYPE]
============================================
EOF
        printf "  服务状态: %b   开机自启: %b   间隔: %s 分钟\n" \
            "$(svc_status_text)" "$(svc_autostart_text)" "$(get_interval)"
        printf "  Telegram: %b\n" "$tg_status"
        echo "--------------------------------------------"
        echo "  当前记录："
        list_records
        echo "--------------------------------------------"
        cat <<'EOF'
  [记录管理]
    1) 新增记录
    2) 编辑记录
    3) 删除记录
    4) 启用/停用记录

  [运行控制]
    5) 立即手动检测
    6) 设置检查间隔
    7) 启动服务
    8) 暂停服务
    9) 开启开机自启动
   10) 关闭开机自启动

  [Telegram 通知]
   11) 配置 Telegram
   12) 发送测试消息

  [其他]
   13) 查看日志
   14) 完全卸载
    0) 退出
EOF
        echo
        read -rp "请选择: " c
        case "$c" in
            1)  action_add ;;
            2)  action_edit ;;
            3)  action_delete ;;
            4)  action_toggle ;;
            5)  action_manual_check ;;
            6)  action_set_interval ;;
            7)  start_service ;;
            8)  pause_service ;;
            9)  enable_autostart ;;
            10) disable_autostart ;;
            11) action_configure_telegram ;;
            12) action_test_telegram ;;
            13) action_show_log ;;
            14) action_uninstall ;;
            0)  exit 0 ;;
            *)  warn "无效选项" ;;
        esac
        echo
        read -rp "按回车继续..." _
    done
}

# -------------------- 入口 --------------------
print_help() {
    cat <<EOF
cf-ddns.sh v$VERSION  -  Cloudflare DDNS 一键脚本 (Linux + macOS)

用法:
  $0                  首次运行向导 / 管理菜单
  $0 --run            执行一次同步（systemd / launchd 用）
  $0 --status         查看状态
  $0 --check          立即同步（同菜单中的手动检测）
  $0 --version
  $0 --help

平台: 自动检测 (linux=systemd / macos=launchd)
配置目录: $CONFIG_DIR
EOF
}

main() {
    detect_os
    case "${1:-}" in
        --run)
            check_deps; ensure_dirs
            run_all; exit $?
            ;;
        --check)
            check_deps; ensure_dirs
            action_manual_check; exit 0
            ;;
        --status)
            ensure_dirs
            echo "平台: $OS_TYPE"
            echo "服务: $(svc_status_text)   自启: $(svc_autostart_text)   间隔: $(get_interval) min"
            list_records
            exit 0
            ;;
        --version|-v) echo "cf-ddns $VERSION"; exit 0 ;;
        --help|-h)    print_help; exit 0 ;;
        "") ;;
        *) err "未知参数: $1"; print_help; exit 1 ;;
    esac

    check_deps
    ensure_dirs

    shopt -s nullglob
    local files=("$RECORDS_DIR"/*.conf)
    shopt -u nullglob

    if [[ ! -f "$GLOBAL_CONF" ]] && [[ ${#files[@]} -eq 0 ]]; then
        first_run
    else
        show_menu
    fi
}

main "$@"
