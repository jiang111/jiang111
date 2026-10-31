#!/bin/bash
# 宿主机上由 systemd path 单元 emby-proxy-update.path 触发：面板「一键更新」写入 data/update/request（内容是提交号），
# 这里下载该提交的源码，重新执行 install.sh（构建新镜像并替换容器；构建失败时旧容器照常运行），结果写到 data/update/status。
DIR=${EMBY_PROXY_DIR:-/opt/emby-proxy}
UPD="$DIR/data/update"
REQ="$UPD/request"
[ -f "$REQ" ] || exit 0
SHA=$(tr -cd '0-9a-f' < "$REQ" | head -c 40)
rm -f "$REQ"
[ ${#SHA} -ge 7 ] || exit 0

status() { printf '%s\n%s\n%s\n' "$1" "$(date +%s)" "$SHA" > "$UPD/status.tmp" && mv "$UPD/status.tmp" "$UPD/status"; }
status running
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT
{
    echo "== $(date '+%F %T') 更新到 $SHA"
    if ! curl -fsSL "https://codeload.github.com/jiang111/jiang111/tar.gz/$SHA" | tar -xz -C "$TMP" --wildcards --strip-components=1 '*/emby-proxy'; then
        echo "下载源码失败"
        exit 1
    fi
    [ -f "$TMP/emby-proxy/main.go" ] || { echo "源码不完整"; exit 1; }
    EMBY_PROXY_COMMIT=$SHA bash "$TMP/emby-proxy/install.sh"
} > "$UPD/log" 2>&1
if [ $? -eq 0 ]; then status ok; else status fail; fi
