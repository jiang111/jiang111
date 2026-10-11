#!/usr/bin/env python3
"""Emby 保号脚本：定期登录每个账号，模拟播放一段视频若干分钟。

只用 Python 标准库，配置文件见 config.example.json。
用法：
  python3 keepalive.py --config /data/config.json --once   # 立即对所有账号跑一次
  python3 keepalive.py --config /data/config.json          # 常驻，按 interval_days 周期执行
常驻模式下若配置了 web.password，会在 web.port 提供网页面板（状态、日志、立即执行按钮）。
"""
import argparse
import base64
import collections
import hmac
import http.server
import json
import os
import random
import ssl
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from datetime import date, datetime, timedelta, timezone
from datetime import time as dtime

TICKS_PER_SECOND = 10_000_000
CLIENT = "Emby Web"
CLIENT_VERSION = "4.8.10.0"
RETRY_GAP = 2 * 3600
# 默认台湾时间 UTC+8（台湾没有夏令时），不依赖容器的 tzdata
TZ = timezone(timedelta(hours=8))


def set_tz(settings):
    global TZ
    TZ = timezone(timedelta(hours=settings.get("utc_offset", 8)))


WEB_PLACEHOLDER = "CHANGE_ME"
LOG_BUF = collections.deque(maxlen=300)
# 常驻模式的运行状态，供网页面板读取
STATUS = {"running": False, "next_run": None, "last_success_date": None,
          "last_summary": None, "last_finished": None}
MANUAL = threading.Event()


def log(msg):
    line = f"[{datetime.now(TZ):%Y-%m-%d %H:%M:%S}] {msg}"
    LOG_BUF.append(line)
    print(line, flush=True)


class EmbyClient:
    def __init__(self, base_url, device_id, device_name, verify_ssl=True, timeout=30):
        self.base = base_url.rstrip("/")
        self.device_id = device_id
        self.device_name = device_name
        self.token = None
        self.user_id = None
        self.timeout = timeout
        self.ctx = None if verify_ssl else ssl._create_unverified_context()

    def _auth_header(self):
        h = (f'MediaBrowser Client="{CLIENT}", Device="{self.device_name}", '
             f'DeviceId="{self.device_id}", Version="{CLIENT_VERSION}"')
        if self.token:
            h += f', Token="{self.token}"'
        return h

    def request(self, method, path, params=None, body=None, raw=False):
        url = self.base + path
        if params:
            url += "?" + urllib.parse.urlencode(params)
        headers = {
            "X-Emby-Authorization": self._auth_header(),
            "Accept": "application/json",
            "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                          "(KHTML, like Gecko) Chrome/129.0 Safari/537.36",
        }
        if self.token:
            headers["X-Emby-Token"] = self.token
        data = None
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        req = urllib.request.Request(url, data=data, headers=headers, method=method)
        resp = urllib.request.urlopen(req, timeout=self.timeout, context=self.ctx)
        if raw:
            return resp
        with resp:
            text = resp.read()
        return json.loads(text) if text else None

    def login(self, username, password):
        r = self.request("POST", "/Users/AuthenticateByName",
                         body={"Username": username, "Pw": password})
        self.token = r["AccessToken"]
        self.user_id = r["User"]["Id"]

    def logout(self):
        try:
            self.request("POST", "/Sessions/Logout")
        except Exception:
            pass

    def pick_item(self, min_seconds):
        params = {
            "IncludeItemTypes": "Movie,Episode",
            "Recursive": "true",
            "SortBy": "Random",
            "Limit": "30",
            "Fields": "RunTimeTicks,MediaSources",
        }
        r = self.request("GET", f"/Users/{self.user_id}/Items", params=params)
        items = [i for i in (r or {}).get("Items", [])
                 if (i.get("RunTimeTicks") or 0) >= min_seconds * TICKS_PER_SECOND]
        if not items:
            items = (r or {}).get("Items", [])
        if not items:
            raise RuntimeError("媒体库里没有找到电影/剧集")
        return random.choice(items)

    def playback_info(self, item_id):
        try:
            r = self.request("POST", f"/Items/{item_id}/PlaybackInfo",
                             params={"UserId": self.user_id, "StartTimeTicks": 0,
                                     "IsPlayback": "true", "AutoOpenLiveStream": "true",
                                     "MaxStreamingBitrate": 140000000},
                             body={"DeviceProfile": {}})
            src = (r.get("MediaSources") or [{}])[0]
            return r.get("PlaySessionId") or uuid.uuid4().hex, src.get("Id") or item_id
        except Exception as e:
            log(f"  PlaybackInfo 失败（忽略，继续）：{e}")
            return uuid.uuid4().hex, item_id


