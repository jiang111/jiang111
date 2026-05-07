#!/usr/bin/env bash
# ============================================================
#  cf-ddns.sh  —  Cloudflare DDNS 一键管理脚本
# ============================================================
#  功能：
#    - 配置 Cloudflare API Token + 域名，自动建立/更新 DNS 记录
#    - 多条记录管理：新增 / 编辑 / 删除 / 启用 / 停用
#    - 自定义检查间隔（分钟）
#    - 手动立即检查
#    - 开机自启动 / 暂停 / 启动（基于 systemd timer）
#    - 完全卸载
#
#  依赖：bash, curl, jq, systemd
#  运行：./cf-ddns.sh   （首次会引导配置，之后显示菜单）
#       ./cf-ddns.sh --run   （供 systemd 调用）
# ============================================================

set -uo pipefail

VERSION="1.0.0"
CONFIG_DIR="${CF_DDNS_DIR:-$HOME/.cf-ddns}"
RECORDS_DIR="$CONFIG_DIR/records"
GLOBAL_CONF="$CONFIG_DIR/config"
LOG_FILE="$CONFIG_DIR/ddns.log"
INSTALL_PATH="$CONFIG_DIR/cf-ddns.sh"
SERVICE_NAME="cf-ddns"
SYSTEMD_DIR="/etc/systemd/system"
SCRIPT_PATH="$(readlink -f "$0" 2>/dev/null || echo "$0")"

SUDO=""
[[ $EUID -ne 0 ]] && SUDO="sudo"

# -------------------- helpers --------------------
RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'
BLUE='\033[0;34m'; CYAN='\033[0;36m'; NC='\033[0m'

log()  { echo "[$(date '+%F %T')] $*" >> "$LOG_FILE"; }
info() { echo -e "${BLUE}[*]${NC} $*"; }
ok()   { echo -e "${GREEN}[✓]${NC} $*"; }
warn() { echo -e "${YELLOW}[!]${NC} $*"; }
err()  { echo -e "${RED}[✗]${NC} $*" >&2; }

