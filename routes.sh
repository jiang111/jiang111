#!/bin/bash

ROUTE_LIST="/usr/local/etc/routes.list"
SYSTEMD_SERVICE="/etc/systemd/system/routes.service"
SERVICE_NAME="routes.service"

# 颜色定义
GREEN="\033[0;32m"
RED="\033[0;31m"
YELLOW="\033[0;33m"
NC="\033[0m" # 恢复默认颜色

function print_systemd_status() {
    echo -e "${YELLOW}====== Systemd 服务状态 ======${NC}"
    if [[ -f "$SYSTEMD_SERVICE" ]]; then
        echo -e "${GREEN}$SERVICE_NAME 已初始化${NC}（服务文件存在）"
        
        if systemctl is-enabled $SERVICE_NAME &>/dev/null; then
            echo -e "${GREEN}$SERVICE_NAME 已启用${NC}"
        else
            echo -e "${RED}$SERVICE_NAME 未启用${NC}"
        fi
        
        if systemctl is-active $SERVICE_NAME &>/dev/null; then
            echo -e "${GREEN}$SERVICE_NAME 已启动${NC}"
        else
            echo -e "${RED}$SERVICE_NAME 未启动${NC}"
        fi
    else
        echo -e "${RED}$SERVICE_NAME 未初始化${NC}（服务文件不存在）"
    fi
    echo -e "${YELLOW}=============================${NC}"
}

function init_systemd() {
    echo "正在初始化 systemd 服务..."
    
    # 确保路由列表文件的目录存在
    mkdir -p "$(dirname "$ROUTE_LIST")"
    
    cat << EOF > $SYSTEMD_SERVICE
[Unit]
Description=Custom Static Routes Setup
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
ExecStart=$0 apply
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    systemctl enable $SERVICE_NAME
    echo -e "${GREEN}✓ Systemd 服务已初始化并启用，重启后自动应用所有静态路由。${NC}"
}




function check_nexttrace() {
    if ! command -v nexttrace &>/dev/null; then
        echo -e "${YELLOW}nexttrace 未安装，正在自动安装...${NC}"
        curl -sL nxtrace.org/nt | bash
        if ! command -v nexttrace &>/dev/null; then
            echo -e "${RED}❌ nexttrace 安装失败。${NC}"
            return 1
        fi
        echo -e "${GREEN}✓ nexttrace 安装成功${NC}"
    fi
    return 0
}



function trace_route() {
    echo -e "${YELLOW}=== 路由追踪 ===${NC}"
    
    # 检查并安装 nexttrace
    if ! check_nexttrace; then
        return 1
    fi
    
    read -p "请输入目标域名或IP: " TARGET
    
    if [[ -z "$TARGET" ]]; then
        echo -e "${RED}❌ 输入不能为空${NC}"
        return 1
    fi
    
    IP=$(getent ahosts "$TARGET" | grep -m1 "STREAM" | awk '{print $1}')
    if [[ -z $IP ]]; then
        echo -e "${RED}❌ 无法解析域名或IP: $TARGET${NC}"
        return 1
    fi
    echo -e "${GREEN}目标IP为: $IP${NC}"
    
    echo -e "\n${YELLOW}请选择要使用的网卡进行追踪：${NC}"
    echo "1) eth0"
    echo "2) eth1"
    echo "3) 两个网卡都追踪"
    echo "4) 取消追踪"
    
    read -p "请输入选择 [1-4]: " CHOICE
    
    case $CHOICE in
        1)
            echo -e "\n${YELLOW}=== eth0 路由追踪结果 ===${NC}"
            # 配置路由
            ip route add $IP/32 via 10.7.0.1 dev eth0 2>/dev/null
            nexttrace "$TARGET"
            # 删除路由
            ip route del $IP/32 via 10.7.0.1 dev eth0 2>/dev/null
            apply_routes
            ;;
        2)
            echo -e "\n${YELLOW}=== eth1 路由追踪结果 ===${NC}"
            # 配置路由
            ip route add $IP/32 via 10.8.0.1 dev eth1 2>/dev/null
            nexttrace "$TARGET"
            # 删除路由
            ip route del $IP/32 via 10.8.0.1 dev eth1 2>/dev/null
            apply_routes
            ;;
        3)
            echo -e "\n${YELLOW}=== eth0 路由追踪结果 ===${NC}"
            # 配置 eth0 路由
            ip route add $IP/32 via 10.7.0.1 dev eth0 2>/dev/null
            nexttrace "$TARGET"
            # 删除 eth0 路由
            ip route del $IP/32 via 10.7.0.1 dev eth0 2>/dev/null

            sleep 1
                
            echo -e "\n${YELLOW}=== eth1 路由追踪结果 ===${NC}"
            # 配置 eth1 路由
            ip route add $IP/32 via 10.8.0.1 dev eth1 2>/dev/null
            nexttrace "$TARGET"
            # 删除 eth1 路由
            ip route del $IP/32 via 10.8.0.1 dev eth1 2>/dev/null
            apply_routes
            ;;
        4)
            echo -e "${YELLOW}已取消路由追踪${NC}"
            return 0
            ;;
        *)
            echo -e "${RED}❌ 无效选择${NC}"
            return 1
            ;;
    esac
}

