#!/bin/bash

# 双出口路由管理脚本 - 支持 IPv4 / IPv6 / 出口测速 / 自动恢复 / 策略路由
# 仅针对首周用户(因为只有首周才有 CN2 和 9929 双出口啦~)

# Author: 自动化CCB (chunkburst / tg: @auto_ccb)
# 感谢使用啦~

# ========== 我是分割线 ==========
# 产品食用说明:

# 如果您购买的是Promo机型，您会看到两个网口：
# eth0 - 10.7.x.x
# ens19 - 10.8.x.x
# eth0系9929出口，ens19系CN2出口. 

# 如果您是正价机型，您亦会看到两个网口：
# eth0 - 您原先的内部IP
# eht1 - 另加的内部IP
# 其中10.7.x.x系9929出口，10.8.x.x系CN2出口.

# ========== 配置区域 ==========

# 具体情况（_SRC和_NET）以 ip addr show 命令运行出来的为准

CN2_IF="eth1" #如果是ens19就写ens19
CN2_GW="10.8.0.1" #gateway(网关地址)
CN2_SRC="10.8.3.255" #这里填你的eth1 / ens19的源IP 
CN2_NET="10.8.0.204/22"  #这里填你的eth1 / ens19的网段(应该是/22)

NET9929_IF="eth0" #无需更改
NET9929_GW="10.7.0.1" #网关地址
NET9929_SRC="10.7.1.255" #这里填你的eth0的源IP
NET9929_NET="10.7.1.255/23" #这里填你的eth0的网段(一般应该大概是/23)

ROUTE_LIST="/etc/custom-routes.list"

# IP隐藏配置（true=隐藏IP最后两段，false=显示完整IP）
IS_HIDDEN=true


# ==============================

CYAN="\033[0;36m"
GREEN="\033[0;32m"
YELLOW="\033[1;33m"
BLUE="\033[0;34m"
NC="\033[0m"

# 判断是否为 IPv6
is_ipv6() {
    [[ "$1" =~ : ]]
}

# 判断合法 IPv4
is_valid_ipv4() {
    [[ "$1" =~ ^([0-9]{1,3}\.){3}[0-9]{1,3}$ ]] && \
    awk -F. '{for(i=1;i<=4;i++) if($i>255) exit 1}' <<< "$1"
}

