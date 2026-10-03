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
#     bash province_whitelist.sh check-ip <IP>      # 查某个 IP 是否放行, 以及数据源把它归到哪个省
#     bash province_whitelist.sh auto-update on|off # 开启/关闭每日自动同步
#     bash province_whitelist.sh pause      # 暂停白名单(保留配置, 不再拦截)
#     bash province_whitelist.sh resume     # 恢复白名单
#     bash province_whitelist.sh uninstall  # 卸载并恢复原样
#
#   升级: 直接用新版脚本执行任意命令(如 status), 会自动把新版同步到
#         /usr/local/bin 并刷新 systemd 单元, 已有配置和规则原样保留。
#=================================================================#

set -u

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; PLAIN='\033[0m'

CONF_DIR="/etc/province-whitelist"
CONF_FILE="${CONF_DIR}/config"
IPSET_RULES="${CONF_DIR}/ipset.rules"
IPSET_MAIN="province_wl"        # 省份 IP 集合
IPSET_EXTRA="province_wl_extra" # 额外手动白名单集合
CHAIN="PROVINCE_WL"             # iptables 自定义链
SERVICE_NAME="province-whitelist.service"
SERVICE_FILE="/etc/systemd/system/${SERVICE_NAME}"
CRON_FILE="/etc/cron.d/province-whitelist"
SCRIPT_PATH="/usr/local/bin/province_whitelist.sh"
SCRIPT_URL="https://raw.githubusercontent.com/jiang111/jiang111/master/province_whitelist.sh"
LOCK_FILE="/var/lock/province-whitelist.lock"

# 数据源镜像, 按顺序尝试 (前两个国内一般可直连)
MIRRORS=(
    "https://cdn.jsdelivr.net/gh/metowolf/iplist/data/cncity"
    "https://fastly.jsdelivr.net/gh/metowolf/iplist/data/cncity"
    "https://raw.githubusercontent.com/metowolf/iplist/master/data/cncity"
    "https://metowolf.github.io/iplist/data/cncity"
)

# GB/T 2260 省级行政区划代码 (仅中国大陆)
PROVINCE_CODES=(110000 120000 130000 140000 150000 210000 220000 230000 310000 320000 330000 340000 350000 360000 370000 410000 420000 430000 440000 450000 460000 500000 510000 520000 530000 540000 610000 620000 630000 640000 650000)
PROVINCE_NAMES=("北京" "天津" "河北" "山西" "内蒙古" "辽宁" "吉林" "黑龙江" "上海" "江苏" "浙江" "安徽" "福建" "江西" "山东" "河南" "湖北" "湖南" "广东" "广西" "海南" "重庆" "四川" "贵州" "云南" "西藏" "陕西" "甘肃" "青海" "宁夏" "新疆")

# 注意: 数据源的省份归属并不精确. 运营商整体登记在总部的大段(如联通
# 116.128.0.0/10 登记在北京)会被大量划到北京, 江苏联通的手机 IP 也可能落在北京文件里.
# 遇到"明明是本省 IP 却被拦"的情况, 用 check-ip 查归属, 再决定加省份还是 add-ip.

err()  { echo -e "${RED}[错误]${PLAIN} $*" >&2; }
warn() { echo -e "${YELLOW}[警告]${PLAIN} $*"; }
info() { echo -e "${GREEN}[信息]${PLAIN} $*"; }

require_root() {
    [[ $EUID -eq 0 ]] || { err "请使用 root 运行此脚本"; exit 1; }
}

is_installed() { [[ -f "$CONF_FILE" ]]; }