function add_route() {
    echo -e "${YELLOW}=== 添加路由 ===${NC}"
    read -p "请输入域名或IP: " TARGET
    
    if [[ -z "$TARGET" ]]; then
        echo -e "${RED}❌ 输入不能为空${NC}"
        return 1
    fi
    
    IP=$(getent ahosts "$TARGET" | grep -m1 "STREAM" | awk '{print $1}')
    if [[ -z $IP ]]; then
        echo -e "${RED}❌ 无法解析域名或IP: $TARGET${NC}"
        return 1
    fi
    echo -e "${GREEN}目标IP为: $IP${NC}"

    # 测试 eth0 延迟
    echo -e "${YELLOW}正在测试 eth0 延迟...${NC}"
    ip route add $IP/32 via 10.7.0.1 dev eth0 2>/dev/null
    PING0=$(ping -c 3 -w 5 -I eth0 "$IP" 2>/dev/null | awk -F'/' '/rtt/ {print $5}')
    [[ -z $PING0 ]] && PING0=9999
    echo -e "${GREEN}eth0 rtt: $PING0 ms${NC}"
    ip route del $IP/32 via 10.7.0.1 dev eth0 2>/dev/null

    sleep 1

    # 测试 eth1 延迟
    echo -e "${YELLOW}正在测试 eth1 延迟...${NC}"
    ip route add $IP/32 via 10.8.0.1 dev eth1 2>/dev/null
    PING1=$(ping -c 3 -w 5 -I eth1 "$IP" 2>/dev/null | awk -F'/' '/rtt/ {print $5}')
    [[ -z $PING1 ]] && PING1=9999
    echo -e "${GREEN}eth1 rtt: $PING1 ms${NC}"
    ip route del $IP/32 via 10.8.0.1 dev eth1 2>/dev/null

    # 显示测试结果和建议 (使用 awk 替代 bc)
    echo -e "\n${YELLOW}=== 延迟测试结果 ===${NC}"
    echo -e "eth0: ${GREEN}$PING0 ms${NC} (via 10.7.0.1)"
    echo -e "eth1: ${GREEN}$PING1 ms${NC} (via 10.8.0.1)"
    
    # 使用 awk 进行浮点数比较
    if awk "BEGIN {exit !($PING0 > $PING1)}"; then
        echo -e "${YELLOW}建议：${GREEN}eth1 延迟较低${NC}"
    else
        echo -e "${YELLOW}建议：${GREEN}eth0 延迟较低${NC}"
    fi

    # 让用户选择网卡
    echo -e "\n${YELLOW}请选择要使用的网卡：${NC}"
    echo "1) eth0 (via 10.7.0.1)"
    echo "2) eth1 (via 10.8.0.1)"
    echo "3) 取消添加"
    
    while true; do
        read -p "请输入选择 [1-3]: " CHOICE
        case $CHOICE in
            1)
                BEST_IF="eth0"
                GATEWAY="10.7.0.1"
                break
                ;;
            2)
                BEST_IF="eth1"
                GATEWAY="10.8.0.1"
                break
                ;;
            3)
                echo -e "${YELLOW}已取消添加路由${NC}"
                return 0
                ;;
            *)
                echo -e "${RED}❌ 无效选择，请输入 1-3${NC}"
                ;;
        esac
    done

    ROUTE_CMD="ip route add $IP/32 via $GATEWAY dev $BEST_IF"
    
    # 检查路由是否已存在
    if grep -q "^$ROUTE_CMD$" "$ROUTE_LIST" 2>/dev/null; then
        echo -e "${YELLOW}⚠️  路由已存在，无需重复添加。${NC}"
        return 0
    fi
    
    # 添加到路由列表并立即生效
    echo "$ROUTE_CMD" >> "$ROUTE_LIST"
    if $ROUTE_CMD; then
        echo -e "${GREEN}✓ 路由已添加并立即生效：$ROUTE_CMD${NC}"
    else
        echo -e "${RED}❌ 路由添加失败${NC}"
    fi
}