# 判断合法 IPv6
is_valid_ipv6() {
    [[ "$1" =~ ^([0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}$ ]]
}

# 检查是否为域名
is_domain() {
    [[ "$1" =~ ^[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}$ ]]
}

# 解析域名为IP（IPv4/IPv6都兼容）
resolve_domain() {
    local DOMAIN="$1"
    local IP=""
    # 优先解析IPv4
    IP=$(getent ahosts "$DOMAIN" | awk '/STREAM/ && !/:/ {print $1; exit}')
    if [[ -n "$IP" ]]; then
        echo "$IP"
        return
    fi
    # 再解析IPv6
    IP=$(getent ahosts "$DOMAIN" | awk '/STREAM/ && /:/ {print $1; exit}')
    if [[ -n "$IP" ]]; then
        echo "$IP"
        return
    fi
    # fallback dig (优先A/AAAA)
    IP=$(dig +short "$DOMAIN" A | head -n1)
    if [[ -n "$IP" ]]; then
        echo "$IP"
        return
    fi
    IP=$(dig +short "$DOMAIN" AAAA | head -n1)
    if [[ -n "$IP" ]]; then
        echo "$IP"
        return
    fi
    echo ""
}

# 隐藏IP函数
hide_ip() {
    local IP="$1"
    if [[ "$IS_HIDDEN" == "true" && "$IP" != "N/A" ]]; then
        if [[ "$IP" =~ : ]]; then
            # IPv6 - 隐藏后4段
            echo "$IP" | sed -E 's/:[0-9a-fA-F]*:[0-9a-fA-F]*:[0-9a-fA-F]*:[0-9a-fA-F]*$/:*:*:*:*/'
        else
            # IPv4 - 隐藏后2段
            echo "$IP" | sed -E 's/\.[0-9]+\.[0-9]+$/.*.*/g'
        fi
    else
        echo "$IP"
    fi
}

# 自动安装bc模块
install_bc() {
    if command -v apt-get &>/dev/null; then
        # Debian/Ubuntu
        apt-get update >/dev/null 2>&1 && apt-get install -y bc >/dev/null 2>&1
    elif command -v yum &>/dev/null; then
        # CentOS/RHEL 7
        yum install -y bc >/dev/null 2>&1
    elif command -v dnf &>/dev/null; then
        # CentOS/RHEL 8+/Fedora
        dnf install -y bc >/dev/null 2>&1
    elif command -v zypper &>/dev/null; then
        # openSUSE
        zypper install -y bc >/dev/null 2>&1
    elif command -v pacman &>/dev/null; then
        # Arch Linux
        pacman -S --noconfirm bc >/dev/null 2>&1
    elif command -v apk &>/dev/null; then
        # Alpine Linux
        apk add --no-cache bc >/dev/null 2>&1
    else
        return 1
    fi
    return $?
}

get_exit_ip() {
    local IFACE_NAME="$1" TARGET_TYPE="$2"
    local IP RETRY_COUNT=0 MAX_RETRIES=2

    if [[ "$TARGET_TYPE" == "ipv6" ]]; then
        local URLS=("https://v6.ip.sb" "https://ipv6.icanhazip.com")
    else
        local URLS=("http://ip.sb" "https://ipv4.icanhazip.com")
    fi

    # 重试机制：每个URL最多重试2次
    while [[ $RETRY_COUNT -le $MAX_RETRIES ]]; do
        for URL in "${URLS[@]}"; do
            local CURL_OPTS="-s --interface $IFACE_NAME --connect-timeout 10 --max-time 15"
            if [[ "$TARGET_TYPE" == "ipv6" ]]; then
                CURL_OPTS+=" -6"
            else
                CURL_OPTS+=" -4"
            fi
            IP=$(curl $CURL_OPTS "$URL" 2>/dev/null)
            
            if [[ "$TARGET_TYPE" == "ipv6" ]]; then
                [[ "$IP" =~ ^[0-9a-fA-F:]+$ ]] && { echo "$IP"; return; }
            else
                [[ "$IP" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] && { echo "$IP"; return; }
            fi
        done
        ((RETRY_COUNT++))
        [[ $RETRY_COUNT -le $MAX_RETRIES ]] && sleep 1
    done
    
    echo "N/A"
}

# 单线路测速
test_single_exit() {
    local NAME="$1" IFACE="$2" SRC="$3" TARGET="$4"
    local OUT_IP PING_RESULT AVG_RTT PACKET_LOSS TARGET_TYPE

    if is_ipv6 "$TARGET"; then
        TARGET_TYPE="ipv6"
        PING_CMD="ping -6 -c 3 -I $IFACE"
    else
        TARGET_TYPE="ipv4"
        PING_CMD="ping -4 -c 3 -I $SRC"
    fi

    OUT_IP=$(get_exit_ip "$IFACE" "$TARGET_TYPE")
    PING_RESULT=$($PING_CMD "$TARGET" 2>/dev/null)

    PACKET_LOSS=$(echo "$PING_RESULT" | grep -Eo '[0-9]+% packet loss' | cut -d'%' -f1)
    if [[ "$PACKET_LOSS" == "100" || -z "$PING_RESULT" ]]; then
        AVG_RTT="fail"
    else
        AVG_RTT=$(echo "$PING_RESULT" | awk -F'/' 'END{if(NR>0) print $5; else print "fail"}')
        [[ -z "$AVG_RTT" ]] && AVG_RTT="fail"
    fi

    {
        echo -e "${BLUE}-- $NAME 测试 --${NC}"
        echo -e "出口 IP  : $(hide_ip "$OUT_IP")"
        if [[ "$AVG_RTT" == "fail" ]]; then
            echo -e "平均延迟 : $AVG_RTT"
        else
            echo -e "平均延迟 : ${AVG_RTT}ms"
        fi
        echo
    } >&2

    echo "$AVG_RTT"
}

# 出口测速核心逻辑
run_speed_test() {
    local TARGET="$1"
    if ! command -v bc &>/dev/null; then
        echo -e "${YELLOW}[*] 正在安装 bc 计算工具...${NC}"
        if ! install_bc; then
            echo -e "${CYAN}[-] 无法自动安装 bc...${NC}"
            exit 1
        fi
        echo -e "${GREEN}[+] bc 安装完成${NC}"
    fi
    echo -e "${YELLOW}[*] 正在测试目标: $TARGET${NC}"
    echo
    DELAY1=$(test_single_exit "9929（eth0）" "$NET9929_IF" "$NET9929_SRC" "$TARGET")
    DELAY2=$(test_single_exit "CN2（eth1）"  "$CN2_IF"     "$CN2_SRC"     "$TARGET")
    echo -e "${BLUE}推荐线路：${NC}"
    if [[ "$DELAY1" == "fail" && "$DELAY2" == "fail" ]]; then
        echo -e "${CYAN}两个出口都测试失败${NC}"
    elif [[ "$DELAY1" == "fail" ]]; then
        echo -e "${CYAN}→ CN2（eth1） ←（9929 无响应）${NC}"
    elif [[ "$DELAY2" == "fail" ]]; then
        echo -e "${GREEN}→ 9929（eth0） ←（CN2 无响应）${NC}"
    elif (( $(echo "$DELAY1 < $DELAY2" | bc -l) )); then
        echo -e "${GREEN}→ 9929（eth0） ← 更低延迟（${DELAY1}ms vs ${DELAY2}ms）${NC}"
    else
        echo -e "${CYAN}→ CN2（eth1） ← 更低延迟（${DELAY2}ms vs ${DELAY1}ms）${NC}"
    fi
    echo
    read -p "按 Enter 键返回主菜单..."
}

# 公共测速菜单
speed_test_public() {
    echo
    echo -e "${BLUE}[*] 选择一个公共测试目标:${NC}"
    echo "1) 8.8.8.8      (Google DNS)"
    echo "2) 1.1.1.1      (Cloudflare DNS)"
    echo "3) 2001:4860:4860::8888 (Google IPv6)"
    echo "0) 返回主菜单"
    echo
    read -p ">> 请选择测试地址 [默认1]: " CHOICE
    CHOICE=${CHOICE:-1}
    case "$CHOICE" in
        1) run_speed_test "8.8.8.8" ;;
        2) run_speed_test "1.1.1.1" ;;
        3) run_speed_test "2001:4860:4860::8888" ;;
        0) return ;;
        *) echo -e "${CYAN}[-] 无效选择${NC}"; sleep 1 ;;
    esac
}
# 自定义测速菜单（支持域名输入）
speed_test_custom() {
    read -p "[*] 请输入要测试的目标 IP（IPv4 / IPv6 / 域名）: " TARGET
    if [[ -z "$TARGET" ]]; then
        echo -e "${CYAN}[-] 目标不能为空${NC}"; echo; read -p "按 Enter..."; return
    fi
    # 支持域名测试
    if is_domain "$TARGET"; then
        IP=$(resolve_domain "$TARGET")
        if [[ -z "$IP" ]]; then
            echo -e "${CYAN}[-] 域名解析失败${NC}"; echo; read -p "按 Enter..."; return
        fi
        TARGET="$IP"
        echo -e "${YELLOW}[*] 域名已解析: $TARGET${NC}"
    fi
    if is_ipv6 "$TARGET"; then
        is_valid_ipv6 "$TARGET" || { echo -e "${CYAN}[-] IPv6 地址不合法${NC}"; echo; read -p "按 Enter..."; return; }
    else
        is_valid_ipv4 "$TARGET" || { echo -e "${CYAN}[-] IPv4 地址不合法${NC}"; echo; read -p "按 Enter..."; return; }
    fi
    run_speed_test "$TARGET"
}

