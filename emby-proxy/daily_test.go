package main

import (
	"strings"
	"testing"
	"time"
)

func TestDailyStats(t *testing.T) {
	d := newDailyStore()
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.Local)
	d.now = func() time.Time { return now }
	d.play("s1", "1.1.1.1", "100")
	d.play("s1", "1.1.1.1", "100") // 30 分钟内同一部不重复计
	d.play("s1", "1.1.1.1", "101")
	d.play("s1", "2.2.2.2", "100")
	d.visit("s1", "1.1.1.1")
	d.visit("s1", "1.1.1.1")
	d.visit("s1", "2.2.2.2")
	d.bytesTick(map[string]int64{"s1": 1000}) // 第一次只记基准
	d.bytesTick(map[string]int64{"s1": 1500})
	d.bytesTick(map[string]int64{"s1": 200}) // 清零后不倒扣
	d.bytesTick(map[string]int64{"s1": 300})
	now = now.Add(40 * time.Minute)
	d.play("s1", "1.1.1.1", "100")
	got := d.export(7)["2026-10-31"]["s1"]
	if got.Plays != 4 || got.IPs != 2 || got.Bytes != 600 {
		t.Fatalf("today: %+v", got)
	}
	now = now.AddDate(0, 0, 1)
	d.play("s1", "3.3.3.3", "1")
	ex := d.export(7)
	if ex["2026-11-01"]["s1"].Plays != 1 || ex["2026-10-31"]["s1"].Plays != 4 {
		t.Fatalf("two days: %+v", ex)
	}
	p := t.TempDir() + "/daily.json"
	d.save(p)
	d2 := newDailyStore()
	d2.now = d.now
	d2.load(p)
	if d2.export(7)["2026-10-31"]["s1"].IPs != 2 {
		t.Fatal("reload lost data")
	}
	now = now.AddDate(0, 0, 40)
	d2.bytesTick(nil)
	if len(d2.export(60)) != 0 {
		t.Fatal("old days should be pruned")
	}
}

func TestDailyThroughProxy(t *testing.T) {
	m, entry, _, _ := setup(t, Site{Mode: "path", Path: "hk"})
	get(t, entry.URL+"/hk/emby/Items/1/PlaybackInfo", "X-Real-Ip", "5.5.5.5")
	get(t, entry.URL+"/hk/emby/Items/1/PlaybackInfo", "X-Real-Ip", "5.5.5.5")
	get(t, entry.URL+"/hk/emby/Items/1/PlaybackInfo", "X-Real-Ip", "6.6.6.6")
	today := m.daily.export(1)[dayOf(time.Now())]["s1"]
	if today.Plays != 2 || today.IPs != 2 {
		t.Fatalf("daily via proxy: %+v", today)
	}
	if playItem("/emby/items/abc/playbackinfo") != "abc" || !strings.Contains(dayOf(time.Now()), "-") {
		t.Fatal("playItem")
	}
}

func TestDailyHours(t *testing.T) {
	d := newDailyStore()
	now := time.Date(2026, 10, 31, 21, 5, 0, 0, time.Local)
	d.now = func() time.Time { return now }
	d.play("s1", "1.1.1.1", "1")
	d.play("s1", "2.2.2.2", "1")
	got := d.export(1)["2026-10-31"]["s1"]
	if got.Hours[21] != 2 || got.Last != now.Unix() {
		t.Fatalf("hours: %+v", got)
	}
}