def stream_reader(client, item_id, media_source_id, stop_event, kbps):
    """真实拉取少量视频流，模拟客户端在下载。kbps<=0 时不运行。"""
    params = {"static": "true", "MediaSourceId": media_source_id,
              "DeviceId": client.device_id, "api_key": client.token}
    chunk = max(int(kbps * 1024 / 8), 4096)  # 每秒读取的字节数
    try:
        resp = client.request("GET", f"/Videos/{item_id}/stream", params=params, raw=True)
        with resp:
            while not stop_event.is_set():
                if not resp.read(chunk):
                    break
                stop_event.wait(1)
    except Exception as e:
        log(f"  拉流失败（不影响播放上报）：{e}")


def watch(acc, settings):
    name = acc.get("name") or acc["username"]
    device_id = acc.get("device_id") or uuid.uuid5(
        uuid.NAMESPACE_URL, acc["server"] + acc["username"]).hex
    c = EmbyClient(acc["server"], device_id, acc.get("device_name", "Chrome Windows"),
                   verify_ssl=settings.get("verify_ssl", True))
    lo, hi = settings.get("watch_minutes", [6, 9])
    seconds = int(random.uniform(lo, hi) * 60)
    interval = settings.get("report_interval", 10)

    log(f"[{name}] 登录 {acc['server']}")
    c.login(acc["username"], acc["password"])
    try:
        item = c.pick_item(seconds + 60)
        item_id = item["Id"]
        title = item.get("Name", "")
        if item.get("SeriesName"):
            title = f"{item['SeriesName']} - {title}"
        total = (item.get("RunTimeTicks") or 0) // TICKS_PER_SECOND
        start = 0
        if total > seconds + 120:
            start = random.randint(0, min(total - seconds - 60, 1800))
        play_session, media_source_id = c.playback_info(item_id)
        log(f"[{name}] 开始播放《{title}》，从 {start // 60} 分开始，计划看 {seconds // 60} 分 {seconds % 60} 秒")

        base = {"ItemId": item_id, "MediaSourceId": media_source_id,
                "PlaySessionId": play_session, "PlayMethod": "DirectStream",
                "CanSeek": True, "IsPaused": False, "IsMuted": False,
                "VolumeLevel": 100, "PlaybackRate": 1, "RepeatMode": "RepeatNone"}

        stop = threading.Event()
        t = None
        if settings.get("stream_kbps", 0) > 0:
            t = threading.Thread(target=stream_reader, daemon=True,
                                 args=(c, item_id, media_source_id, stop, settings["stream_kbps"]))
            t.start()

        c.request("POST", "/Sessions/Playing",
                  body={**base, "PositionTicks": start * TICKS_PER_SECOND})
        began = time.time()
        pos = start
        try:
            while True:
                elapsed = time.time() - began
                if elapsed >= seconds:
                    break
                time.sleep(min(interval, seconds - elapsed))
                pos = start + int(time.time() - began)
                try:
                    c.request("POST", "/Sessions/Playing/Progress",
                              body={**base, "PositionTicks": pos * TICKS_PER_SECOND,
                                    "EventName": "timeupdate"})
                except Exception as e:
                    log(f"[{name}]   进度上报失败（继续）：{e}")
        finally:
            stop.set()
            c.request("POST", "/Sessions/Playing/Stopped",
                      body={**base, "PositionTicks": pos * TICKS_PER_SECOND})
        log(f"[{name}] ✅ 完成，实际播放 {pos - start} 秒")
        return True, f"{name}：《{title}》{(pos - start) // 60} 分钟"
    finally:
        c.logout()


def notify(settings, text):
    tg = settings.get("telegram") or {}
    if not tg.get("bot_token") or not tg.get("chat_id"):
        return
    try:
        data = urllib.parse.urlencode({"chat_id": tg["chat_id"], "text": text}).encode()
        urllib.request.urlopen(
            f"https://api.telegram.org/bot{tg['bot_token']}/sendMessage", data=data, timeout=20)
    except Exception as e:
        log(f"Telegram 通知失败：{e}")