# 添加路由（支持域名输入）
add_route() {
    read -p "[*] 请输入目标 IP 地址或域名: " TARGET_IP
    if [[ -z "$TARGET_IP" ]]; then
        echo -e "${CYAN}[-] IP/域名不能为空${NC}"; echo; read -p "按 Enter..."; return
    fi
    # 支持域名输入
    if is_domain "$TARGET_IP"; then
        IP=$(resolve_domain "$TARGET_IP")
        if [[ -z "$IP" ]]; then
            echo -e "${CYAN}[-] 域名解析失败${NC}"; echo; read -p "按 Enter..."; return
        fi
        TARGET_IP="$IP"
        echo -e "${YELLOW}[*] 域名已解析: $TARGET_IP${NC}"
    fi
    if is_ipv6 "$TARGET_IP"; then
        is_valid_ipv6 "$TARGET_IP" || { echo -e "${CYAN}[-] 非法的 IPv6 地址${NC}"; echo; read -p "按 Enter..."; return; }
    else
        is_valid_ipv4 "$TARGET_IP" || { echo -e "${CYAN}[-] 非法的 IPv4 地址${NC}"; echo; read -p "按 Enter..."; return; }
    fi
    echo -e "${BLUE}[*] 请选择出口线路:${NC}"
    echo "   1) 9929（联通）"
    echo "   2) CN2（电信）"
    read -p ">> 请选择 [1-2]: " CHOICE
    sed -i "/^$TARGET_IP /d" "$ROUTE_LIST"
    if is_ipv6 "$TARGET_IP"; then
        case "$CHOICE" in
            1) ip -6 route replace "$TARGET_IP/128" dev "$NET9929_IF"; echo "$TARGET_IP via-9929-v6" >> "$ROUTE_LIST"; echo -e "${GREEN}[+] 已添加 IPv6 9929 路由${NC}" ;;
            2) ip -6 route replace "$TARGET_IP/128" dev "$CN2_IF"; echo "$TARGET_IP via-cn2-v6" >> "$ROUTE_LIST"; echo -e "${CYAN}[+] 已添加 IPv6 CN2 路由${NC}" ;;
        esac
    else
        case "$CHOICE" in
            1) ip route replace "$TARGET_IP/32" via "$NET9929_GW" dev "$NET9929_IF" src "$NET9929_SRC"; echo "$TARGET_IP via-9929" >> "$ROUTE_LIST"; echo -e "${GREEN}[+] 已添加 IPv4 9929 路由${NC}" ;;
            2) ip route replace "$TARGET_IP/32" via "$CN2_GW" dev "$CN2_IF" src "$CN2_SRC"; echo "$TARGET_IP via-cn2" >> "$ROUTE_LIST"; echo -e "${CYAN}[+] 已添加 IPv4 CN2 路由${NC}" ;;
        esac
    fi
    echo; read -p "按 Enter 键返回主菜单..."
}

