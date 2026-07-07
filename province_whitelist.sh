#!/usr/bin/env bash
#=================================================================#
#   省份 IP 白名单一键脚本 (ipset + iptables)
#
#   功能: 只允许指定省份的 IP 访问服务器指定端口(或全部端口),
#         其余 IP 一律丢弃, 用于减少扫描和攻击。
#
#   数据源: https://github.com/metowolf/iplist (每小时自动更新)
#           按 GB/T 2260 省级行政区划代码提供 CIDR 列表
#
#   用法:
#     bash province_whitelist.sh            # 打开交互式管理菜单
#     bash province_whitelist.sh install    # 直接进入安装向导
#     bash province_whitelist.sh update     # 更新省份 IP 数据(可放 cron)
#     bash province_whitelist.sh status     # 查看当前状态
#     bash province_whitelist.sh add-province [省份名|代码]...  # 新增白名单省份
#     bash province_whitelist.sh del-province [省份名|代码]...  # 移除白名单省份
#     bash province_whitelist.sh add-ip <IP/CIDR>   # 添加额外白名单 IP
#     bash province_whitelist.sh del-ip <IP/CIDR>   # 删除额外白名单 IP
#     bash province_whitelist.sh auto-update on|off # 开启/关闭每日自动同步
#     bash province_whitelist.sh pause      # 暂停白名单(保留配置, 不再拦截)
#     bash province_whitelist.sh resume     # 恢复白名单
#     bash province_whitelist.sh uninstall  # 卸载并恢复原样
#=================================================================#

set -u

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; PLAIN='\033[0m'

CONF_DIR="/etc/province-whitelist"
CONF_FILE="${CONF_DIR}/config"
IPSET_MAIN="province_wl"        # 省份 IP 集合
IPSET_EXTRA="province_wl_extra" # 额外手动白名单集合
CHAIN="PROVINCE_WL"             # iptables 自定义链
SERVICE_FILE="/etc/systemd/system/province-whitelist.service"
CRON_FILE="/etc/cron.d/province-whitelist"
SCRIPT_PATH="/usr/local/bin/province_whitelist.sh"

# 数据源镜像, 按顺序尝试 (前两个国内一般可直连)
MIRRORS=(
    "https://cdn.jsdelivr.net/gh/metowolf/iplist/data/cncity"
    "https://fastly.jsdelivr.net/gh/metowolf/iplist/data/cncity"
    "https://raw.githubusercontent.com/metowolf/iplist/master/data/cncity"
    "https://metowolf.github.io/iplist/data/cncity"
)

# GB/T 2260 省级行政区划代码
PROVINCE_CODES=(110000 120000 130000 140000 150000 210000 220000 230000 310000 320000 330000 340000 350000 360000 370000 410000 420000 430000 440000 450000 460000 500000 510000 520000 530000 540000 610000 620000 630000 640000 650000)
PROVINCE_NAMES=("北京" "天津" "河北" "山西" "内蒙古" "辽宁" "吉林" "黑龙江" "上海" "江苏" "浙江" "安徽" "福建" "江西" "山东" "河南" "湖北" "湖南" "广东" "广西" "海南" "重庆" "四川" "贵州" "云南" "西藏" "陕西" "甘肃" "青海" "宁夏" "新疆")

err()  { echo -e "${RED}[错误]${PLAIN} $*" >&2; }
warn() { echo -e "${YELLOW}[警告]${PLAIN} $*"; }
info() { echo -e "${GREEN}[信息]${PLAIN} $*"; }

require_root() {
    [[ $EUID -eq 0 ]] || { err "请使用 root 运行此脚本"; exit 1; }
}