function delete_route() {
    echo -e "${YELLOW}=== 删除路由 ===${NC}"
    
    # 显示当前路由列表
    if [[ -f "$ROUTE_LIST" && -s "$ROUTE_LIST" ]]; then
        echo "当前已添加的路由："
        cat -n "$ROUTE_LIST"
        echo ""
    else
        echo -e "${YELLOW}⚠️  当前没有已添加的路由。${NC}"
        return 0
    fi
    
    read -p "请输入要删除的域名或IP: " TARGET
    
    if [[ -z "$TARGET" ]]; then
        echo -e "${RED}❌ 输入不能为空${NC}"
        return 1
    fi
    
    IP=$(getent ahosts "$TARGET" | grep -m1 "STREAM" | awk '{print $1}')
    if [[ -z $IP ]]; then
        echo -e "${RED}❌ 无法解析域名或IP: $TARGET${NC}"
        return 1
    fi
    
    MATCH=$(grep "$IP/32" "$ROUTE_LIST" 2>/dev/null)
    if [[ -z $MATCH ]]; then
        echo -e "${RED}❌ 未找到对应的路由。${NC}"
        return 1
    fi
    
    CMD=$(echo "$MATCH" | sed 's/add/del/')
    echo "正在删除路由：$CMD"
    if $CMD; then
        echo -e "${GREEN}✓ 路由已从系统中移除${NC}"
    else
        echo -e "${YELLOW}⚠️  路由可能已不存在于系统中${NC}"
    fi
    
    # 从列表文件中移除
    grep -v "$IP/32" "$ROUTE_LIST" > "$ROUTE_LIST.tmp" && mv "$ROUTE_LIST.tmp" "$ROUTE_LIST"
    echo -e "${GREEN}✓ 路由已从列表中移除。${NC}"
}

function list_routes() {
    echo -e "${YELLOW}=== 当前路由列表 ===${NC}"
    if [[ -f "$ROUTE_LIST" && -s "$ROUTE_LIST" ]]; then
        echo -e "${GREEN}已保存的路由：${NC}"
        cat -n "$ROUTE_LIST"
        echo ""
        echo -e "${GREEN}当前系统路由（相关部分）：${NC}"
        ip route | grep -E "(10\.7\.0\.1|10\.8\.0\.1)" || echo -e "${YELLOW}未找到相关路由${NC}"
    else
        echo -e "${YELLOW}⚠️  当前没有已添加的路由。${NC}"
    fi
}


