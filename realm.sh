#!/bin/bash

# 颜色定义
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

# 在脚本开头添加版本号
VERSION="1.1"

# 检查realm是否已安装
if [ -f "/root/realm/realm" ]; then
    echo "检测到realm已安装。"
    realm_status="已安装"
    realm_status_color="\033[0;32m"
else
    echo "realm未安装。"
    realm_status="未安装"
    realm_status_color="\033[0;31m"
fi

# 检查realm服务状态
check_realm_service_status() {
    if systemctl is-active --quiet realm; then
        echo -e "\033[0;32m启用\033[0m"
    else
        echo -e "\033[0;31m未启用\033[0m"
    fi
}

# 显示菜单的函数
show_menu() {
    clear
    echo "realm转发脚本"
    echo "================================================="
    echo -e "${GREEN}作者：jinqian${NC}"
    echo -e "${GREEN}网站：https://jinqians.com${NC}"
    echo "================================================="
    echo "1. 部署环境"
    echo "2. 添加转发"
    echo "3. 删除转发"
    echo "4. 启动服务"
    echo "5. 停止服务"
    echo "6. 重启服务"
    echo "7. 一键卸载"
    echo "================="
    echo -e "realm 状态：${realm_status_color}${realm_status}\033[0m"
    echo -n "realm 转发状态："
    check_realm_service_status
}

# 配置防火墙规则
configure_firewall() {
    local port=$1
    local action=$2  # "add" 或 "remove"

    # 检查是否安装了ufw
    if command -v ufw >/dev/null 2>&1; then
        if [ "$action" = "add" ]; then
            ufw allow $port/tcp
        else
            ufw delete allow $port/tcp
        fi
    fi

    # 检查是否安装了iptables
    if command -v iptables >/dev/null 2>&1; then
        if [ "$action" = "add" ]; then
            iptables -I INPUT -p tcp --dport $port -j ACCEPT
        else
            iptables -D INPUT -p tcp --dport $port -j ACCEPT 2>/dev/null
        fi
    fi
}

# 部署环境的函数
deploy_realm() {
    mkdir -p /root/realm
    cd /root/realm
    wget -O realm.tar.gz https://github.com/zhboner/realm/releases/download/v2.6.0/realm-x86_64-unknown-linux-gnu.tar.gz
    tar -xvf realm.tar.gz
    chmod +x realm
    # 创建配置文件
    wget -O config.toml https://raw.githubusercontent.com/jiang111/jiang111/master/config.toml
    
    # 创建服务文件
    cat > /etc/systemd/system/realm.service << EOF
[Unit]
Description=realm
After=network-online.target
Wants=network-online.target systemd-networkd-wait-online.service

[Service]
Type=simple
User=root
Restart=on-failure
RestartSec=5s
ExecStart=/root/realm/realm -c /root/realm/config.toml
WorkingDirectory=/root/realm

[Install]
WantedBy=multi-user.target
EOF

    # 设置正确的权限
    chmod 644 /etc/systemd/system/realm.service
    
    systemctl daemon-reload
    systemctl enable realm.service
    
    # 更新realm状态变量
    realm_status="已安装"
    realm_status_color="\033[0;32m"
    echo "部署完成。"
}

# 卸载realm
uninstall_realm() {
    systemctl stop realm
    systemctl disable realm
    rm -f /etc/systemd/system/realm.service
    systemctl daemon-reload
    rm -rf /root/realm
    echo "realm已被卸载。"
    # 更新realm状态变量
    realm_status="未安装"
    realm_status_color="\033[0;31m"
}

# 添加转发规则
add_forward() {
    while true; do
        read -p "请输入本地监听端口: " port
        read -p "请输入目标IP/域名: " ip
        read -p "请输入目标端口: " remote_port

        # 验证端口号
        if ! [[ "$port" =~ ^[0-9]+$ ]] || [ "$port" -lt 1 ] || [ "$port" -gt 65535 ]; then
            echo -e "${RED}错误：端口必须是1-65535之间的数字${NC}"
            continue
        fi

        # 追加到config.toml文件
        echo "[[endpoints]]
listen = \"[::]:$port\"
remote = \"$ip:$remote_port\"" >> /root/realm/config.toml
        
        # 配置防火墙
        configure_firewall $port "add"
        
        echo -e "${GREEN}已添加转发规则：本地端口 $port -> $ip:$remote_port${NC}"
        
        read -p "是否继续添加(Y/N)? " answer
        if [[ $answer != "Y" && $answer != "y" ]]; then
            break
        fi
    done
    
    # 重启realm服务以应用新配置
    restart_service
}

# 删除转发规则
delete_forward() {
    echo "当前转发规则："
    local IFS=$'\n'
    local lines=($(grep -n 'listen =' /root/realm/config.toml))
    if [ ${#lines[@]} -eq 0 ]; then
        echo "没有发现任何转发规则。"
        return
    fi

    local index=1
    declare -A port_map
    for line in "${lines[@]}"; do
        local port=$(echo $line | grep -o '[0-9]\+')
        port_map[$index]=$port
        local remote_line=$(($(echo $line | cut -d':' -f1) + 1))
        local remote=$(sed -n "${remote_line}p" /root/realm/config.toml | cut -d'"' -f2)
        echo "${index}. 本地端口 $port -> $remote"
        let index+=1
    done

    read -p "请输入要删除的转发规则序号，直接按回车返回主菜单：" choice
    if [ -z "$choice" ]; then
        return
    fi

    if ! [[ $choice =~ ^[0-9]+$ ]] || [ $choice -lt 1 ] || [ $choice -gt ${#lines[@]} ]; then
        echo -e "${RED}无效的选择${NC}"
        return
    fi

    # 获取要删除的端口
    local port_to_delete=${port_map[$choice]}
    
    # 删除防火墙规则
    configure_firewall $port_to_delete "remove"

    # 删除配置文件中的规则
    local line_number=$(echo ${lines[$((choice-1))]} | cut -d':' -f1)
    sed -i "$line_number,$(($line_number+1))d" /root/realm/config.toml

    echo -e "${GREEN}已删除转发规则和对应的防火墙规则${NC}"
    
    # 重启realm服务以应用新配置
    restart_service
}

# 重启realm服务
restart_service() {
    systemctl daemon-reload
    systemctl restart realm
    if [ $? -eq 0 ]; then
        echo -e "${GREEN}realm服务已重启${NC}"
    else
        echo -e "${RED}realm服务重启失败，请检查日志：journalctl -u realm${NC}"
    fi
}

# 启动服务
start_service() {
    systemctl daemon-reload
    systemctl enable realm.service
    systemctl start realm.service
    if [ $? -eq 0 ]; then
        echo -e "${GREEN}realm服务已启动并设置为开机自启${NC}"
    else
        echo -e "${RED}realm服务启动失败，请检查日志：journalctl -u realm${NC}"
    fi
}

# 停止服务
stop_service() {
    systemctl stop realm
    echo "realm服务已停止。"
}


# 主循环
while true; do
    show_menu
    read -p "请选择一个选项: " choice
    case $choice in
        1)
            deploy_realm
            ;;
        2)
            add_forward
            ;;
        3)
            delete_forward
            ;;
        4)
            start_service
            ;;
        5)
            stop_service
            ;;
        6)
            restart_service
            ;;
        7)
            uninstall_realm
            ;;
        *)
            echo "无效选项: $choice"
            ;;
    esac
    read -p "按任意键继续..." key
done