def run_all(cfg):
    settings = cfg.get("settings", {})
    accounts = [a for a in cfg["accounts"] if a.get("enabled", True)]
    results = [None] * len(accounts)

    def worker(i, acc):
        name = acc.get("name") or acc["username"]
        retries = settings.get("retries", 3)
        for attempt in range(1, retries + 1):
            try:
                results[i] = watch(acc, settings)
                return
            except urllib.error.HTTPError as e:
                err = f"HTTP {e.code} {e.reason}"
            except Exception as e:
                err = str(e)
            log(f"[{name}] ❌ 第 {attempt} 次失败：{err}")
            if attempt < retries:
                time.sleep(60 * attempt)
        results[i] = (False, f"{name}：失败 - {err}")

    # 各账号并行，开始时间错开几秒
    threads = []
    for i, acc in enumerate(accounts):
        t = threading.Thread(target=worker, args=(i, acc))
        t.start()
        threads.append(t)
        time.sleep(random.uniform(3, 15))
    for t in threads:
        t.join()

    ok = all(r[0] for r in results)
    summary = "Emby 保号" + ("全部成功 ✅" if ok else "有失败 ❌") + "\n" + \
              "\n".join(("✅ " if r[0] else "❌ ") + r[1] for r in results)
    log(summary.replace("\n", " | "))
    STATUS["last_summary"] = summary
    STATUS["last_finished"] = f"{datetime.now(TZ):%Y-%m-%d %H:%M}"
    notify(settings, summary)
    return ok


def load_state(path):
    try:
        with open(path) as f:
            return json.load(f)
    except Exception:
        return {}


def save_state(path, state):
    tmp = path + ".tmp"
    with open(tmp, "w") as f:
        json.dump(state, f)
    os.replace(tmp, path)


PAGE = r"""<!doctype html>
<html lang="zh"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Emby 保号</title>
<style>
:root{--bg:#f5f6f8;--card:#fff;--fg:#1f2329;--muted:#6b7280;--accent:#2563eb;--ok:#16a34a;--bad:#dc2626;--log:#0f172a;--logfg:#e2e8f0}
@media (prefers-color-scheme:dark){:root{--bg:#0b0d10;--card:#16191e;--fg:#e5e7eb;--muted:#9ca3af;--log:#05070a}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.5 -apple-system,system-ui,"PingFang TC","Microsoft JhengHei",sans-serif}
main{max-width:760px;margin:0 auto;padding:16px}
h1{font-size:20px;margin:4px 0 16px}
.card{background:var(--card);border-radius:12px;padding:16px;margin-bottom:12px;box-shadow:0 1px 3px rgba(0,0,0,.08)}
.row{display:flex;justify-content:space-between;gap:12px;padding:4px 0}.row span:first-child{color:var(--muted)}
button{width:100%;padding:14px;border:0;border-radius:10px;background:var(--accent);color:#fff;font-size:16px;font-weight:600;cursor:pointer}
button:disabled{opacity:.5;cursor:default}
.badge{font-weight:600}.on{color:var(--accent)}.ok{color:var(--ok)}.bad{color:var(--bad)}
pre{margin:0;white-space:pre-wrap;word-break:break-all;font:12px/1.5 ui-monospace,Menlo,Consolas,monospace}
#summary{white-space:pre-wrap}
#log{background:var(--log);color:var(--logfg);border-radius:8px;padding:12px;max-height:420px;overflow:auto}
</style></head><body><main>
<h1>Emby 保号</h1>
<div class="card">
<div class="row"><span>状态</span><span id="state" class="badge">…</span></div>
<div class="row"><span>上次成功</span><span id="last">…</span></div>
<div class="row"><span>下次执行</span><span id="next">…</span></div>
</div>
<div class="card"><button id="run">立即执行</button></div>
<div class="card"><div class="row"><span>最近一次结果</span><span id="fin"></span></div><div id="summary">—</div></div>
<div class="card"><pre id="log"></pre></div>
</main><script>
const $=id=>document.getElementById(id);
async function refresh(){
  try{
    const s=await (await fetch('api/status',{cache:'no-store'})).json();
    $('state').textContent=s.running?'执行中…':'等待中';
    $('state').className='badge '+(s.running?'on':'ok');
    $('last').textContent=s.last_success_date||'—';
    $('next').textContent=s.running?'—':(s.next_run||'—');
    $('fin').textContent=s.last_finished||'';
    $('summary').textContent=s.last_summary||'—';
    $('run').disabled=s.running||s.queued;
    $('run').textContent=s.running?'执行中，约 6～9 分钟…':(s.queued?'即将开始…':'立即执行');
    const log=$('log'),bottom=log.scrollHeight-log.scrollTop-log.clientHeight<40;
    log.textContent=s.log.join('\n');if(bottom)log.scrollTop=log.scrollHeight;
  }catch(e){$('state').textContent='连接失败';$('state').className='badge bad'}
}
$('run').onclick=async()=>{
  if(!confirm('立即对所有账号执行一轮保号？'))return;
  $('run').disabled=true;
  await fetch('api/run',{method:'POST',headers:{'X-Requested-With':'fetch'}});
  refresh();
};
refresh();setInterval(refresh,3000);
</script></body></html>"""


