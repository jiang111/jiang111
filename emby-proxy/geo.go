package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// IP 归属国家：用五大洲 RIR（APNIC / ARIN / RIPE / LACNIC / AFRINIC）每天公开的分配数据，
// 不需要注册账号、不需要第三方库，也不信任客户端自己带的国家请求头（直连时随便伪造）。
// 精度是「分配给哪个国家的机构」，和商业 GeoIP 相比个别 IP 会有出入，用来封国家足够。
// 访问控制里填了国家时才下载（约 10MB，压成约 3MB 的缓存文件），之后每 7 天更新一次；
// 缓存在数据目录 geoip.txt，重启直接读缓存。

var geoSources = []string{
	"https://ftp.apnic.net/stats/apnic/delegated-apnic-latest",
	"https://ftp.arin.net/pub/stats/arin/delegated-arin-extended-latest",
	"https://ftp.ripe.net/pub/stats/ripencc/delegated-ripencc-latest",
	"https://ftp.lacnic.net/pub/stats/lacnic/delegated-lacnic-latest",
	"https://ftp.afrinic.net/pub/stats/afrinic/delegated-afrinic-latest",
}

const geoRefresh = 7 * 24 * time.Hour

type geo4 struct {
	start, end uint32
	cc         [2]byte
}

type geo6 struct {
	start, end [2]uint64
	cc         [2]byte
}

type geoDB struct {
	v4      []geo4
	v6      []geo6
	updated time.Time
}

type geoState struct {
	mu      sync.Mutex
	err     string
	loading bool
	tried   time.Time
}

