#!/bin/sh
# 宿主机上由 systemd 定时器每 15 秒执行一次：
# emby-proxy 把最新的 nginx 配置写在数据目录 emby-proxy.conf；内容变化时把它复制成 applied.conf
# （/etc/nginx/conf.d/emby-proxy.conf 是指向 applied.conf 的软链接），执行 nginx -t：
#   通过 → reload；不通过 → 恢复上一份 applied.conf。
# 所以 conf.d 里永远是检查通过的配置，即使服务器重启 nginx 也不会因为它起不来。
# 结果写回 reload.log 供面板显示。

DIR=${1:-/opt/emby-proxy/data/nginx}
CONF="$DIR/emby-proxy.conf"
APPLIED="$DIR/applied.conf"
LOG="$DIR/reload.log"

[ -f "$CONF" ] || exit 0
SUM=$(sha256sum "$CONF" | cut -d' ' -f1)
NOW=$(date +%s)

if [ -f "$LOG" ]; then
    LAST_STATUS=$(sed -n 1p "$LOG")
    LAST_TIME=$(sed -n 2p "$LOG")
    LAST_SUM=$(sed -n 3p "$LOG")
    if [ "$SUM" = "$LAST_SUM" ]; then
        # 已处理过这份配置：成功就不再动；失败的 5 分钟后重试一次（例如当时 nginx 没在运行）
        [ "$LAST_STATUS" = "ok" ] && exit 0
        [ $((NOW - ${LAST_TIME:-0})) -lt 300 ] && exit 0
    fi
fi

if [ -f "$APPLIED" ]; then cp "$APPLIED" "$APPLIED.prev"; else : > "$APPLIED.prev"; fi
cp "$CONF" "$APPLIED"

if OUT=$(nginx -t 2>&1); then
    if command -v systemctl >/dev/null 2>&1 && systemctl is-active --quiet nginx 2>/dev/null; then
        R=$(systemctl reload nginx 2>&1)
    else
        R=$(nginx -s reload 2>&1)
    fi
    if [ $? -eq 0 ]; then STATUS=ok; else STATUS=fail; fi
    OUT="$OUT
$R"
else
    STATUS=fail
    cp "$APPLIED.prev" "$APPLIED"   # 回滚，nginx 继续用上一份通过检查的配置
fi

printf '%s\n%s\n%s\n%s\n' "$STATUS" "$NOW" "$SUM" "$OUT" > "$LOG.tmp" && mv "$LOG.tmp" "$LOG"
[ "$STATUS" = "ok" ]