# 删除路由
delete_route() {
    echo -e "${BLUE}[*] 当前静态路由列表:${NC}"
    if [[ ! -f "$ROUTE_LIST" || ! -s "$ROUTE_LIST" ]]; then
        echo "(暂无记录)"; echo; read -p "按 Enter..."; return
    fi
    mapfile -t ROUTES < "$ROUTE_LIST"
    for i in "${!ROUTES[@]}"; do
        INDEX=$((i+1)); IP=$(echo "${ROUTES[$i]}" | awk '{print $1}'); TAG=$(echo "${ROUTES[$i]}" | awk '{print $2}'); COLOR="${NC}"
        [[ "$TAG" =~ cn2 ]] && COLOR="${CYAN}"; [[ "$TAG" =~ 9929 ]] && COLOR="${GREEN}"
        echo -e "$INDEX) $IP ${COLOR}$TAG${NC}"
    done
    echo
    read -p ">> 请输入要删除的编号（或输入 m 手动模式）: " CHOICE
    if [[ "$CHOICE" == "m" ]]; then
        read -p "[*] 请输入要删除的目标 IP: " TARGET_IP
        if [[ -z "$TARGET_IP" ]]; then echo -e "${CYAN}[-] IP 不能为空${NC}"; echo; read -p "按 Enter..."; return; fi
        if is_ipv6 "$TARGET_IP"; then ip -6 route delete "$TARGET_IP/128" 2>/dev/null; else ip route delete "$TARGET_IP/32" 2>/dev/null; fi
        sed -i "/^$TARGET_IP /d" "$ROUTE_LIST"; echo -e "${GREEN}[+] 已删除 $TARGET_IP 的路由${NC}"; echo; read -p "按 Enter..."; return
    fi
    if ! [[ "$CHOICE" =~ ^[0-9]+$ ]] || [ "$CHOICE" -lt 1 ] || [ "$CHOICE" -gt "${#ROUTES[@]}" ]; then
        echo -e "${CYAN}[-] 输入无效编号${NC}"; echo; read -p "按 Enter..."; return
    fi
    INDEX=$((CHOICE-1)); ENTRY="${ROUTES[$INDEX]}"; IP=$(echo "$ENTRY" | awk '{print $1}'); TYPE=$(echo "$ENTRY" | awk '{print $2}')
    [[ "$TYPE" == *v6 ]] && ip -6 route delete "$IP/128" 2>/dev/null || ip route delete "$IP/32" 2>/dev/null
    sed -i "/^$IP /d" "$ROUTE_LIST"; echo -e "${GREEN}[+] 已删除 $IP 的路由${NC}"; echo; read -p "按 Enter..."
}