check_deps() {
    local missing=()
    for c in curl jq; do command -v "$c" &>/dev/null || missing+=("$c"); done
    if [[ ${#missing[@]} -gt 0 ]]; then
        err "缺少依赖: ${missing[*]}"
        info "Debian/Ubuntu:  sudo apt install -y ${missing[*]}"
        info "RHEL/CentOS:    sudo yum install -y ${missing[*]}"
        info "Arch:           sudo pacman -S ${missing[*]}"
        exit 1
    fi
    if ! command -v systemctl &>/dev/null; then
        err "未检测到 systemd，本脚本仅支持 systemd 系统"
        exit 1
    fi
}

ensure_dirs() {
    mkdir -p "$CONFIG_DIR" "$RECORDS_DIR"
    chmod 700 "$CONFIG_DIR" "$RECORDS_DIR"
    [[ -f "$LOG_FILE" ]] || touch "$LOG_FILE"
}

new_id() { date +%s%N | sha256sum | head -c 8; }

# -------------------- 全局配置 --------------------
get_interval() {
    if [[ -f "$GLOBAL_CONF" ]]; then
        # shellcheck source=/dev/null
        source "$GLOBAL_CONF"
        echo "${INTERVAL:-5}"
    else
        echo "5"
    fi
}

set_interval() {
    cat > "$GLOBAL_CONF" <<EOF
INTERVAL=$1
EOF
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

# 根据完整域名找出对应的 zone (取所有 zones 中最长后缀匹配)
find_zone() {
    local record="$1" token="$2"
    cf_api GET "/zones?per_page=50" "$token" | jq -r \
        --arg rec "$record" '
        .result
        | map(select($rec == .name or ($rec | endswith("." + .name))))
        | sort_by(.name | length) | reverse | .[0]
        | "\(.id) \(.name)"' 2>/dev/null
}

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
        return 1
    fi

    local zone_resp zone_id
    zone_resp=$(cf_api GET "/zones?name=$ZONE_NAME" "$API_TOKEN")
    zone_id=$(echo "$zone_resp" | jq -r '.result[0].id // empty')
    if [[ -z "$zone_id" ]]; then
        log "[$label] 找不到 zone $ZONE_NAME"
        return 1
    fi

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
            log "[$label] 创建 $RECORD_NAME -> $current_ip"
        else
            log "[$label] 创建失败: $(echo "$resp" | jq -c '.errors')"
            return 1
        fi
        return 0
    fi

    if [[ "$rec_ip" == "$current_ip" ]]; then
        log "[$label] 无变化 ($RECORD_NAME = $current_ip)"
        return 0
    fi

    local resp; resp=$(cf_api PUT "/zones/$zone_id/dns_records/$rec_id" "$API_TOKEN" "$data")
    if [[ "$(echo "$resp" | jq -r '.success')" == "true" ]]; then
        log "[$label] 更新 $RECORD_NAME: $rec_ip -> $current_ip"
    else
        log "[$label] 更新失败: $(echo "$resp" | jq -c '.errors')"
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

# 交互式输入新记录（支持自动检测 zone）
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

    info "自动检测 zone..."
    zone_info=$(find_zone "$domain" "$token")
    if [[ -z "$zone_info" || "$zone_info" == "null null" ]]; then
        err "未在你的账号下找到 $domain 对应的 zone"
        warn "你也可以手动输入 zone 名称"
        read -rp "Zone 名称 (根域名, 例如 example.com): " zone_name
        [[ -z "$zone_name" ]] && return 1
    else
        read -r zone_id zone_name <<< "$zone_info"
        ok "找到 zone: $zone_name"
    fi

    read -rp "为这条记录起个备注名 [默认: $domain]: " name
    name="${name:-$domain}"

    local id; id=$(new_id)
    write_record "$id" "$name" "true" "$token" "$zone_name" "$domain" "$rec_type" "$proxied"
    ok "已保存记录: $name ($domain)"
    return 0
}

# -------------------- systemd 管理 --------------------
service_status() {
    if systemctl is-active --quiet "$SERVICE_NAME.timer" 2>/dev/null; then
        echo -e "${GREEN}运行中${NC}"
    elif [[ -f "$SYSTEMD_DIR/$SERVICE_NAME.timer" ]]; then
        if systemctl is-enabled --quiet "$SERVICE_NAME.timer" 2>/dev/null; then
            echo -e "${YELLOW}已安装但未运行${NC}"
        else
            echo -e "${YELLOW}已安装未启用${NC}"
        fi
    else
        echo -e "${RED}未安装${NC}"
    fi
}

autostart_status() {
    if systemctl is-enabled --quiet "$SERVICE_NAME.timer" 2>/dev/null; then
        echo -e "${GREEN}已启用${NC}"
    else
        echo -e "${RED}未启用${NC}"
    fi
}

install_systemd() {
    local interval; interval=$(get_interval)

    # 把脚本拷到稳定路径，避免源文件移动后服务失效
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

enable_autostart() {
    install_systemd
    $SUDO systemctl enable --now "$SERVICE_NAME.timer"
    ok "已开启开机自启动，每 $(get_interval) 分钟执行一次"
}

disable_autostart() {
    $SUDO systemctl disable --now "$SERVICE_NAME.timer" 2>/dev/null || true
    ok "已关闭开机自启动"
}

start_service() {
    [[ -f "$SYSTEMD_DIR/$SERVICE_NAME.timer" ]] || install_systemd
    $SUDO systemctl start "$SERVICE_NAME.timer"
    ok "服务已启动"
}

pause_service() {
    $SUDO systemctl stop "$SERVICE_NAME.timer" 2>/dev/null || true
    ok "服务已暂停"
}

uninstall_systemd() {
    $SUDO systemctl disable --now "$SERVICE_NAME.timer" 2>/dev/null || true
    $SUDO rm -f "$SYSTEMD_DIR/$SERVICE_NAME.service" "$SYSTEMD_DIR/$SERVICE_NAME.timer"
    $SUDO systemctl daemon-reload
}

# -------------------- 菜单动作 --------------------
action_add()    { prompt_new_record || true; }

action_edit() {
    local conf; conf=$(select_record) || { warn "无可编辑记录"; return; }
    # shellcheck source=/dev/null
    source "$conf"
    info "直接回车保留原值"
    local nname ntoken nzone nrec ntype np
    read -rp "备注名 [$NAME]: " nname; nname="${nname:-$NAME}"
    read -rp "API Token [${API_TOKEN:0:6}...]: " ntoken; ntoken="${ntoken:-$API_TOKEN}"
    read -rp "Zone [$ZONE_NAME]: " nzone; nzone="${nzone:-$ZONE_NAME}"
    read -rp "完整域名 [$RECORD_NAME]: " nrec; nrec="${nrec:-$RECORD_NAME}"
    read -rp "类型 [$RECORD_TYPE]: " ntype; ntype="${ntype:-$RECORD_TYPE}"
    read -rp "Proxied [$PROXIED]: " np; np="${np:-$PROXIED}"
    write_record "$ID" "$nname" "${ENABLED:-true}" "$ntoken" "$nzone" "$nrec" "$ntype" "$np"
    ok "已更新"
}

action_delete() {
    local conf; conf=$(select_record) || { warn "无可删除记录"; return; }
    # shellcheck source=/dev/null
    source "$conf"
    read -rp "确认删除 '$NAME' ($RECORD_NAME)? [y/N]: " yn
    [[ "$yn" =~ ^[yY]$ ]] && { rm -f "$conf"; ok "已删除"; } || info "已取消"
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
    if [[ -f "$SYSTEMD_DIR/$SERVICE_NAME.timer" ]]; then
        install_systemd
        $SUDO systemctl restart "$SERVICE_NAME.timer" 2>/dev/null || true
    fi
    ok "间隔已设为 $new 分钟"
}

action_manual_check() {
    info "立即执行一次同步..."
    run_all
    ok "完成。最近日志："
    tail -n 20 "$LOG_FILE" 2>/dev/null || true
}

action_show_log() {
    [[ -f "$LOG_FILE" ]] && tail -n 50 "$LOG_FILE" || info "暂无日志"
}

action_uninstall() {
    warn "这会删除所有配置和 systemd 服务"
    read -rp "确定卸载？[y/N]: " yn
    if [[ "$yn" =~ ^[yY]$ ]]; then
        uninstall_systemd
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
    ok "全部完成！再次运行此脚本可进入管理菜单"
    echo
    info "脚本已安装到: $INSTALL_PATH"
    info "日志位置:     $LOG_FILE"
    info "配置目录:     $CONFIG_DIR"
}

# -------------------- 菜单 --------------------
show_menu() {
    while true; do
        clear
        cat <<EOF
============================================
  Cloudflare DDNS 管理器  v$VERSION
============================================
EOF
        printf "  服务状态: %b   开机自启: %b   间隔: %s 分钟\n" \
            "$(service_status)" "$(autostart_status)" "$(get_interval)"
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

  [其他]
   11) 查看日志
   12) 完全卸载
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
            11) action_show_log ;;
            12) action_uninstall ;;
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
cf-ddns.sh v$VERSION  -  Cloudflare DDNS 一键脚本

用法:
  $0                  首次运行向导 / 管理菜单
  $0 --run            执行一次同步（systemd 用）
  $0 --status         查看状态
  $0 --check          立即同步（同菜单中的手动检测）
  $0 --version
  $0 --help

配置目录: $CONFIG_DIR
EOF
}

main() {
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
            echo "服务: $(service_status)   自启: $(autostart_status)   间隔: $(get_interval) min"
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
