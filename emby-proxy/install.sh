#!/bin/bash
# Emby 反代面板一键安装/更新（Docker 版）
set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

DIR=/opt/emby-proxy
RAW=https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy
TARBALL=https://codeload.github.com/jiang111/jiang111/tar.gz/refs/heads/master
SRC_DIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)

if ! command -v docker >/dev/null 2>&1; then
    echo -e "${RED}未检测到 docker，请先安装：curl -fsSL https://get.docker.com | sh${NC}"
    exit 1
fi
if docker compose version >/dev/null 2>&1; then
    COMPOSE="docker compose"
elif command -v docker-compose >/dev/null 2>&1; then
    COMPOSE="docker-compose"
else
    echo -e "${RED}未检测到 docker compose 插件，请安装 docker-compose-plugin${NC}"
    exit 1
fi

mkdir -p "$DIR/data"
rm -rf "$DIR/src.new"
mkdir -p "$DIR/src.new"

# 源码整目录获取，避免漏文件
if [ -n "$SRC_DIR" ] && [ -f "$SRC_DIR/main.go" ]; then
    cp -r "$SRC_DIR/." "$DIR/src.new/"
else
    curl -fsSL "$TARBALL" | tar -xz -C "$DIR/src.new" --strip-components=2 jiang111-master/emby-proxy
fi
if [ ! -f "$DIR/src.new/main.go" ]; then
    echo -e "${RED}下载源码失败${NC}"
    exit 1
fi
rm -rf "$DIR/src"
mv "$DIR/src.new" "$DIR/src"
cp "$DIR/src/docker-compose.yml" "$DIR/docker-compose.yml"

cd "$DIR"
echo "正在构建镜像（首次需要拉取 golang 镜像，稍等一两分钟）..."
$COMPOSE up -d --build

sleep 2

# ---- nginx 自动应用：程序把配置写到 data/nginx/，宿主机定时检查、nginx -t 通过才 reload ----
NGINX_AUTO=no
if command -v nginx >/dev/null 2>&1 && command -v systemctl >/dev/null 2>&1 && [ -d /etc/nginx/conf.d ] && [ "${EMBY_PROXY_NO_NGINX:-}" != "1" ]; then
    install -m 755 "$DIR/src/nginx-reload.sh" "$DIR/nginx-reload.sh"
    mkdir -p "$DIR/data/nginx"
    [ -f "$DIR/data/nginx/applied.conf" ] || : > "$DIR/data/nginx/applied.conf"
    LINK=/etc/nginx/conf.d/emby-proxy.conf
    if [ -e "$LINK" ] && [ ! -L "$LINK" ]; then
        mv "$LINK" "/etc/nginx/emby-proxy.conf.bak.$(date +%s)"
        echo "已把原来手动放的 $LINK 备份到 /etc/nginx/"
    fi
    ln -sfn "$DIR/data/nginx/applied.conf" "$LINK"
    cat > /etc/systemd/system/emby-proxy-nginx.service <<UNIT
[Unit]
Description=Apply emby-proxy nginx config
After=nginx.service

[Service]
Type=oneshot
ExecStart=/bin/sh $DIR/nginx-reload.sh $DIR/data/nginx
UNIT
    cat > /etc/systemd/system/emby-proxy-nginx.timer <<UNIT
[Unit]
Description=Check emby-proxy nginx config every 15s

[Timer]
OnBootSec=30s
OnUnitActiveSec=15s
AccuracySec=1s

[Install]
WantedBy=timers.target
UNIT
    systemctl daemon-reload
    systemctl enable --now emby-proxy-nginx.timer >/dev/null 2>&1
    sh "$DIR/nginx-reload.sh" "$DIR/data/nginx" >/dev/null 2>&1 || true
    NGINX_AUTO=yes
fi

PW=$(grep -o '"admin_password": *"[^"]*"' "$DIR/data/config.json" 2>/dev/null | sed 's/.*: *"\(.*\)"/\1/')
IP=$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || echo 服务器IP)
echo
echo -e "${GREEN}已启动${NC}"
echo -e "面板地址：${GREEN}http://$IP:55660${NC}"
echo -e "面板密码：${GREEN}$PW${NC}（可在面板「设置」里修改）"
echo "入口端口：55661（路径/域名模式共用），独立端口模式的端口在面板里自己定"
echo "记得在防火墙/安全组放行 55660（面板）；用 nginx 做 HTTPS 时放行 nginx 的对外端口即可"
if [ "$NGINX_AUTO" = yes ]; then
    echo -e "nginx 自动应用：${GREEN}已开启${NC}（面板里改站点后约 15 秒内自动 nginx -t && reload，检查不通过会保留旧配置）"
else
    echo "nginx 自动应用：未开启（未检测到 nginx 或 systemd）。装好 nginx 后重新执行本脚本即可开启"
fi
echo "完整教程：https://github.com/jiang111/jiang111/tree/master/emby-proxy"
echo "查看日志：docker logs -f emby-proxy"
