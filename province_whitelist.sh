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
#     bash province_whitelist.sh add-ip <IP/CIDR> [b|c]  # 添加额外白名单 IP; 加 b 放行整个 B 段(/16), 加 c 放行 C 段(/24)
#     bash province_whitelist.sh del-ip <IP/CIDR> [b|c]  # 删除额外白名单 IP/网段
#     bash province_whitelist.sh check-ip <IP>      # 查某个 IP 是否放行, 以及数据源把它归到哪个省
#     bash province_whitelist.sh auto-update on|off # 开启/关闭每日自动同步
#     bash province_whitelist.sh pause      # 暂停白名单(保留配置, 不再拦截)
#     bash province_whitelist.sh resume     # 恢复白名单
#     bash province_whitelist.sh uninstall  # 卸载并恢复原样
#
#   升级: 直接用新版脚本执行任意管理命令(如 status), 会自动把新版同步到
#         /usr/local/bin 并刷新 systemd 单元, 已有配置和规则原样保留。
#         同步只升不降(按 VERSION 比较), 用旧脚本跑命令不会覆盖新版本。
#=================================================================#

set -u

# 脚本版本号 (整数). 自动同步副本时只升不降, 用旧脚本跑命令不会把已安装的新版本覆盖掉
VERSION=4

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[0;33m'; PLAIN='\033[0m'

# 数据文件里的 IPv4 CIDR 行
CIDR_RE='^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+/[0-9]+'

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

is_ipv4() {
    local o
    [[ "${1:-}" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] || return 1
    for o in ${1//./ }; do (( 10#$o <= 255 )) || return 1; done
    return 0
}

ipset_exists() { ipset list -name 2>/dev/null | grep -qx "$1"; }

# 自定义链是否已挂到 INPUT 上
chain_hooked() { iptables-save 2>/dev/null | grep -q "^-A INPUT .*-j ${CHAIN}$"; }

install_deps() {
    local need=()
    command -v ipset >/dev/null 2>&1 || need+=(ipset)
    command -v iptables >/dev/null 2>&1 || need+=(iptables)
    command -v curl >/dev/null 2>&1 || need+=(curl)
    command -v flock >/dev/null 2>&1 || need+=(util-linux)
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
            if grep -Eq "$CIDR_RE" "${out}.tmp"; then
                mv "${out}.tmp" "${out}"
                return 0
            fi
        fi
    done
    rm -f "${out}.tmp"
    return 1
}

# 在文件锁内执行命令 (子 shell 里跑, 退出即释放锁). 没有 flock 时降级为不加锁并提示.
# 注意: 已经持锁的代码里不要再套一层 with_lock, 会自己等自己.
with_lock() {
    (
        if command -v flock >/dev/null 2>&1; then
            exec 9>"$LOCK_FILE"
            if ! flock -w 600 9; then
                err "另一个操作正在进行中 (更新/写盘), 等待锁超时, 本次放弃"
                exit 1
            fi
        else
            warn "系统没有 flock 命令, 本次不加锁执行 (建议安装 util-linux)"
        fi
        "$@"
    )
}

# 把两个集合保存到磁盘供开机恢复 (写到唯一的临时文件再替换, 集合不存在就跳过, 不会把文件清空)
# 任何一个 ipset save 失败都不替换原文件, 宁可保留上一次的完整数据
# 调用方负责持锁: build_ipset_apply 内部已持锁, 其他地方用 with_lock save_ipsets
save_ipsets() {
    local tmp rc=0
    mkdir -p "$CONF_DIR"
    tmp=$(mktemp "${IPSET_RULES}.XXXXXX") || return 1
    {
        if ipset_exists "$IPSET_MAIN";  then ipset save "$IPSET_MAIN"  || rc=1; fi
        if ipset_exists "$IPSET_EXTRA"; then ipset save "$IPSET_EXTRA" || rc=1; fi
    } > "$tmp"
    if (( rc != 0 )); then
        rm -f "$tmp"
        err "ipset save 失败, 保留原有的 ${IPSET_RULES}"
        return 1
    fi
    mv -f "$tmp" "$IPSET_RULES"
}

# 下载所选省份数据并原子更新 ipset (先建临时集合再 swap, 更新过程不断流)
# 下载阶段不加锁 (各进程用自己的临时目录), 只有建集合/swap/写盘这一小段持锁,
# 这样 cron 更新卡在慢镜像上时, add-ip 之类的写盘操作不会跟着等
build_ipset() {
    local codes=("$@")
    local tmpdir code name rc

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

    with_lock build_ipset_apply "$tmpdir" "${codes[@]}"
    rc=$?
    rm -rf "$tmpdir"
    return $rc
}

# $1 = 已下载好数据的目录, 其余参数 = 省份代码. 在锁内执行.
build_ipset_apply() {
    local tmpdir=$1; shift
    local codes=("$@") tmpset="${IPSET_MAIN}_tmp" total=0 code

    ipset destroy "$tmpset" 2>/dev/null
    ipset create "$tmpset" hash:net family inet hashsize 4096 maxelem 262144 || return 1

    {
        for code in "${codes[@]}"; do
            grep -E "$CIDR_RE" "${tmpdir}/${code}.txt" \
                | sed "s/^/add ${tmpset} /"
        done
    } | ipset restore -!

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

    # 内存里的集合已经生效; 写盘失败只影响下次开机, 提示但不算本次更新失败
    save_ipsets || warn "本次更新未能写入磁盘, 重启后会使用上一次保存的数据"
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
# 排在常见防火墙工具之后, 否则它们启动时会把本脚本刚挂上的链清掉 (不存在的单元会被 systemd 忽略)
After=network-online.target ufw.service firewalld.service netfilter-persistent.service iptables.service nftables.service

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

# 读取某个脚本文件里的 VERSION, 读不到(老版本没有这个字段)按 0
script_version_of() {
    local v
    v=$(grep -m1 -E '^VERSION=[0-9]+' "$1" 2>/dev/null | cut -d= -f2)
    echo "${v:-0}"
}

# 用新内容原子替换 SCRIPT_PATH: 写到临时文件再 mv 换 inode.
# bash 是边读边执行脚本文件的, 直接 cp 原地覆盖会让正在跑的 cron/systemd 进程读到错位的内容.
replace_script_with() {
    local tmp
    tmp=$(mktemp "${SCRIPT_PATH}.XXXXXX") || return 1
    if ! cat "$1" > "$tmp"; then rm -f "$tmp"; return 1; fi
    chmod 755 "$tmp"
    mv -f "$tmp" "$SCRIPT_PATH"
}

# 把当前运行的脚本同步到固定位置 (cron / systemd 运行的是那份副本)
# $1 = force: install 时使用. 即使没有本地文件(curl | bash 方式运行)也要从仓库下载一份,
#             且允许用旧版本覆盖(用户明确要装这一版). 不带 force 时只升不降.
# 返回: 0 已同步或无需同步; 2 因为已安装的版本更新而拒绝覆盖; 1 出错
sync_script() {
    local force=${1:-} src tmp installed_ver dl_ver
    src=$(readlink -f "$0" 2>/dev/null || true)
    # 只有确实是本脚本的文件才算"本地文件". curl | bash 时 $0 是 "bash",
    # readlink 可能解析到 bash 二进制本身, 不能把它复制过去.
    if [[ -z "$src" || ! -f "$src" ]] || ! grep -aqE '^VERSION=[0-9]+$' "$src"; then
        src=""
    fi

    if [[ -n "$src" && "$src" != "$SCRIPT_PATH" ]]; then
        if ! cmp -s "$src" "$SCRIPT_PATH"; then
            installed_ver=$(script_version_of "$SCRIPT_PATH")
            if [[ -z "$force" ]] && (( installed_ver > VERSION )); then
                warn "已安装的脚本版本(${installed_ver})比当前运行的(${VERSION})新, 不覆盖 ${SCRIPT_PATH}"
                return 2
            fi
            replace_script_with "$src" || { err "复制脚本到 ${SCRIPT_PATH} 失败"; return 1; }
            info "已更新脚本副本 ${SCRIPT_PATH} (版本 ${VERSION})"
        fi
    elif [[ -z "$src" ]] && { [[ -n "$force" ]] || [[ ! -f "$SCRIPT_PATH" ]]; }; then
        # 通过 curl | bash 等方式运行, 本地没有脚本文件, 从仓库下载.
        # 下载到的必须和当前运行的是同一个版本, 否则装进去的就不是用户实际跑的那份.
        info "从 ${SCRIPT_URL} 下载脚本到 ${SCRIPT_PATH}..."
        tmp=$(mktemp) || return 1
        if ! curl -fsSL --max-time 60 "$SCRIPT_URL" -o "$tmp"; then
            rm -f "$tmp"
            err "下载失败; 请先把脚本保存为文件再运行: curl -fsSL -o province_whitelist.sh ${SCRIPT_URL} && bash province_whitelist.sh install"
            return 1
        fi
        dl_ver=$(script_version_of "$tmp")
        if (( dl_ver != VERSION )); then
            rm -f "$tmp"
            err "仓库里的脚本版本(${dl_ver})与当前运行的(${VERSION})不一致, 不能确定该安装哪一份; 请先把当前脚本保存为文件再运行 install"
            return 1
        fi
        replace_script_with "$tmp" || { rm -f "$tmp"; err "写入 ${SCRIPT_PATH} 失败"; return 1; }
        rm -f "$tmp"
    fi
    [[ -f "$SCRIPT_PATH" ]] || { err "${SCRIPT_PATH} 不存在"; return 1; }
    chmod +x "$SCRIPT_PATH"
}

# $1 = 1 表示全新安装(写 cron); 重装时尊重用户之前 auto-update off 的选择
# 脚本副本在 cmd_install 开头就已同步好, 这里只写单元和 cron
install_persistence() {
    local fresh=${1:-1}
    ensure_service_unit
    if [[ "$fresh" == "1" || -f "$CRON_FILE" ]]; then
        write_cron
    fi
}

# 新版脚本直接运行时, 顺手把已安装的副本和 systemd 单元升级到当前版本.
# systemd 单元模板是跟脚本版本配套的: 副本没换(拒绝降级或复制失败)就不动单元.
upgrade_installed() {
    [[ $EUID -eq 0 ]] && is_installed || return 0
    if sync_script; then
        ensure_service_unit
    fi
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
    local ip=""
    [[ -n "${SSH_CLIENT:-}" ]] && ip=${SSH_CLIENT%% *}
    [[ -z "$ip" && -n "${SSH_CONNECTION:-}" ]] && ip=${SSH_CONNECTION%% *}
    if ! is_ipv4 "$ip"; then
        ip=$(who am i 2>/dev/null | awk '{print $NF}' | tr -d '()')
    fi
    is_ipv4 "$ip" && echo "$ip"
}

# 猜测: sshd 上只有一个已建立的连接时返回它的来源 IP.
# 这不一定是当前会话 (比如从控制台安装而别人正好在线), 所以调用方必须让用户确认, 不能直接加白.
guess_ssh_ip() {
    local peers
    peers=$(ss -tnH state established "( sport = :$(detect_ssh_port) )" 2>/dev/null \
        | awk '{print $4}' | sed -E 's/^\[::ffff:([0-9.]+)\]/\1/; s/:[0-9]+$//' | sort -u)
    [[ $(printf '%s\n' "$peers" | grep -c .) -eq 1 ]] && is_ipv4 "$peers" && echo "$peers"
}

cmd_install() {
    require_root
    install_deps

    local fresh=1 fw
    is_installed && fresh=0

    # 先把脚本副本放到位 (cron / systemd 要用). 放在最前面: 这一步失败就什么都不改,
    # 不会出现规则已生效、却没有开机恢复和自动更新的半安装状态
    sync_script force || exit 1

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
        # 空格、中文逗号都当分隔符, 合并重复分隔符, 去掉首尾分隔符 ("22 80" -> "22,80")
        ports=${ports//，/,}
        ports=${ports//[[:space:]]/,}
        while [[ "$ports" == *,,* ]]; do ports=${ports//,,/,}; done
        ports=${ports#,}; ports=${ports%,}
        validate_ports "$ports" || { err "端口格式错误: ${ports} (应为 1-65535 的数字, 逗号或空格分隔)"; exit 1; }
    else
        read -rp "确认保护全部端口? 不在白名单内的 IP 将无法访问本机任何服务 [y/N]: " ok
        [[ "$ok" =~ ^[Yy]$ ]] || exit 1
    fi

    # 防止把自己锁在门外: 把当前 SSH 来源 IP 加入额外白名单
    ensure_extra_set
    local myip guess
    myip=$(detect_ssh_ip)
    if [[ -n "$myip" ]]; then
        warn "当前 SSH 来源 IP 为 ${myip}, 将自动加入额外白名单, 避免误锁"
        ipset add "$IPSET_EXTRA" "$myip" -exist
    else
        guess=$(guess_ssh_ip)
        if [[ -n "$guess" ]]; then
            warn "无法确认当前会话的来源 IP (可能是经 sudo 或控制台运行), 但 sshd 上只有一个已建立的连接, 来自 ${guess}"
            read -rp "这是你自己的 IP 吗? 是则加入额外白名单 (不确定请选 N) [y/N]: " ok
            if [[ "$ok" =~ ^[Yy]$ ]]; then
                ipset add "$IPSET_EXTRA" "$guess" -exist && myip=$guess
            fi
        fi
        if [[ -z "$myip" ]]; then
            warn "未检测到 SSH 来源 IP, 若你的 IP 不在所选省份内, 应用后可能失联!"
            read -rp "确认继续? [y/N]: " ok
            [[ "$ok" =~ ^[Yy]$ ]] || exit 1
        fi
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

# 点分 IPv4 <-> 32 位整数. 每段都加 10# 前缀, 否则 010 会被 bash 当成八进制, 08 直接报错
ip_to_int() { local a b c d; IFS=. read -r a b c d <<< "$1"; echo $(( (10#$a<<24) | (10#$b<<16) | (10#$c<<8) | 10#$d )); }
int_to_ip() { echo "$(( ($1>>24) & 255 )).$(( ($1>>16) & 255 )).$(( ($1>>8) & 255 )).$(( $1 & 255 ))"; }

# 把 "IP", "IP/前缀" 或 "IP + 范围(b|c)" 统一成规范网段:
#   1.2.3.4        -> 1.2.3.4            (单个 IP)
#   1.2.3.4 c      -> 1.2.3.0/24         (C 段)
#   1.2.3.4 b      -> 1.2.0.0/16         (B 段)
#   1.2.3.4/20     -> 1.2.0.0/20         (自定义前缀, 主机位自动清零)
# 前缀只接受 1-32 (hash:net 存不了 /0). 返回: 0 成功; 2 前缀和 b/c 同时给了; 1 其他错误
parse_net() {
    local arg=$1 scope=${2:-} ip bits ipn mask
    ip=${arg%%/*}
    is_ipv4 "$ip" || return 1
    if [[ "$arg" == */* ]]; then
        # 已经写了前缀就不能再给 b/c, 两者冲突时宁可报错也不猜用户想要哪个
        [[ -z "$scope" ]] || return 2
        bits=${arg#*/}
        [[ "$bits" =~ ^[0-9]+$ ]] && (( 10#$bits >= 1 && 10#$bits <= 32 )) || return 1
        bits=$((10#$bits))
    else
        case "$scope" in
            b|B|16) bits=16 ;;
            c|C|24) bits=24 ;;
            ""|ip|32) bits=32 ;;
            *) return 1 ;;
        esac
    fi
    ipn=$(ip_to_int "$ip")
    mask=$(( (0xFFFFFFFF << (32 - bits)) & 0xFFFFFFFF ))
    if (( bits == 32 )); then
        int_to_ip "$ipn"   # 顺便把 010.0.0.1 之类规范成 10.0.0.1
    else
        echo "$(int_to_ip $(( ipn & mask )))/${bits}"
    fi
}

# add-ip / del-ip 共用的参数解析: 成功打印规范网段, 失败打印原因并返回 1
net_from_args() {
    local cmdname=$1 arg=${2:-} scope=${3:-} net rc
    [[ -n "$arg" ]] || { err "用法: $0 ${cmdname} <IP|IP/CIDR> [b|c]   (b = 整个 B 段 /16, c = 整个 C 段 /24)"; return 1; }
    net=$(parse_net "$arg" "$scope"); rc=$?
    case $rc in
        0) echo "$net" ;;
        2) err "${arg} 已带前缀, 不能再加 ${scope}; 要么写 IP 加 b/c, 要么直接写 IP/前缀"; return 1 ;;
        *) err "无效的 IP/网段: ${arg} ${scope} (前缀须在 1-32 之间; b/c 只能跟在不带前缀的 IP 后面)"; return 1 ;;
    esac
}

cmd_add_ip() {
    require_root
    local net
    net=$(net_from_args add-ip "${1:-}" "${2:-}") || exit 1
    if [[ "$net" == */* ]] && (( ${net#*/} < 16 )); then
        warn "前缀 /${net#*/} 比 B 段还大, 将放行 $(( 1 << (32 - ${net#*/}) )) 个地址, 请确认这是你要的"
    fi
    ensure_extra_set
    ipset add "$IPSET_EXTRA" "$net" -exist || exit 1
    info "已添加 ${net}"
    with_lock save_ipsets || warn "未能写入磁盘, 重启后 ${net} 会丢失"
}

cmd_del_ip() {
    require_root
    local net
    net=$(net_from_args del-ip "${1:-}" "${2:-}") || exit 1
    ipset_exists "$IPSET_EXTRA" || { err "额外白名单集合不存在, 没有可删除的条目"; exit 1; }
    ipset del "$IPSET_EXTRA" "$net" || exit 1
    info "已删除 ${net}"
    with_lock save_ipsets || warn "未能写入磁盘, 重启后 ${net} 会重新出现"
}

# 从一个 CIDR 列表文件里找出包含指定 IP 的网段, 一次 awk 扫描, 不依赖 ipset 也不逐行 fork.
# 判断方法: 把 IP 和网段都右移 (32-前缀) 位后比较, 用除法代替位运算以兼容 mawk.
cidrs_containing() {
    local ip=$1 file=$2
    # 正则走环境变量而不是 -v: -v 会处理反斜杠转义, gawk 会对每个 \. 打警告
    CIDR_RE="$CIDR_RE" awk -v ipn="$(ip_to_int "$ip")" '
        $1 ~ ENVIRON["CIDR_RE"] {
            split($1, a, "/"); split(a[1], o, ".")
            n = ((o[1]*256 + o[2])*256 + o[3])*256 + o[4]
            d = 2 ^ (32 - a[2])
            if (int(ipn / d) == int(n / d)) printf "%s ", $1
        }' "$file"
}

# 查某个 IP 当前是否放行, 以及数据源把它归到哪个省
# 省份文件并行下载, 然后逐个文件 awk 扫描; 下载失败的省份会单独列出, 不会被说成"不在数据源里"
cmd_check_ip() {
    require_root
    local ip=${1:-} hit=0 tmpdir i code found=() missing=() hits
    is_ipv4 "$ip" || { err "用法: $0 check-ip <IPv4 地址>"; exit 1; }
    ip=$(int_to_ip "$(ip_to_int "$ip")")   # 规范化, 去掉前导零

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

    info "在数据源中查找 ${ip} 的省份归属 (并行下载 ${#PROVINCE_CODES[@]} 个省份文件, 请稍候)..."
    tmpdir=$(mktemp -d)
    for code in "${PROVINCE_CODES[@]}"; do
        fetch_province "$code" "${tmpdir}/${code}.txt" &
    done
    wait

    for i in "${!PROVINCE_CODES[@]}"; do
        code=${PROVINCE_CODES[$i]}
        if [[ ! -s "${tmpdir}/${code}.txt" ]]; then
            missing+=("${PROVINCE_NAMES[$i]}(${code})")
            continue
        fi
        hits=$(cidrs_containing "$ip" "${tmpdir}/${code}.txt")
        [[ -n "$hits" ]] && found+=("${PROVINCE_NAMES[$i]}(${code}): ${hits}")
    done
    rm -rf "$tmpdir"

    [[ ${#missing[@]} -gt 0 ]] && warn "以下省份数据下载失败, 未参与判断: ${missing[*]}"
    if [[ ${#found[@]} -gt 0 ]]; then
        info "数据源归属: ${found[*]}"
        info "如归属与实际不符, 可放行该 IP 所在 B 段: $0 add-ip ${ip} b, 直接放行上面的网段: $0 add-ip <网段>, 或把该省加入白名单: $0 add-province <省份>"
    elif [[ ${#missing[@]} -gt 0 ]]; then
        warn "已下载的省份文件里没有 ${ip}, 但有省份数据缺失, 无法断定归属; 请稍后重试"
    else
        warn "数据源的 ${#PROVINCE_CODES[@]} 个省份文件里都没有 ${ip}; 如需放行请用: $0 add-ip ${ip} b   (放行其 B 段)"
    fi
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
    local ch summary auto_label pause_label enabled pnames c ip ok scope
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
                [[ -n "$ip" ]] || continue
                scope=""
                if [[ "$ip" != */* ]]; then
                    echo " 1) 只放行这个 IP"
                    echo " 2) 放行整个 C 段 /24 (256 个地址)"
                    echo " 3) 放行整个 B 段 /16 (65536 个地址, 适合家宽/手机等 IP 经常变的情况)"
                    read -rp "请选择 [1/2/3, 默认 1]: " scope
                    case "${scope:-1}" in 1) scope="" ;; 2) scope=c ;; 3) scope=b ;; *) err "无效选择"; continue ;; esac
                fi
                ( cmd_add_ip "$ip" "$scope" )
                ;;
            6)
                is_installed || { err "尚未安装, 请先选 1"; continue; }
                ipset list "$IPSET_EXTRA" 2>/dev/null | sed -n '/^Members:/,$p' | tail -n +2 | sed 's/^/   /'
                read -rp "输入要删除的 IP 或网段(按上面列出的原样输入): " ip
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

# 已安装的机器上用新版脚本跑管理命令时, 先把副本和 systemd 单元升级到当前版本
# (只对合法的管理命令触发; install 自己会同步, restore-rules/uninstall 和拼错的命令不碰)
case "$cmd" in
    menu|update|status|add-province|del-province|add-ip|del-ip|check-ip|auto-update|pause|resume)
        upgrade_installed ;;
esac

case "$cmd" in
    menu)           main_menu ;;
    install)        cmd_install ;;
    update)         cmd_update ;;
    status)         cmd_status ;;
    add-province)   cmd_add_province "$@" ;;
    del-province)   cmd_del_province "$@" ;;
    add-ip)         cmd_add_ip "${1:-}" "${2:-}" ;;
    del-ip)         cmd_del_ip "${1:-}" "${2:-}" ;;
    check-ip)       cmd_check_ip "${1:-}" ;;
    auto-update)    cmd_autoupdate "${1:-}" ;;
    pause)          cmd_pause ;;
    resume)         cmd_resume ;;
    restore-rules)  cmd_restore_rules ;;
    uninstall)      cmd_uninstall ;;
    *)
        echo "用法: $0                # 交互式管理菜单"
        echo "     $0 {install|update|status|add-province [省份]...|del-province [省份]...|add-ip <IP> [b|c]|del-ip <IP> [b|c]|check-ip <IP>|auto-update on|off|pause|resume|uninstall}"
        exit 1
        ;;
esac