# 查看路由
list_routes() {
    echo -e "${BLUE}[*] 当前静态路由列表:${NC}"
    if [[ -f "$ROUTE_LIST" && -s "$ROUTE_LIST" ]]; then
        while read -r line; do
            IP=$(echo "$line" | awk '{print $1}'); TAG=$(echo "$line" | awk '{print $2}')
            case "$TAG" in
                via-cn2|via-cn2-v6) echo -e "$IP ${CYAN}$TAG${NC}" ;;
                via-9929|via-9929-v6) echo -e "$IP ${GREEN}$TAG${NC}" ;;
                *) echo "$line" ;;
            esac
        done < "$ROUTE_LIST"
    else
        echo "(暂无记录)"
    fi
    echo; read -p "按 Enter 返回主菜单..."
}

# 恢复路由
restore_routes() {
    if [[ ! -f "$ROUTE_LIST" ]]; then echo -e "${YELLOW}[!] 没有路由记录可恢复${NC}"; echo; read -p "按 Enter..."; return; fi
    echo -e "${BLUE}[*] 正在恢复静态路由...${NC}"
    while read -r line; do
        IP=$(echo "$line" | awk '{print $1}'); TYPE=$(echo "$line" | awk '{print $2}')
        case "$TYPE" in
            via-9929) ip route replace "$IP/32" via "$NET9929_GW" dev "$NET9929_IF" src "$NET9929_SRC" ;;
            via-cn2) ip route replace "$IP/32" via "$CN2_GW" dev "$CN2_IF" src "$CN2_SRC" ;;
            via-9929-v6) ip -6 route replace "$IP/128" dev "$NET9929_IF" ;;
            via-cn2-v6) ip -6 route replace "$IP/128" dev "$CN2_IF" ;;
        esac
    done < "$ROUTE_LIST"
    echo -e "${GREEN}[+] 路由恢复完成${NC}"; echo; read -p "按 Enter..."
}

# 清空所有路由
clear_all_routes() {
    echo -e "${YELLOW}[!] 警告：此操作将删除所有已配置的静态路由。${NC}"
    read -p "你确定要继续吗？[y/N]: " CONFIRM
    if [[ "${CONFIRM,,}" != "y" ]]; then echo -e "${CYAN}[-] 操作已取消。${NC}"; echo; read -p "按 Enter..."; return; fi
    if [[ ! -f "$ROUTE_LIST" || ! -s "$ROUTE_LIST" ]]; then echo -e "${GREEN}[+] 没有路由记录可清除。${NC}"; echo; read -p "按 Enter..."; return; fi
    echo -e "${BLUE}[*] 正在清除所有静态路由...${NC}"
    while read -r line; do
        IP=$(echo "$line" | awk '{print $1}'); TYPE=$(echo "$line" | awk '{print $2}')
        if [[ "$TYPE" == *v6 ]]; then ip -6 route delete "$IP/128" 2>/dev/null; else ip route delete "$IP/32" 2>/dev/null; fi
        echo "  - 已删除路由: $IP"
    done < "$ROUTE_LIST"
    > "$ROUTE_LIST"
    echo -e "${GREEN}[+] 所有静态路由已成功清除。${NC}"; echo; read -p "按 Enter..."
}

# systemd 启动项
enable_custom_routes_autostart() {
    cat > /etc/systemd/system/custom-routes.service <<EOF
[Unit]
Description=恢复自定义静态路由
After=network.target
[Service]
Type=oneshot
ExecStart=/bin/bash $(realpath $0) --restore
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reexec; systemctl enable custom-routes.service
    echo -e "${GREEN}[+] 已设置开机自动恢复自定义路由。${NC}"
    echo -e "${YELLOW}    此设置将在重启后，恢复您在路由列表中添加的所有路由。${NC}"
    echo; read -p "按 Enter..."
}

