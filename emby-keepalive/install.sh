#!/bin/bash
# Emby 保号一键安装（Docker 版，适合 1Panel）
set -e

GREEN='\033[0;32m'
RED='\033[0;31m'
NC='\033[0m'

DIR=/opt/emby-keepalive
RAW=https://raw.githubusercontent.com/jiang111/jiang111/master/emby-keepalive
SRC_DIR=$(cd "$(dirname "$0")" 2>/dev/null && pwd || true)

if ! command -v docker >/dev/null 2>&1; then
    echo -e "${RED}未检测到 docker，请先在 1Panel 里安装 Docker${NC}"
    exit 1
fi

mkdir -p "$DIR"

fetch() {
    if [ -n "$SRC_DIR" ] && [ -f "$SRC_DIR/$1" ]; then
        cp "$SRC_DIR/$1" "$2"
    else
        curl -fsSL "$RAW/$1" -o "$2"
    fi
}

fetch keepalive.py "$DIR/keepalive.py"
fetch docker-compose.yml "$DIR/docker-compose.yml"
if [ ! -f "$DIR/config.json" ]; then
    fetch config.example.json "$DIR/config.json"
    WEB_PW=$(head -c 32 /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c 12)
    sed -i "s/\"password\": \"CHANGE_ME\"/\"password\": \"$WEB_PW\"/" "$DIR/config.json"
    echo -e "${GREEN}已生成配置文件 $DIR/config.json，请先填入你的账号${NC}"
    echo -e "网页面板：http://服务器IP:55656  密码：${GREEN}$WEB_PW${NC}（用户名随便填，可在 config.json 的 web.password 修改）"
    echo "编辑完成后执行："
    echo "  cd $DIR && docker compose up -d"
    echo "立即测试一次："
    echo "  docker run --rm -v $DIR:/data python:3.12-alpine python3 -u /data/keepalive.py --once"
    exit 0
fi

cd "$DIR"
docker compose up -d
echo -e "${GREEN}已启动，查看日志：docker logs -f emby-keepalive${NC}"
