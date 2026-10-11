#!/usr/bin/env python3
"""Emby 保号脚本：定期登录每个账号，模拟播放一段视频若干分钟。

只用 Python 标准库，配置文件见 config.example.json。
用法：
  python3 keepalive.py --config /data/config.json --once   # 立即对所有账号跑一次
  python3 keepalive.py --config /data/config.json          # 常驻，按 interval_days 周期执行
"""
import argparse
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


def log(msg):
    print(f"[{datetime.now(TZ):%Y-%m-%d %H:%M:%S}] {msg}", flush=True)


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


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--config", default="/data/config.json")
    ap.add_argument("--once", action="store_true", help="立即执行一次后退出")
    args = ap.parse_args()

    with open(args.config) as f:
        cfg = json.load(f)
    set_tz(cfg.get("settings", {}))
    if args.once:
        sys.exit(0 if run_all(cfg) else 1)

    state_path = os.path.join(os.path.dirname(os.path.abspath(args.config)), "state.json")
    log("常驻模式启动")
    first_run = not os.path.exists(state_path)
    shown = None
    while True:
        with open(args.config) as f:
            cfg = json.load(f)
        settings = cfg.get("settings", {})
        set_tz(settings)
        interval_days = settings.get("interval_days", 1)
        hh, mm = (int(x) for x in settings.get("run_at", "04:00").split(":"))
        state = load_state(state_path)
        now = datetime.now(TZ)

        if first_run:
            # 没有 state.json（首次安装或手动删除）时立即执行一轮
            next_run = now
        else:
            last_date = state.get("last_success_date")
            if not last_date and state.get("last_success"):
                last_date = datetime.fromtimestamp(state["last_success"], TZ).date().isoformat()
            day = (date.fromisoformat(last_date) + timedelta(days=interval_days)
                   if last_date else now.date())
            next_run = datetime.combine(day, dtime(hh, mm), TZ)
            if state.get("last_attempt"):
                # 失败后 2 小时再试
                next_run = max(next_run, datetime.fromtimestamp(state["last_attempt"], TZ)
                               + timedelta(seconds=RETRY_GAP))

        if now >= next_run:
            first_run = False
            state["last_attempt"] = time.time()
            save_state(state_path, state)
            if run_all(cfg):
                state["last_success_date"] = datetime.now(TZ).date().isoformat()
                state.pop("last_attempt", None)
                state.pop("last_success", None)
            save_state(state_path, state)
            continue
        if next_run != shown:
            log(f"下次执行时间：{next_run:%Y-%m-%d %H:%M}")
            shown = next_run
        time.sleep(min(max((next_run - now).total_seconds(), 1), 300))


if __name__ == "__main__":
    main()