// parseGeo 解析 RIR 分配数据（registry|cc|type|start|value|date|status），返回按起始地址排序的区间。
func parseGeo(r io.Reader) (*geoDB, error) {
	db := &geoDB{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == '#' {
			continue
		}
		f := strings.Split(line, "|")
		if len(f) < 5 || len(f[1]) != 2 || f[1] == "ZZ" || f[3] == "*" {
			continue
		}
		var cc [2]byte
		copy(cc[:], strings.ToUpper(f[1]))
		switch f[2] {
		case "ipv4":
			ip := net.ParseIP(f[3]).To4()
			n, err := strconv.ParseUint(f[4], 10, 64)
			if ip == nil || err != nil || n == 0 {
				continue
			}
			s := binary.BigEndian.Uint32(ip)
			e := uint64(s) + n - 1
			if e > 0xffffffff {
				e = 0xffffffff
			}
			db.v4 = append(db.v4, geo4{s, uint32(e), cc})
		case "ipv6":
			ip := net.ParseIP(f[3])
			bits, err := strconv.Atoi(f[4])
			if ip == nil || ip.To4() != nil || err != nil || bits < 1 || bits > 128 {
				continue
			}
			s := ip6(ip.Mask(net.CIDRMask(bits, 128)))
			e := s
			host := 128 - bits
			if host >= 64 {
				e[0] |= (1<<(host-64) - 1)
				if host == 128 {
					e[0] = ^uint64(0)
				}
				e[1] = ^uint64(0)
			} else {
				e[1] |= (1<<host - 1)
			}
			db.v6 = append(db.v6, geo6{s, e, cc})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	sort.Slice(db.v4, func(i, j int) bool { return db.v4[i].start < db.v4[j].start })
	sort.Slice(db.v6, func(i, j int) bool { return less6(db.v6[i].start, db.v6[j].start) })
	return db, nil
}

func ip6(ip net.IP) [2]uint64 {
	ip = ip.To16()
	return [2]uint64{binary.BigEndian.Uint64(ip[:8]), binary.BigEndian.Uint64(ip[8:])}
}

func less6(a, b [2]uint64) bool { return a[0] < b[0] || (a[0] == b[0] && a[1] < b[1]) }

func (db *geoDB) lookup(ip net.IP) string {
	if db == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		x := binary.BigEndian.Uint32(v4)
		i := sort.Search(len(db.v4), func(i int) bool { return db.v4[i].start > x }) - 1
		if i >= 0 && x <= db.v4[i].end {
			return string(db.v4[i].cc[:])
		}
		return ""
	}
	x := ip6(ip)
	i := sort.Search(len(db.v6), func(i int) bool { return less6(x, db.v6[i].start) }) - 1
	if i >= 0 && !less6(db.v6[i].end, x) {
		return string(db.v6[i].cc[:])
	}
	return ""
}

func (db *geoDB) size() int {
	if db == nil {
		return 0
	}
	return len(db.v4) + len(db.v6)
}

func (m *Manager) geoLookup(ip net.IP) string {
	return m.geo.Load().lookup(ip)
}

func (m *Manager) geoLookupStr(s string) string {
	ip := net.ParseIP(s)
	if ip == nil || m.geo.Load() == nil {
		return ""
	}
	return m.geoLookup(ip)
}

func (m *Manager) geoPath() string { return filepath.Join(filepath.Dir(m.path), "geoip.txt") }

// loadGeoCache 启动时读缓存文件（有的话）。
func (m *Manager) loadGeoCache() {
	f, err := os.Open(m.geoPath())
	if err != nil {
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	db, err := parseGeo(f)
	if err != nil || db.size() == 0 {
		return
	}
	db.updated = st.ModTime()
	m.geo.Store(db)
}

// geoWanted 判断要不要国家数据：访问控制里填了国家。
func (m *Manager) geoWanted() bool {
	a := m.acl.Load()
	return a != nil && a.cfg.Mode != "off" && len(a.cfg.Countries) > 0
}

// geoLoop 需要时下载 / 更新国家数据。
func (m *Manager) geoLoop() {
	for {
		m.geoTick(time.Now())
		time.Sleep(time.Minute)
	}
}

func (m *Manager) geoTick(now time.Time) {
	if !m.geoWanted() {
		return
	}
	db := m.geo.Load()
	if db != nil && now.Sub(db.updated) < geoRefresh {
		return
	}
	m.geoSt.mu.Lock()
	if m.geoSt.loading || now.Sub(m.geoSt.tried) < time.Hour && m.geoSt.err != "" {
		m.geoSt.mu.Unlock()
		return
	}
	m.geoSt.loading, m.geoSt.tried = true, now
	m.geoSt.mu.Unlock()
	err := m.downloadGeo()
	m.geoSt.mu.Lock()
	m.geoSt.loading, m.geoSt.err = false, ""
	if err != nil {
		m.geoSt.err = err.Error()
		log.Printf("下载 IP 国家数据失败：%v", err)
	}
	m.geoSt.mu.Unlock()
}

var geoClient = &http.Client{Timeout: 3 * time.Minute}

func (m *Manager) downloadGeo() error {
	var all bytes.Buffer
	for _, src := range geoSources {
		resp, err := geoClient.Get(src)
		if err != nil {
			return fmt.Errorf("%s: %s", src, simplifyNetErr(err))
		}
		sc := bufio.NewScanner(io.LimitReader(resp.Body, 64<<20))
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		n := 0
		for sc.Scan() {
			l := sc.Text()
			// 只留需要的行，并去掉日期、状态等列，缓存文件小很多
			if f := strings.Split(l, "|"); len(f) >= 5 && (f[2] == "ipv4" || f[2] == "ipv6") && len(f[1]) == 2 {
				all.WriteString("|" + f[1] + "|" + f[2] + "|" + f[3] + "|" + f[4] + "\n")
				n++
			}
		}
		resp.Body.Close()
		if err := sc.Err(); err != nil || resp.StatusCode != 200 || n == 0 {
			return fmt.Errorf("%s: HTTP %d，%d 条", src, resp.StatusCode, n)
		}
	}
	db, err := parseGeo(bytes.NewReader(all.Bytes()))
	if err != nil {
		return err
	}
	db.updated = time.Now()
	if err := writeFileAtomic(m.geoPath(), all.Bytes()); err != nil {
		log.Printf("保存 IP 国家数据失败：%v", err)
	}
	m.geo.Store(db)
	log.Printf("IP 国家数据已更新：%d 段", db.size())
	return nil
}

type geoView struct {
	Ranges  int    `json:"ranges"`
	Updated int64  `json:"updated,omitempty"`
	Loading bool   `json:"loading,omitempty"`
	Err     string `json:"err,omitempty"`
}

func (m *Manager) geoView() geoView {
	v := geoView{}
	if db := m.geo.Load(); db != nil {
		v.Ranges, v.Updated = db.size(), db.updated.Unix()
	}
	m.geoSt.mu.Lock()
	v.Loading, v.Err = m.geoSt.loading, m.geoSt.err
	m.geoSt.mu.Unlock()
	return v
}