function  delete_system_routes() {
    echo -e "${YELLOW}=== 删除ROUTE_LIST中的路由 ===${NC}"
    # 备份ROUTE_LIST这个文件 到ROUTE_LIST.bak
    cp "$ROUTE_LIST" "$ROUTE_LIST.bak"
    if [[ -f "$ROUTE_LIST" ]]; then
        while read -r ROUTE_CMD; do
            if [[ -n "$ROUTE_CMD" ]]; then
                DEL_CMD=$(echo "$ROUTE_CMD" | sed 's/add/del/')
                $DEL_CMD && echo -e "${GREEN}✓ 已删除：$DEL_CMD${NC}" || echo -e "${YELLOW}⚠️  路由可能已不存在于系统中：$DEL_CMD${NC}"
            fi
        done < "$ROUTE_LIST"
        # 删除ROUTE_LIST
        rm -f "$ROUTE_LIST"
        # 恢复ROUTE_LIST.bak到ROUTE_LIST
        mv "$ROUTE_LIST.bak" "$ROUTE_LIST"
        echo -e "${GREEN}✓ 所有路由已删除${NC}"
    else
        echo -e "${YELLOW}⚠️  路由列表文件不存在${NC}"
    fi

}
function apply_routes() {
    echo "正在应用所有路由..."
    # 先检查已经存在的路由，如果存在则删除
    delete_system_routes
    # 重新应用路由
    if [[ -f "$ROUTE_LIST" ]]; then
        while read -r ROUTE_CMD; do
            if [[ -n "$ROUTE_CMD" ]]; then
                $ROUTE_CMD && echo -e "${GREEN}✓ 已应用：$ROUTE_CMD${NC}" || echo -e "${RED}❌ 应用失败：$ROUTE_CMD${NC}"
            fi
        done < "$ROUTE_LIST"
        echo -e "${GREEN}✓ 所有路由已应用${NC}"
    else
        echo -e "${YELLOW}⚠️  路由列表文件不存在${NC}"
    fi

}

function start_service() {
    delete_system_routes
    echo -e "${YELLOW}=== 启动服务 ===${NC}"
    systemctl start $SERVICE_NAME
    systemctl status $SERVICE_NAME --no-pager
}

function restart_service() {
    delete_system_routes
    echo -e "${YELLOW}=== 重启服务 ===${NC}"
    systemctl restart $SERVICE_NAME
    systemctl status $SERVICE_NAME --no-pager
}

function stop_service() {
    delete_system_routes
    echo -e "${YELLOW}=== 停止服务 ===${NC}"
    systemctl stop $SERVICE_NAME
    systemctl status $SERVICE_NAME --no-pager
}

function show_menu() {
    echo ""
    echo -e "${YELLOW}==================== 路由管理脚本 ====================${NC}"
    print_systemd_status
    echo ""
    echo "请选择功能："
    echo "1) 初始化 systemd 服务"
    echo "2) 添加域名或IP路由（自动测试延迟）"
    echo "3) 删除已添加的域名或IP路由"
    echo "4) 查看当前路由列表"
    echo "5) 手动应用所有路由"
    echo "6) 启动 systemd 服务"
    echo "7) 重启 systemd 服务"
    echo "8) 停止 systemd 服务"
    echo "9) 追踪路由"
    echo "0) 退出脚本"
    echo -e "${YELLOW}====================================================${NC}"
}

function execute_choice() {
    local choice=$1
    case $choice in
        1) init_systemd ;;
        2) add_route ;;

        3) delete_route ;;
        4) list_routes ;;
        5) apply_routes ;;
        6) start_service ;;
        7) restart_service ;;
        8) stop_service ;;
        9) trace_route ;;
        0) 
            echo -e "${GREEN}👋 感谢使用，再见！${NC}"
            exit 0
            ;;
        *) 
            echo -e "${RED}❌ 无效选择，请输入 0-9${NC}"
            return 1
            ;;
    esac
}

function main_loop() {
    while true; do
        show_menu
        read -p "请输入序号 [0-9]: " CHOICE
        
        # 验证输入是否为数字
        if ! [[ "$CHOICE" =~ ^[0-9]$ ]]; then
            echo -e "${RED}❌ 请输入有效的数字 (0-9)${NC}"
            echo "按任意键继续..."
            read -n 1
            continue
        fi
        
        execute_choice "$CHOICE"
        
        # 如果不是退出命令，询问是否继续
        if [[ "$CHOICE" != "0" ]]; then
            echo ""
            echo -e "${GREEN}操作完成！${NC}"
            read -p "按回车键返回主菜单，或输入 'q' 退出: " CONTINUE
            if [[ "$CONTINUE" == "q" || "$CONTINUE" == "Q" ]]; then
                echo -e "${GREEN}👋 感谢使用，再见！${NC}"
                exit 0
            fi
        fi
    done
}

# 主程序逻辑
case "$1" in
    apply) 
        apply_routes 
        ;;
    *)
        # 如果有命令行参数，执行对应功能后退出
        if [[ -n "$1" ]]; then
            execute_choice "$1"
        else
            # 否则进入交互式循环
            main_loop
        fi
        ;;
esac
