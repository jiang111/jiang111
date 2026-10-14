#!/bin/bash
# Emby 反代面板一键安装/更新（Docker 版，适合 1Panel）
set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

DIR=/opt/emby-proxy
RAW=https://raw.githubusercontent.com/jiang111/jiang111/master/emby-proxy
SRC_DIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)
FILES="go.mod main.go config.go proxy.go rewrite.go stats.go manager.go admin.go web/index.html Dockerfile"

if ! command -v docker >/dev/null 2>&1; then
    echo -e "${RED}未检测到 docker，请先在 1Panel 里安装 Docker${NC}"
    exit 1
fi

mkdir -p "$DIR/src/web" "$DIR/data"

fetch() {
    if [ -n "$SRC_DIR" ] && [ -f "$SRC_DIR/$1" ]; then
        cp "$SRC_DIR/$1" "$2"
    else
        curl -fsSL "$RAW/$1" -o "$2"
    fi
}

for f in $FILES; do
    fetch "$f" "$DIR/src/$f"
done
fetch docker-compose.yml "$DIR/docker-compose.yml"

cd "$DIR"
echo "正在构建镜像（首次需要拉取 golang 镜像，稍等一两分钟）..."
docker compose up -d --build

sleep 2
PW=$(grep -o '"admin_password": *"[^"]*"' "$DIR/data/config.json" 2>/dev/null | sed 's/.*: *"\(.*\)"/\1/')
IP=$(curl -fsS4 --max-time 5 https://api.ipify.org 2>/dev/null || echo 服务器IP)
echo
echo -e "${GREEN}已启动${NC}"
echo -e "面板地址：${GREEN}http://$IP:55660${NC}"
echo -e "面板密码：${GREEN}$PW${NC}（可在面板「设置」里修改）"
echo "入口端口：55661（路径/域名模式共用），独立端口模式的端口在面板里自己定"
echo "记得在 1Panel → 主机 → 防火墙 放行 55660、55661 以及你给站点分配的端口"
echo "查看日志：docker logs -f emby-proxy"