install_deps() {
    local need=()
    command -v ipset >/dev/null 2>&1 || need+=(ipset)
    command -v iptables >/dev/null 2>&1 || need+=(iptables)
    command -v curl >/dev/null 2>&1 || need+=(curl)
    [[ ${#need[@]} -eq 0 ]] && return 0
    info "安装依赖: ${need[*]}"
    if command -v apt-get >/dev/null 2>&1; then
        apt-get update -qq && apt-get install -y -qq "${need[@]}"
    elif command -v dnf >/dev/null 2>&1; then
        dnf install -y "${need[@]}"
    elif command -v yum >/dev/null 2>&1; then
        yum install -y "${need[@]}"
    else
        err "未识别的包管理器, 请手动安装: ${need[*]}"; exit 1
    fi
}

# 从镜像下载单个省份的 CIDR 列表到指定文件, 成功返回 0
fetch_province() {
    local code=$1 out=$2 m
    for m in "${MIRRORS[@]}"; do
        if curl -fsSL --max-time 60 "${m}/${code}.txt" -o "${out}.tmp" 2>/dev/null; then
            # 校验内容确实是 CIDR 列表
            if grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+' "${out}.tmp"; then
                mv "${out}.tmp" "${out}"
                return 0
            fi
        fi
    done
    rm -f "${out}.tmp"
    return 1
}

# 下载所选省份数据并原子更新 ipset (先建临时集合再 swap, 更新过程不断流)
build_ipset() {
    local codes=("$@")
    local tmpdir tmpset="${IPSET_MAIN}_tmp" total=0 code name i

    tmpdir=$(mktemp -d)

    for code in "${codes[@]}"; do
        name=$code
        for i in "${!PROVINCE_CODES[@]}"; do
            [[ ${PROVINCE_CODES[$i]} == "$code" ]] && name=${PROVINCE_NAMES[$i]}
        done
        info "下载 ${name}(${code}) IP 段..."
        if ! fetch_province "$code" "${tmpdir}/${code}.txt"; then
            err "下载 ${name}(${code}) 失败, 所有镜像均不可用, 本次不更新"
            rm -rf "$tmpdir"
            return 1
        fi
    done

    ipset destroy "$tmpset" 2>/dev/null
    ipset create "$tmpset" hash:net family inet hashsize 4096 maxelem 262144

    {
        for code in "${codes[@]}"; do
            grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+' "${tmpdir}/${code}.txt" \
                | sed "s/^/add ${tmpset} /"
        done
    } | ipset restore -!

    rm -rf "$tmpdir"

    total=$(ipset list "$tmpset" -terse | awk '/Number of entries/{print $4}')
    if [[ -z "$total" || "$total" -lt 10 ]]; then
        err "IP 段数量异常(${total:-0}), 放弃本次更新"
        ipset destroy "$tmpset" 2>/dev/null
        return 1
    fi

    if ipset list -name | grep -qx "$IPSET_MAIN"; then
        ipset swap "$tmpset" "$IPSET_MAIN"
        ipset destroy "$tmpset"
    else
        ipset rename "$tmpset" "$IPSET_MAIN"
    fi
    info "白名单 IP 段共 ${total} 条"

    # 保存到磁盘, 供开机恢复
    mkdir -p "$CONF_DIR"
    ipset save "$IPSET_MAIN" > "${CONF_DIR}/ipset.rules"
    ipset save "$IPSET_EXTRA" >> "${CONF_DIR}/ipset.rules" 2>/dev/null
    return 0
}

ensure_extra_set() {
    ipset list -name | grep -qx "$IPSET_EXTRA" || \
        ipset create "$IPSET_EXTRA" hash:net family inet hashsize 1024 maxelem 65536
}

# 应用 iptables 规则: PORTS 为空表示保护全部端口
apply_iptables() {
    local ports=$1 p

    # 清掉旧链, 重建
    iptables -D INPUT -j "$CHAIN" 2>/dev/null
    iptables -F "$CHAIN" 2>/dev/null
    iptables -X "$CHAIN" 2>/dev/null
    iptables -N "$CHAIN"

    # 放行: 本机回环 / 已建立连接 / 内网 / 额外白名单 / 省份白名单
    iptables -A "$CHAIN" -i lo -j RETURN
    iptables -A "$CHAIN" -m state --state ESTABLISHED,RELATED -j RETURN
    iptables -A "$CHAIN" -s 10.0.0.0/8     -j RETURN
    iptables -A "$CHAIN" -s 172.16.0.0/12  -j RETURN
    iptables -A "$CHAIN" -s 192.168.0.0/16 -j RETURN
    iptables -A "$CHAIN" -m set --match-set "$IPSET_EXTRA" src -j RETURN
    iptables -A "$CHAIN" -m set --match-set "$IPSET_MAIN"  src -j RETURN
    iptables -A "$CHAIN" -j DROP

    if [[ -n "$ports" ]]; then
        for p in ${ports//,/ }; do
            iptables -I INPUT -p tcp --dport "$p" -j "$CHAIN"
            iptables -I INPUT -p udp --dport "$p" -j "$CHAIN"
        done
    else
        iptables -I INPUT -j "$CHAIN"
    fi

    iptables-save > "${CONF_DIR}/iptables.rules"
}

install_persistence() {
    # systemd 开机恢复
    cat > "$SERVICE_FILE" <<EOF
[Unit]
Description=Province IP whitelist (ipset + iptables restore)
After=network.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash ${SCRIPT_PATH} restore-rules

[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reload
    systemctl enable province-whitelist.service >/dev/null 2>&1

    write_cron

    # 把脚本自身复制到固定位置
    [[ "$(readlink -f "$0")" != "$SCRIPT_PATH" ]] && cp -f "$(readlink -f "$0")" "$SCRIPT_PATH"
    chmod +x "$SCRIPT_PATH"
}

load_conf() {
    [[ -f "$CONF_FILE" ]] || { err "未找到配置 ${CONF_FILE}, 请先运行安装"; exit 1; }
    # shellcheck disable=SC1090
    source "$CONF_FILE"
    ENABLED=${ENABLED:-1}
}

save_conf() {
    mkdir -p "$CONF_DIR"
    cat > "$CONF_FILE" <<EOF
PROVINCES="${PROVINCES}"
PORTS="${PORTS}"
ENABLED="${ENABLED:-1}"
EOF
}

# 行政区划代码 -> 省份名
name_of_code() {
    local i
    for i in "${!PROVINCE_CODES[@]}"; do
        [[ ${PROVINCE_CODES[$i]} == "$1" ]] && { echo "${PROVINCE_NAMES[$i]}"; return; }
    done
    echo "$1"
}

# 省份名(江苏)或代码(320000) -> 代码, 未匹配返回 1
resolve_province() {
    local i
    for i in "${!PROVINCE_CODES[@]}"; do
        if [[ ${PROVINCE_CODES[$i]} == "$1" || ${PROVINCE_NAMES[$i]} == "$1" ]]; then
            echo "${PROVINCE_CODES[$i]}"; return 0
        fi
    done
    return 1
}

# 交互式省份菜单, 结果代码存入全局数组 PICKED
pick_provinces() {
    local i picks=() n
    echo "==================== 省份列表 ===================="
    for i in "${!PROVINCE_NAMES[@]}"; do
        printf "%2d) %-4s\t" "$((i+1))" "${PROVINCE_NAMES[$i]}"
        (( (i+1) % 4 == 0 )) && echo
    done
    echo
    echo "=================================================="
    read -rp "$1(可多选, 空格分隔, 如: 16 19): " -a picks
    [[ ${#picks[@]} -eq 0 ]] && { err "未选择任何省份"; exit 1; }
    PICKED=()
    for n in "${picks[@]}"; do
        [[ "$n" =~ ^[0-9]+$ ]] && (( n >= 1 && n <= ${#PROVINCE_CODES[@]} )) \
            || { err "无效编号: $n"; exit 1; }
        PICKED+=("${PROVINCE_CODES[$((n-1))]}")
    done
}

# 把自定义链从 INPUT 上摘下来 (全端口/按端口两种挂载方式都处理)
detach_chain() {
    iptables -D INPUT -j "$CHAIN" 2>/dev/null
    while read -r rule; do
        # shellcheck disable=SC2086
        iptables ${rule/-A/-D} 2>/dev/null
    done < <(iptables-save 2>/dev/null | grep -- "-j ${CHAIN}" | grep "^-A INPUT")
}

write_cron() {
    # 每天自动更新省份 IP 数据 (数据源每小时更新, 每天同步一次足够)
    cat > "$CRON_FILE" <<EOF
$((RANDOM % 60)) $((RANDOM % 6)) * * * root bash ${SCRIPT_PATH} update >/dev/null 2>&1
EOF
}

cmd_install() {
    require_root
    install_deps

    pick_provinces "输入要加入白名单的省份编号"
    local codes=("${PICKED[@]}") names=() c
    for c in "${codes[@]}"; do names+=("$(name_of_code "$c")"); done
    info "已选省份: ${names[*]}"

    echo
    echo "保护模式:"
    echo " 1) 只保护指定端口 (推荐, 如 SSH 端口)"
    echo " 2) 保护全部端口 (网站等对外服务也会被限制, 请确认)"
    read -rp "请选择 [1/2, 默认 1]: " mode
    mode=${mode:-1}

    local ports=""
    if [[ "$mode" == "1" ]]; then
        local sshport
        sshport=$(ss -tnlp 2>/dev/null | awk '/sshd/{split($4,a,":"); print a[length(a)]; exit}')
        sshport=${sshport:-22}
        read -rp "输入要保护的端口(逗号分隔, 默认 ${sshport}): " ports
        ports=${ports:-$sshport}
        [[ "$ports" =~ ^[0-9]+(,[0-9]+)*$ ]] || { err "端口格式错误"; exit 1; }
    fi

    # 防止把自己锁在门外: 把当前 SSH 来源 IP 加入额外白名单
    ensure_extra_set
    local myip=""
    [[ -n "${SSH_CLIENT:-}" ]] && myip=${SSH_CLIENT%% *}
    if [[ -n "$myip" ]]; then
        warn "当前 SSH 来源 IP 为 ${myip}, 将自动加入额外白名单, 避免误锁"
        ipset add "$IPSET_EXTRA" "$myip" 2>/dev/null
    else
        warn "未检测到 SSH 来源 IP, 若你的 IP 不在所选省份内, 应用后可能失联!"
        read -rp "确认继续? [y/N]: " ok
        [[ "$ok" =~ ^[Yy]$ ]] || exit 1
    fi

    mkdir -p "$CONF_DIR"
    build_ipset "${codes[@]}" || exit 1

    PROVINCES="${codes[*]}"
    PORTS="${ports}"
    ENABLED=1
    save_conf

    apply_iptables "$ports"
    install_persistence

    echo
    info "安装完成!"
    info "省份: ${names[*]}"
    [[ -n "$ports" ]] && info "保护端口: ${ports}" || info "保护范围: 全部端口"
    info "IP 数据每天自动更新"
    info "日常管理直接运行: bash ${SCRIPT_PATH}  (交互菜单)"
}

cmd_update() {
    require_root
    load_conf
    ensure_extra_set
    # shellcheck disable=SC2086
    build_ipset $PROVINCES
}

cmd_restore_rules() {
    require_root
    load_conf
    if [[ "$ENABLED" != "1" ]]; then
        info "白名单处于暂停状态, 跳过规则恢复"
        return 0
    fi
    [[ -f "${CONF_DIR}/ipset.rules" ]] && ipset restore -! < "${CONF_DIR}/ipset.rules"
    ensure_extra_set
    ipset list -name | grep -qx "$IPSET_MAIN" || bash "$SCRIPT_PATH" update
    apply_iptables "$PORTS"
}

cmd_status() {
    require_root
    [[ -f "$CONF_FILE" ]] || { warn "白名单未安装"; exit 0; }
    load_conf
    local c pnames=""
    for c in $PROVINCES; do pnames+="$(name_of_code "$c") "; done
    [[ "$ENABLED" == "1" ]] && info "状态: 启用" || warn "状态: 已暂停 (resume 恢复)"
    info "白名单省份: ${pnames}(${PROVINCES})"
    [[ -n "$PORTS" ]] && info "保护端口: ${PORTS}" || info "保护范围: 全部端口"
    [[ -f "$CRON_FILE" ]] && info "每日自动同步: 开启" || info "每日自动同步: 关闭"
    if ipset list -name 2>/dev/null | grep -qx "$IPSET_MAIN"; then
        info "省份 IP 段: $(ipset list "$IPSET_MAIN" -terse | awk '/Number of entries/{print $4}') 条"
        info "额外白名单: $(ipset list "$IPSET_EXTRA" -terse 2>/dev/null | awk '/Number of entries/{print $4}') 条"
    else
        warn "ipset 集合不存在 (未生效)"
    fi
    if iptables -L "$CHAIN" -n >/dev/null 2>&1; then
        echo "---- iptables 链 ${CHAIN} ----"
        iptables -L "$CHAIN" -n -v --line-numbers
    fi
}

cmd_add_province() {
    require_root
    load_conf
    ensure_extra_set
    # shellcheck disable=SC2206
    local codes=($PROVINCES) new=() a c
    if [[ $# -gt 0 ]]; then
        for a in "$@"; do
            c=$(resolve_province "$a") || { err "未知省份: $a (支持省份名如 江苏, 或代码如 320000)"; exit 1; }
            new+=("$c")
        done
    else
        pick_provinces "输入要新增的省份编号"
        new=("${PICKED[@]}")
    fi
    for c in "${new[@]}"; do
        [[ " ${codes[*]} " == *" $c "* ]] || codes+=("$c")
    done
    build_ipset "${codes[@]}" || exit 1
    PROVINCES="${codes[*]}"
    save_conf
    local pnames=""
    for c in $PROVINCES; do pnames+="$(name_of_code "$c") "; done
    info "当前白名单省份: ${pnames}"
}

cmd_del_province() {
    require_root
    load_conf
    ensure_extra_set
    # shellcheck disable=SC2206
    local codes=($PROVINCES) del=() left=() a c
    if [[ $# -gt 0 ]]; then
        for a in "$@"; do
            c=$(resolve_province "$a") || { err "未知省份: $a (支持省份名如 江苏, 或代码如 320000)"; exit 1; }
            del+=("$c")
        done
    else
        local pnames=""
        for c in "${codes[@]}"; do pnames+="$(name_of_code "$c") "; done
        info "当前白名单省份: ${pnames}"
        pick_provinces "输入要移除的省份编号"
        del=("${PICKED[@]}")
    fi
    for c in "${codes[@]}"; do
        [[ " ${del[*]} " == *" $c "* ]] || left+=("$c")
    done
    if [[ ${#left[@]} -eq 0 ]]; then
        err "不能移除全部省份; 如需停用白名单请用: $0 pause 或 $0 uninstall"
        exit 1
    fi
    [[ ${#left[@]} -eq ${#codes[@]} ]] && { warn "所选省份不在当前白名单中, 无变化"; exit 0; }
    build_ipset "${left[@]}" || exit 1
    PROVINCES="${left[*]}"
    save_conf
    local pnames=""
    for c in $PROVINCES; do pnames+="$(name_of_code "$c") "; done
    info "当前白名单省份: ${pnames}"
}

cmd_autoupdate() {
    require_root
    case "${1:-}" in
        on)
            write_cron
            info "已开启每日自动同步省份 IP 数据"
            ;;
        off)
            rm -f "$CRON_FILE"
            info "已关闭每日自动同步 (可随时用 auto-update on 恢复, 或手动执行 update)"
            ;;
        *)
            err "用法: $0 auto-update on|off"; exit 1
            ;;
    esac
}

cmd_pause() {
    require_root
    load_conf
    detach_chain
    ENABLED=0
    save_conf
    iptables-save > "${CONF_DIR}/iptables.rules" 2>/dev/null
    info "白名单已暂停, 不再拦截任何 IP (配置保留, 恢复: $0 resume)"
}

cmd_resume() {
    require_root
    load_conf
    ensure_extra_set
    if ! ipset list -name 2>/dev/null | grep -qx "$IPSET_MAIN"; then
        if [[ -f "${CONF_DIR}/ipset.rules" ]]; then
            ipset restore -! < "${CONF_DIR}/ipset.rules"
        fi
        # shellcheck disable=SC2086
        ipset list -name | grep -qx "$IPSET_MAIN" || build_ipset $PROVINCES || exit 1
    fi
    apply_iptables "$PORTS"
    ENABLED=1
    save_conf
    info "白名单已恢复拦截"
}

cmd_add_ip() {
    require_root
    [[ -n "${1:-}" ]] || { err "用法: $0 add-ip <IP/CIDR>"; exit 1; }
    ensure_extra_set
    ipset add "$IPSET_EXTRA" "$1" && info "已添加 $1"
    ipset save "$IPSET_MAIN" > "${CONF_DIR}/ipset.rules" 2>/dev/null
    ipset save "$IPSET_EXTRA" >> "${CONF_DIR}/ipset.rules"
}

cmd_del_ip() {
    require_root
    [[ -n "${1:-}" ]] || { err "用法: $0 del-ip <IP/CIDR>"; exit 1; }
    ipset del "$IPSET_EXTRA" "$1" && info "已删除 $1"
    ipset save "$IPSET_MAIN" > "${CONF_DIR}/ipset.rules" 2>/dev/null
    ipset save "$IPSET_EXTRA" >> "${CONF_DIR}/ipset.rules"
}

cmd_uninstall() {
    require_root
    detach_chain
    iptables -F "$CHAIN" 2>/dev/null
    iptables -X "$CHAIN" 2>/dev/null
    ipset destroy "$IPSET_MAIN" 2>/dev/null
    ipset destroy "$IPSET_EXTRA" 2>/dev/null
    systemctl disable province-whitelist.service >/dev/null 2>&1
    rm -f "$SERVICE_FILE" "$CRON_FILE"
    systemctl daemon-reload
    rm -rf "$CONF_DIR"
    info "已卸载, 防火墙恢复原样 (脚本 ${SCRIPT_PATH} 保留, 可手动删除)"
}

is_installed() { [[ -f "$CONF_FILE" ]]; }

main_menu() {
    require_root
    local ch summary auto_label pause_label enabled pnames c ip ok
    while true; do
        # 状态摘要
        summary="未安装"
        auto_label="开启每日自动同步"
        pause_label="暂停白名单"
        if is_installed; then
            # shellcheck disable=SC1090
            enabled=$(source "$CONF_FILE" 2>/dev/null; echo "${ENABLED:-1}")
            # shellcheck disable=SC1090
            pnames=$(source "$CONF_FILE" 2>/dev/null; for c in ${PROVINCES}; do printf '%s ' "$(name_of_code "$c")"; done)
            if [[ "$enabled" == "1" ]]; then
                summary="运行中 | 省份: ${pnames}"
                pause_label="暂停白名单"
            else
                summary="已暂停 | 省份: ${pnames}"
                pause_label="恢复白名单"
            fi
            [[ -f "$CRON_FILE" ]] && auto_label="关闭每日自动同步" || auto_label="开启每日自动同步"
        fi

        echo
        echo "============ 省份 IP 白名单管理 ============"
        echo -e " 当前状态: ${GREEN}${summary}${PLAIN}"
        echo "--------------------------------------------"
        echo "  1) 安装 / 重新配置"
        echo "  2) 查看详细状态"
        echo "  3) 新增白名单省份"
        echo "  4) 移除白名单省份"
        echo "  5) 添加额外白名单 IP"
        echo "  6) 删除额外白名单 IP"
        echo "  7) 立即更新省份 IP 数据"
        echo "  8) ${auto_label}"
        echo "  9) ${pause_label}"
        echo " 10) 卸载"
        echo "  0) 退出"
        echo "============================================"
        read -rp "请选择 [0-10]: " ch

        # 除安装外的操作都需要先安装; 子命令放子 shell 里跑,
        # 内部 exit 不会退出菜单
        case "$ch" in
            1) ( cmd_install ) ;;
            2) is_installed && ( cmd_status ) || err "尚未安装, 请先选 1" ;;
            3) is_installed && ( cmd_add_province ) || err "尚未安装, 请先选 1" ;;
            4) is_installed && ( cmd_del_province ) || err "尚未安装, 请先选 1" ;;
            5)
                is_installed || { err "尚未安装, 请先选 1"; continue; }
                read -rp "输入要添加的 IP 或网段(如 1.2.3.4 或 1.2.3.0/24): " ip
                [[ -n "$ip" ]] && ( cmd_add_ip "$ip" )
                ;;
            6)
                is_installed || { err "尚未安装, 请先选 1"; continue; }
                read -rp "输入要删除的 IP 或网段: " ip
                [[ -n "$ip" ]] && ( cmd_del_ip "$ip" )
                ;;
            7) is_installed && ( cmd_update ) || err "尚未安装, 请先选 1" ;;
            8)
                is_installed || { err "尚未安装, 请先选 1"; continue; }
                if [[ -f "$CRON_FILE" ]]; then ( cmd_autoupdate off ); else ( cmd_autoupdate on ); fi
                ;;
            9)
                is_installed || { err "尚未安装, 请先选 1"; continue; }
                # shellcheck disable=SC1090
                enabled=$(source "$CONF_FILE" 2>/dev/null; echo "${ENABLED:-1}")
                if [[ "$enabled" == "1" ]]; then ( cmd_pause ); else ( cmd_resume ); fi
                ;;
            10)
                is_installed || { err "尚未安装, 无需卸载"; continue; }
                read -rp "确认卸载并清除所有规则? [y/N]: " ok
                [[ "$ok" =~ ^[Yy]$ ]] && ( cmd_uninstall )
                ;;
            0) exit 0 ;;
            *) err "无效选择: $ch" ;;
        esac
    done
}

cmd=${1:-menu}
shift 2>/dev/null || true
case "$cmd" in
    menu)           main_menu ;;
    install)        cmd_install ;;
    update)         cmd_update ;;
    status)         cmd_status ;;
    add-province)   cmd_add_province "$@" ;;
    del-province)   cmd_del_province "$@" ;;
    add-ip)         cmd_add_ip "${1:-}" ;;
    del-ip)         cmd_del_ip "${1:-}" ;;
    auto-update)    cmd_autoupdate "${1:-}" ;;
    pause)          cmd_pause ;;
    resume)         cmd_resume ;;
    restore-rules)  cmd_restore_rules ;;
    uninstall)      cmd_uninstall ;;
    *)
        echo "用法: $0                # 交互式管理菜单"
        echo "     $0 {install|update|status|add-province [省份]...|del-province [省份]...|add-ip <IP>|del-ip <IP>|auto-update on|off|pause|resume|uninstall}"
        exit 1
        ;;
esac