# 配置策略路由
setup_policy_routing() {
    echo -e "${BLUE}[*] 正在配置策略路由...${NC}"
    
    # 确保路由表存在
    if ! grep -q "eth0_table" /etc/iproute2/rt_tables; then
        echo "100 eth0_table" >> /etc/iproute2/rt_tables
        echo -e "${YELLOW}[+] 已创建 eth0_table 路由表${NC}"
    fi
    
    if ! grep -q "eth1_table" /etc/iproute2/rt_tables; then
        echo "101 eth1_table" >> /etc/iproute2/rt_tables
        echo -e "${YELLOW}[+] 已创建 eth1_table 路由表${NC}"
    fi
    
    # 刷新路由表缓存
    ip route flush cache
    
    # 添加路由规则前先检查表是否准备好
    echo -e "${YELLOW}[*] 添加路由规则到自定义表...${NC}"
    
    # 设置eth0路由表
    ip route add "$NET9929_NET" dev "$NET9929_IF" src "$NET9929_SRC" table eth0_table 2>/dev/null || true
    ip route add default via "$NET9929_GW" dev "$NET9929_IF" table eth0_table 2>/dev/null || true
    
    # 设置eth1路由表
    ip route add "$CN2_NET" dev "$CN2_IF" src "$CN2_SRC" table eth1_table 2>/dev/null || true
    ip route add default via "$CN2_GW" dev "$CN2_IF" table eth1_table 2>/dev/null || true
    
    # 添加规则
    ip rule del from "$NET9929_SRC" table eth0_table priority 100 2>/dev/null || true
    ip rule add from "$NET9929_SRC" table eth0_table priority 100
    
    ip rule del from "$CN2_SRC" table eth1_table priority 101 2>/dev/null || true
    ip rule add from "$CN2_SRC" table eth1_table priority 101
    
    echo -e "${GREEN}[+] 策略路由配置完成。${NC}"
}

# 永久化策略路由
persist_policy_routing() {
    cat > /etc/systemd/system/policy-routing.service <<EOF
[Unit]
Description=应用双网卡策略路由
After=network.target network-online.target
Wants=network-online.target
[Service]
Type=oneshot
ExecStart=/bin/bash $(realpath $0) --setup-policy
RemainAfterExit=yes
[Install]
WantedBy=multi-user.target
EOF
    systemctl daemon-reexec
    systemctl enable policy-routing.service
    echo -e "${GREEN}[+] 已设置开机自动应用策略路由。${NC}"
    echo -e "${YELLOW}    你可以通过 'systemctl status policy-routing.service' 查看状态。${NC}"
}

# 关闭自动化设置
disable_custom_routes_autostart() {
    systemctl disable custom-routes.service 2>/dev/null
    rm -f /etc/systemd/system/custom-routes.service
    echo -e "${GREEN}[+] 已关闭自定义路由的开机自启。${NC}"
}

disable_policy_routing_autostart() {
    systemctl disable policy-routing.service 2>/dev/null
    rm -f /etc/systemd/system/policy-routing.service
    echo -e "${GREEN}[+] 已关闭策略路由的开机自启。${NC}"
}

disable_autostart_menu() {
    while true; do
        clear
        echo -e "${YELLOW}--- 关闭自动化设置 ---${NC}"
        echo "1) 关闭自定义路由的开机自启"
        echo "2) 关闭策略路由的开机自启"
        echo "0) 返回主菜单"
        echo
        read -p ">> 请选择 [0-2]: " CHOICE
        case "$CHOICE" in
            1) disable_custom_routes_autostart; read -p "按 Enter..."; break ;;
            2) disable_policy_routing_autostart; read -p "按 Enter..."; break ;;
            0) break ;;
            *) echo -e "${CYAN}[-] 无效选择${NC}"; sleep 1 ;;
        esac
    done
}

# 主菜单
main_menu() {
    while true; do
        clear
        echo -e "${YELLOW}==============================="
        echo -e "[ 嘻嘻比双口UI ]"
        echo -e "===============================${NC}"
        echo "1) 添加目标 IP 路由"
        echo "2) 删除指定 IP 路由"
        echo "3) 查看当前路由"
        echo "4) 立即恢复所有路由"
        echo "5) 出口测速（公共）"
        echo "6) 出口测速（自定义）"
        echo
        echo -e "${BLUE}--- 自动化设置 ---${NC}"
        echo "7) 启用-自定义路由开机恢复"
        echo "8) 启用-策略路由开机加载"
        echo "9) 关闭自动化设置"
        echo
        echo "0) 退出"
        echo -e "${YELLOW}===============================${NC}"
        read -p ">> 请选择操作 [0-9]: " OPT
        case "$OPT" in
            1) add_route ;;
            2) delete_route ;;
            3) list_routes ;;
            4) restore_routes ;;
            5) speed_test_public ;;
            6) speed_test_custom ;;
            7) enable_custom_routes_autostart ;;
            8) setup_policy_routing; persist_policy_routing; echo; read -p "按 Enter..." ;;
            9) disable_autostart_menu ;;
            0) exit 0 ;;
            *) echo -e "${CYAN}[-] 无效输入，请重新选择${NC}" ;;
        esac
    done
}

# systemd 启动模式
case "$1" in
    --restore)
        restore_routes
        exit 0
        ;;
    --setup-policy)
        setup_policy_routing
        exit 0
        ;;
esac

main_menu