def start_web(settings):
    web = settings.get("web") or {}
    password = str(web.get("password") or "")
    if not password or password == WEB_PLACEHOLDER:
        log("网页面板未启用（config.json 里 web.password 未设置）")
        return
    port = int(web.get("port", 8686))

    class Handler(http.server.BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _authed(self):
            h = self.headers.get("Authorization", "")
            if h.startswith("Basic "):
                try:
                    pw = base64.b64decode(h[6:]).decode().split(":", 1)[1]
                    if hmac.compare_digest(pw.encode(), password.encode()):
                        return True
                except Exception:
                    pass
            self.send_response(401)
            self.send_header("WWW-Authenticate", 'Basic realm="emby-keepalive"')
            self.end_headers()
            return False

        def _send(self, code, body, ctype="application/json; charset=utf-8"):
            b = body.encode() if isinstance(body, str) else body
            self.send_response(code)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(b)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(b)

        def do_GET(self):
            if not self._authed():
                return
            path = self.path.split("?")[0]
            if path == "/":
                self._send(200, PAGE, "text/html; charset=utf-8")
            elif path == "/api/status":
                self._send(200, json.dumps({**STATUS, "queued": MANUAL.is_set(),
                                            "log": list(LOG_BUF)[-150:]}, ensure_ascii=False))
            else:
                self._send(404, "{}")

        def do_POST(self):
            if not self._authed():
                return
            # 自定义请求头可防止跨站伪造请求
            if self.path != "/api/run" or self.headers.get("X-Requested-With") != "fetch":
                return self._send(400, "{}")
            if STATUS["running"]:
                return self._send(409, '{"msg":"正在执行中"}')
            log("网页面板触发手动执行")
            MANUAL.set()
            self._send(200, '{"msg":"ok"}')

    srv = http.server.ThreadingHTTPServer(("0.0.0.0", port), Handler)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    log(f"网页面板已启动：http://服务器IP:{port}")


def record_success(state_path):
    state = load_state(state_path)
    state["last_success_date"] = datetime.now(TZ).date().isoformat()
    state.pop("last_attempt", None)
    state.pop("last_success", None)
    save_state(state_path, state)
    STATUS["last_success_date"] = state["last_success_date"]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default="/data/config.json")
    ap.add_argument("--once", action="store_true", help="立即执行一次后退出")
    args = ap.parse_args()

    with open(args.config) as f:
        cfg = json.load(f)
    set_tz(cfg.get("settings", {}))
    state_path = os.path.join(os.path.dirname(os.path.abspath(args.config)), "state.json")
    if args.once:
        ok = run_all(cfg)
        if ok:
            # 手动执行成功也记录下来，自动执行从今天重新计算间隔
            record_success(state_path)
        sys.exit(0 if ok else 1)

    log("常驻模式启动")
    start_web(cfg.get("settings", {}))
    first_run = not os.path.exists(state_path)
    shown = None
    while True:
        with open(args.config) as f:
            cfg = json.load(f)
        settings = cfg.get("settings", {})
        set_tz(settings)
        interval_days = settings.get("interval_days", 10)
        hh, mm = (int(x) for x in settings.get("run_at", "04:00").split(":"))
        state = load_state(state_path)
        now = datetime.now(TZ)

        last_date = state.get("last_success_date")
        if not last_date and state.get("last_success"):
            last_date = datetime.fromtimestamp(state["last_success"], TZ).date().isoformat()
        STATUS["last_success_date"] = last_date
        if first_run:
            # 没有 state.json（首次安装或手动删除）时立即执行一轮
            next_run = now
        else:
            day = (date.fromisoformat(last_date) + timedelta(days=interval_days)
                   if last_date else now.date())
            next_run = datetime.combine(day, dtime(hh, mm), TZ)
            if state.get("last_attempt"):
                # 失败后 2 小时再试
                next_run = max(next_run, datetime.fromtimestamp(state["last_attempt"], TZ)
                               + timedelta(seconds=RETRY_GAP))
        STATUS["next_run"] = f"{next_run:%Y-%m-%d %H:%M}"

        manual = MANUAL.is_set()
        if manual or now >= next_run:
            first_run = False
            STATUS["running"] = True
            MANUAL.clear()
            if not manual:
                state["last_attempt"] = time.time()
                save_state(state_path, state)
            try:
                ok = run_all(cfg)
            except Exception as e:
                log(f"执行出错：{e}")
                ok = False
            finally:
                STATUS["running"] = False
            if ok:
                record_success(state_path)
            continue
        if next_run != shown:
            log(f"下次执行时间：{next_run:%Y-%m-%d %H:%M}")
            shown = next_run
        MANUAL.wait(min(max((next_run - now).total_seconds(), 1), 300))


if __name__ == "__main__":
    main()