is_ipv4() { [[ "${1:-}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]]; }

ipset_exists() { ipset list -name 2>/dev/null | grep -qx "$1"; }

# 自定义链是否已挂到 INPUT 上
chain_hooked() { iptables-save 2>/dev/null | grep -q "^-A INPUT .*-j ${CHAIN}$"; }

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

# 把两个集合保存到磁盘供开机恢复 (先写临时文件再替换, 集合不存在就跳过, 不会把文件清空)
save_ipsets() {
    mkdir -p "$CONF_DIR"
    {
        ipset_exists "$IPSET_MAIN"  && ipset save "$IPSET_MAIN"
        ipset_exists "$IPSET_EXTRA" && ipset save "$IPSET_EXTRA"
        true
    } > "${IPSET_RULES}.tmp" && mv -f "${IPSET_RULES}.tmp" "$IPSET_RULES"
}

# 下载所选省份数据并原子更新 ipset (先建临时集合再 swap, 更新过程不断流)
# 整个过程持有文件锁, 避免 cron 与手动更新同时跑时争用临时集合
build_ipset() {
    (
        exec 9>"$LOCK_FILE"
        if ! flock -w 600 9; then
            err "另一个更新正在进行中, 等待超时, 本次放弃"
            exit 1
        fi
        build_ipset_locked "$@"
    )
}

build_ipset_locked() {
    local codes=("$@")
    local tmpdir tmpset="${IPSET_MAIN}_tmp" total=0 code name

    tmpdir=$(mktemp -d)

    for code in "${codes[@]}"; do
        name=$(name_of_code "$code")
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

    total=$(ipset list "$tmpset" -terse 2>/dev/null | awk '/Number of entries/{print $4}')
    if [[ -z "$total" || "$total" -lt 10 ]]; then
        err "IP 段数量异常(${total:-0}), 放弃本次更新"
        ipset destroy "$tmpset" 2>/dev/null
        return 1
    fi

    if ipset_exists "$IPSET_MAIN"; then
        ipset swap "$tmpset" "$IPSET_MAIN"
        ipset destroy "$tmpset"
    else
        ipset rename "$tmpset" "$IPSET_MAIN"
    fi
    info "白名单 IP 段共 ${total} 条"

    save_ipsets
    return 0
}

ensure_extra_set() {
    ipset_exists "$IPSET_EXTRA" || \
        ipset create "$IPSET_EXTRA" hash:net family inet hashsize 1024 maxelem 65536
}

# 把自定义链从 INPUT 上摘下来 (全端口/按端口两种挂载方式都处理, 重复挂载也会全部摘掉)
detach_chain() {
    local rule
    while read -r rule; do
        # shellcheck disable=SC2086
        iptables ${rule/-A/-D} 2>/dev/null
    done < <(iptables-save 2>/dev/null | grep -- "-j ${CHAIN}$" | grep "^-A INPUT")
}

# 应用 iptables 规则: PORTS 为空表示保护全部端口
# 两个集合必须都存在才会挂上 DROP, 否则宁可不拦截, 也不能把所有人挡在门外
apply_iptables() {
    local ports=$1 p

    if ! ipset_exists "$IPSET_MAIN"; then
        err "ipset 集合 ${IPSET_MAIN} 不存在, 为避免误锁, 本次不应用拦截规则"
        return 1
    fi
    ensure_extra_set

    # 先摘掉所有挂载(包括旧版本可能残留的重复挂载), 再重建链
    detach_chain
    iptables -F "$CHAIN" 2>/dev/null
    iptables -X "$CHAIN" 2>/dev/null
    iptables -N "$CHAIN" || { err "创建 iptables 链 ${CHAIN} 失败"; return 1; }

    # 放行: 本机回环 / 已建立连接 / 内网 / 额外白名单 / 省份白名单
    if ! { iptables -A "$CHAIN" -i lo -j RETURN \
        && iptables -A "$CHAIN" -m state --state ESTABLISHED,RELATED -j RETURN \
        && iptables -A "$CHAIN" -s 10.0.0.0/8     -j RETURN \
        && iptables -A "$CHAIN" -s 172.16.0.0/12  -j RETURN \
        && iptables -A "$CHAIN" -s 192.168.0.0/16 -j RETURN \
        && iptables -A "$CHAIN" -m set --match-set "$IPSET_EXTRA" src -j RETURN \
        && iptables -A "$CHAIN" -m set --match-set "$IPSET_MAIN"  src -j RETURN; }; then
        err "放行规则添加失败, 为避免误锁, 不挂载 DROP 规则"
        iptables -F "$CHAIN" 2>/dev/null
        iptables -X "$CHAIN" 2>/dev/null
        return 1
    fi
    iptables -A "$CHAIN" -j DROP

    if [[ -n "$ports" ]]; then
        for p in ${ports//,/ }; do
            iptables -I INPUT -p tcp --dport "$p" -j "$CHAIN"
            iptables -I INPUT -p udp --dport "$p" -j "$CHAIN"
        done
    else
        iptables -I INPUT -j "$CHAIN"
    fi
    return 0
}

# 写 systemd 单元 (内容不变就不动; 变了就重写并 daemon-reload, 用于升级旧版本)
ensure_service_unit() {
    local content
    content="[Unit]
Description=Province IP whitelist (ipset + iptables restore)
Wants=network-online.target
After=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/bin/bash ${SCRIPT_PATH} restore-rules

[Install]
WantedBy=multi-user.target
"
    if [[ ! -f "$SERVICE_FILE" ]] || [[ "$(cat "$SERVICE_FILE")" != "${content%$'\n'}" ]]; then
        printf '%s' "$content" > "$SERVICE_FILE"
        systemctl daemon-reload
        info "已更新 systemd 单元 ${SERVICE_NAME}"
    fi
    systemctl enable "$SERVICE_NAME" >/dev/null 2>&1
}

# 把当前运行的脚本同步到固定位置 (cron / systemd 运行的是那份副本)
# $1 = force: 即使没有本地文件(curl | bash 方式运行)也要从仓库下载一份
sync_script() {
    local force=${1:-} src
    src=$(readlink -f "$0" 2>/dev/null || true)
    if [[ -n "$src" && -f "$src" && "$src" != "$SCRIPT_PATH" ]]; then
        if ! cmp -s "$src" "$SCRIPT_PATH"; then
            cp -f "$src" "$SCRIPT_PATH" || { err "复制脚本到 ${SCRIPT_PATH} 失败"; return 1; }
            info "已更新脚本副本 ${SCRIPT_PATH}"
        fi
    elif [[ ! -f "$src" ]] && { [[ -n "$force" ]] || [[ ! -f "$SCRIPT_PATH" ]]; }; then
        # 通过 curl | bash 等方式运行, 本地没有脚本文件, 从仓库下载
        info "从 ${SCRIPT_URL} 下载脚本到 ${SCRIPT_PATH}..."
        if ! curl -fsSL --max-time 60 "$SCRIPT_URL" -o "${SCRIPT_PATH}.tmp"; then
            rm -f "${SCRIPT_PATH}.tmp"
            err "下载失败; 请把脚本保存为文件后再运行, 否则开机恢复和自动更新无法工作"
            return 1
        fi
        mv -f "${SCRIPT_PATH}.tmp" "$SCRIPT_PATH"
    fi
    [[ -f "$SCRIPT_PATH" ]] || { err "${SCRIPT_PATH} 不存在"; return 1; }
    chmod +x "$SCRIPT_PATH"
}

# $1 = 1 表示全新安装(写 cron); 重装时尊重用户之前 auto-update off 的选择
install_persistence() {
    local fresh=${1:-1}
    sync_script force || exit 1
    ensure_service_unit
    if [[ "$fresh" == "1" || -f "$CRON_FILE" ]]; then
        write_cron
    fi
}

# 新版脚本直接运行时, 顺手把已安装的副本和 systemd 单元升级到当前版本
upgrade_installed() {
    [[ $EUID -eq 0 ]] && is_installed || return 0
    sync_script || true
    ensure_service_unit
}

load_conf() {
    [[ -f "$CONF_FILE" ]] || { err "未找到配置 ${CONF_FILE}, 请先运行安装"; exit 1; }
    # shellcheck disable=SC1090
    source "$CONF_FILE"
    PROVINCES=${PROVINCES:-}
    PORTS=${PORTS:-}
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
    local i picks=() n line
    echo "==================== 省份列表 ===================="
    for i in "${!PROVINCE_NAMES[@]}"; do
        printf "%2d) %-4s\t" "$((i+1))" "${PROVINCE_NAMES[$i]}"
        (( (i+1) % 4 == 0 )) && echo
    done
    echo
    echo "=================================================="
    read -rp "$1(可多选, 空格或逗号分隔, 如: 16 19): " line
    # shellcheck disable=SC2206
    picks=(${line//,/ })
    [[ ${#picks[@]} -eq 0 ]] && { err "未选择任何省份"; exit 1; }
    PICKED=()
    for n in "${picks[@]}"; do
        [[ "$n" =~ ^[0-9]+$ ]] || { err "无效编号: $n"; exit 1; }
        n=$((10#$n))   # 避免 08 之类被当作八进制
        (( n >= 1 && n <= ${#PROVINCE_CODES[@]} )) || { err "无效编号: $n"; exit 1; }
        PICKED+=("${PROVINCE_CODES[$((n-1))]}")
    done
}

# 校验 "22,80,443" 形式的端口列表, 每个都在 1-65535
validate_ports() {
    local p
    [[ "$1" =~ ^[0-9]+(,[0-9]+)*$ ]] || return 1
    for p in ${1//,/ }; do
        p=$((10#$p))
        (( p >= 1 && p <= 65535 )) || return 1
    done
    return 0
}

write_cron() {
    # 每天自动更新省份 IP 数据 (数据源每小时更新, 每天同步一次足够)
    cat > "$CRON_FILE" <<EOF
$((RANDOM % 60)) $((RANDOM % 6)) * * * root bash ${SCRIPT_PATH} update >/dev/null 2>&1
EOF
}

# 当前 sshd 监听端口 (多个时取第一个), 取不到返回 22
detect_ssh_port() {
    local port
    port=$(ss -tnlp 2>/dev/null | awk '/sshd/{split($4,a,":"); print a[length(a)]; exit}')
    echo "${port:-22}"
}

# 当前 SSH 来源 IP (IPv4). sudo 默认会清掉 SSH_* 环境变量, 所以逐级回退:
#   SSH_CLIENT -> SSH_CONNECTION -> 登录记录(who am i) -> sshd 唯一的已建立连接
detect_ssh_ip() {
    local ip="" peers
    [[ -n "${SSH_CLIENT:-}" ]] && ip=${SSH_CLIENT%% *}
    [[ -z "$ip" && -n "${SSH_CONNECTION:-}" ]] && ip=${SSH_CONNECTION%% *}
    if ! is_ipv4 "$ip"; then
        ip=$(who am i 2>/dev/null | awk '{print $NF}' | tr -d '()')
    fi
    if ! is_ipv4 "$ip"; then
        peers=$(ss -tnH state established "( sport = :$(detect_ssh_port) )" 2>/dev/null \
            | awk '{print $4}' | sed -E 's/^\[::ffff:([0-9.]+)\]/\1/; s/:[0-9]+$//' | sort -u)
        [[ $(printf '%s\n' "$peers" | grep -c .) -eq 1 ]] && ip=$peers
    fi
    is_ipv4 "$ip" && echo "$ip"
}

cmd_install() {
    require_root
    install_deps

    local fresh=1 fw
    is_installed && fresh=0

    for fw in ufw firewalld; do
        systemctl is-active --quiet "$fw" 2>/dev/null && \
            warn "检测到 ${fw} 正在运行, 它 reload 时会清掉本脚本的 iptables 规则, 之后需执行 $0 resume 重新应用 (每日自动更新也会顺带补上)"
    done

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
    [[ "$mode" == "1" || "$mode" == "2" ]] || { err "无效选择: ${mode}, 只能是 1 或 2"; exit 1; }

    local ports=""
    if [[ "$mode" == "1" ]]; then
        local sshport
        sshport=$(detect_ssh_port)
        read -rp "输入要保护的端口(逗号分隔, 默认 ${sshport}): " ports
        ports=${ports:-$sshport}
        ports=${ports// /}
        validate_ports "$ports" || { err "端口格式错误: ${ports} (应为 1-65535 的数字, 逗号分隔)"; exit 1; }
    else
        read -rp "确认保护全部端口? 不在白名单内的 IP 将无法访问本机任何服务 [y/N]: " ok
        [[ "$ok" =~ ^[Yy]$ ]] || exit 1
    fi

    # 防止把自己锁在门外: 把当前 SSH 来源 IP 加入额外白名单
    ensure_extra_set
    local myip
    myip=$(detect_ssh_ip)
    if [[ -n "$myip" ]]; then
        warn "当前 SSH 来源 IP 为 ${myip}, 将自动加入额外白名单, 避免误锁"
        ipset add "$IPSET_EXTRA" "$myip" -exist
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

    apply_iptables "$ports" || exit 1
    install_persistence "$fresh"

    echo
    info "安装完成!"
    info "省份: ${names[*]}"
    [[ -n "$ports" ]] && info "保护端口: ${ports}" || info "保护范围: 全部端口"
    [[ -f "$CRON_FILE" ]] && info "IP 数据每天自动更新" || info "每日自动更新处于关闭状态 (auto-update on 开启)"
    info "日常管理直接运行: bash ${SCRIPT_PATH}  (交互菜单)"
}

cmd_update() {
    require_root
    load_conf
    ensure_extra_set
    # shellcheck disable=SC2086
    build_ipset $PROVINCES || exit 1
    # 启用状态下如果链被别的防火墙工具 (ufw/firewalld reload) 清掉了, 顺带补回来
    if [[ "$ENABLED" == "1" ]] && ! chain_hooked; then
        warn "检测到 iptables 规则丢失, 重新应用"
        apply_iptables "$PORTS" || exit 1
    fi
}

cmd_restore_rules() {
    require_root
    load_conf
    if [[ "$ENABLED" != "1" ]]; then
        info "白名单处于暂停状态, 跳过规则恢复"
        return 0
    fi
    [[ -f "$IPSET_RULES" ]] && ipset restore -! < "$IPSET_RULES"
    ensure_extra_set
    # 磁盘上没有集合数据时(例如首次安装后文件被删), 尝试直接下载
    # shellcheck disable=SC2086
    ipset_exists "$IPSET_MAIN" || build_ipset $PROVINCES || true
    if ! ipset_exists "$IPSET_MAIN"; then
        err "省份 IP 集合不存在且无法下载 (网络未就绪?), 本次不应用拦截规则以免误锁; 请稍后执行: ${SCRIPT_PATH} resume"
        return 1
    fi
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
    if ipset_exists "$IPSET_MAIN"; then
        info "省份 IP 段: $(ipset list "$IPSET_MAIN" -terse | awk '/Number of entries/{print $4}') 条"
        info "额外白名单: $(ipset list "$IPSET_EXTRA" -terse 2>/dev/null | awk '/Number of entries/{print $4}') 条"
    else
        warn "ipset 集合不存在 (未生效)"
    fi
    if chain_hooked; then
        info "iptables 规则: 已挂载到 INPUT"
    elif [[ "$ENABLED" == "1" ]]; then
        warn "iptables 规则: 未挂载到 INPUT (当前没有在拦截, 可执行 resume 重新应用)"
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
    info "白名单已暂停, 不再拦截任何 IP (配置保留, 恢复: $0 resume)"
}

cmd_resume() {
    require_root
    load_conf
    ensure_extra_set
    if ! ipset_exists "$IPSET_MAIN"; then
        [[ -f "$IPSET_RULES" ]] && ipset restore -! < "$IPSET_RULES"
        # shellcheck disable=SC2086
        ipset_exists "$IPSET_MAIN" || build_ipset $PROVINCES || exit 1
    fi
    apply_iptables "$PORTS" || exit 1
    ENABLED=1
    save_conf
    info "白名单已恢复拦截"
}

cmd_add_ip() {
    require_root
    [[ -n "${1:-}" ]] || { err "用法: $0 add-ip <IP/CIDR>"; exit 1; }
    ensure_extra_set
    ipset add "$IPSET_EXTRA" "$1" -exist || exit 1
    info "已添加 $1"
    save_ipsets
}

cmd_del_ip() {
    require_root
    [[ -n "${1:-}" ]] || { err "用法: $0 del-ip <IP/CIDR>"; exit 1; }
    ensure_extra_set
    ipset del "$IPSET_EXTRA" "$1" || exit 1
    info "已删除 $1"
    save_ipsets
}

# 查某个 IP 当前是否放行, 以及数据源把它归到哪个省 (逐省下载后用临时集合匹配)
cmd_check_ip() {
    require_root
    local ip=${1:-} hit=0 tmpdir chk="${IPSET_MAIN}_chk" i code found=()
    is_ipv4 "$ip" || { err "用法: $0 check-ip <IPv4 地址>"; exit 1; }

    if ipset_exists "$IPSET_EXTRA" && ipset test "$IPSET_EXTRA" "$ip" 2>/dev/null; then
        info "${ip} 在额外白名单 (${IPSET_EXTRA}) 中, 放行"; hit=1
    fi
    if ipset_exists "$IPSET_MAIN" && ipset test "$IPSET_MAIN" "$ip" 2>/dev/null; then
        info "${ip} 在省份白名单 (${IPSET_MAIN}) 中, 放行"; hit=1
    fi
    if [[ $hit -eq 0 ]]; then
        if ipset_exists "$IPSET_MAIN"; then
            warn "${ip} 不在当前白名单中, 受保护端口上会被拦截"
        else
            warn "白名单集合不存在, 无法判断当前是否放行"
        fi
    fi

    info "在数据源中查找 ${ip} 的省份归属 (需下载全部省份数据, 请稍候)..."
    tmpdir=$(mktemp -d)
    ipset destroy "$chk" 2>/dev/null
    for i in "${!PROVINCE_CODES[@]}"; do
        code=${PROVINCE_CODES[$i]}
        if ! fetch_province "$code" "${tmpdir}/${code}.txt"; then
            warn "下载 ${PROVINCE_NAMES[$i]}(${code}) 失败, 跳过"
            continue
        fi
        ipset create "$chk" hash:net family inet hashsize 1024 maxelem 262144 2>/dev/null || break
        grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+' "${tmpdir}/${code}.txt" \
            | sed "s/^/add ${chk} /" | ipset restore -!
        if ipset test "$chk" "$ip" 2>/dev/null; then
            found+=("${PROVINCE_NAMES[$i]}(${code}): $(grep -E '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+' "${tmpdir}/${code}.txt" | cidr_containing "$ip")")
        fi
        ipset destroy "$chk" 2>/dev/null
    done
    rm -rf "$tmpdir"
    ipset destroy "$chk" 2>/dev/null

    if [[ ${#found[@]} -eq 0 ]]; then
        warn "数据源的 31 个省份文件里都没有 ${ip}; 如需放行请用: $0 add-ip ${ip}"
    else
        info "数据源归属: ${found[*]}"
        info "如归属与实际不符, 可直接放行该网段: $0 add-ip <网段>, 或把该省加入白名单: $0 add-province <省份>"
    fi
}

# 从 stdin 的 CIDR 列表里找出包含指定 IP 的网段 (纯 bash 位运算)
cidr_containing() {
    local ip=$1 cidr net bits ipn netn mask a b c d
    IFS=. read -r a b c d <<< "$ip"; ipn=$(( (a<<24) | (b<<16) | (c<<8) | d ))
    while read -r cidr; do
        net=${cidr%/*}; bits=${cidr#*/}
        IFS=. read -r a b c d <<< "$net"; netn=$(( (a<<24) | (b<<16) | (c<<8) | d ))
        mask=$(( bits == 0 ? 0 : (0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF ))
        (( (ipn & mask) == (netn & mask) )) && printf '%s ' "$cidr"
    done
    echo
}

cmd_uninstall() {
    require_root
    detach_chain
    iptables -F "$CHAIN" 2>/dev/null
    iptables -X "$CHAIN" 2>/dev/null
    ipset destroy "$IPSET_MAIN" 2>/dev/null
    ipset destroy "$IPSET_EXTRA" 2>/dev/null
    systemctl disable "$SERVICE_NAME" >/dev/null 2>&1
    rm -f "$SERVICE_FILE" "$CRON_FILE" "$LOCK_FILE"
    systemctl daemon-reload
    rm -rf "$CONF_DIR"
    info "已卸载, 防火墙恢复原样 (脚本 ${SCRIPT_PATH} 保留, 可手动删除)"
}

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
            pnames=$(source "$CONF_FILE" 2>/dev/null; for c in ${PROVINCES:-}; do printf '%s ' "$(name_of_code "$c")"; done)
            if [[ "$enabled" == "1" ]]; then
                summary="运行中 | 省份: ${pnames}"
                pause_label="暂停白名单"
                chain_hooked || summary="已启用但规则未挂载(选 9 两次或执行 resume) | 省份: ${pnames}"
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
        echo " 11) 查询某个 IP 是否放行 / 归属省份"
        echo "  0) 退出"
        echo "============================================"
        read -rp "请选择 [0-11]: " ch

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
            11)
                read -rp "输入要查询的 IPv4 地址: " ip
                [[ -n "$ip" ]] && ( cmd_check_ip "$ip" )
                ;;
            0) exit 0 ;;
            *) err "无效选择: $ch" ;;
        esac
    done
}

cmd=${1:-menu}
shift 2>/dev/null || true

# 已安装的机器上用新版脚本跑任何管理命令时, 先把副本和 systemd 单元升级到当前版本
case "$cmd" in
    install|restore-rules|uninstall) ;;
    *) upgrade_installed ;;
esac

case "$cmd" in
    menu)           main_menu ;;
    install)        cmd_install ;;
    update)         cmd_update ;;
    status)         cmd_status ;;
    add-province)   cmd_add_province "$@" ;;
    del-province)   cmd_del_province "$@" ;;
    add-ip)         cmd_add_ip "${1:-}" ;;
    del-ip)         cmd_del_ip "${1:-}" ;;
    check-ip)       cmd_check_ip "${1:-}" ;;
    auto-update)    cmd_autoupdate "${1:-}" ;;
    pause)          cmd_pause ;;
    resume)         cmd_resume ;;
    restore-rules)  cmd_restore_rules ;;
    uninstall)      cmd_uninstall ;;
    *)
        echo "用法: $0                # 交互式管理菜单"
        echo "     $0 {install|update|status|add-province [省份]...|del-province [省份]...|add-ip <IP>|del-ip <IP>|check-ip <IP>|auto-update on|off|pause|resume|uninstall}"
        exit 1
        ;;
esac
