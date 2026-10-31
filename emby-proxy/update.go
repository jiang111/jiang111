package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// 在线更新：面板上检查 GitHub 上 emby-proxy 目录的最新提交，一键更新到它。
// 程序跑在容器里，不能自己重建镜像，所以分两半：
//   - 程序把要更新到的提交写进 数据目录/update/request；
//   - 宿主机上安装脚本装好的 systemd path 单元（emby-proxy-update.path）看到这个文件，执行 update.sh：
//     下载该提交的源码 → 重新执行 install.sh（构建新镜像，构建失败时旧容器照常运行）→ 结果写到 update/status。
// 多台服务器时，从节点发现主节点的版本和自己不同，会自动更新到主节点的版本（同一版本 6 小时内最多试一次），
// 整个集群只要在主节点点一下。

var commit = "" // 构建时由 -ldflags "-X main.commit=…" 写入（install.sh 记录的提交）

const updateRepo = "jiang111/jiang111"

var shaRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func appVersion() string {
	if commit == "" {
		return version
	}
	return version + "-" + shortSHA(commit)
}

type latestInfo struct {
	SHA     string `json:"sha"`
	Date    string `json:"date"`
	Message string `json:"message"`
	Err     string `json:"err,omitempty"`
	at      time.Time
}

type updater struct {
	mu       sync.Mutex
	latest   latestInfo
	followed map[string]time.Time // 从节点：自动跟随更新过的目标提交 → 时间
}

var githubAPI = "https://api.github.com" // 测试时替换

func (m *Manager) updateDir() string { return filepath.Join(filepath.Dir(m.path), "update") }

// hasUpdater 判断宿主机装了更新服务（install.sh 会写这个标记文件）。
func (m *Manager) hasUpdater() bool {
	_, err := os.Stat(filepath.Join(m.updateDir(), "installed"))
	return err == nil
}

// checkLatest 查 GitHub 上 emby-proxy 目录的最新提交（缓存 10 分钟）。
func (m *Manager) checkLatest(force bool) latestInfo {
	u := &m.upd
	u.mu.Lock()
	if !force && time.Since(u.latest.at) < 10*time.Minute && u.latest.SHA != "" {
		l := u.latest
		u.mu.Unlock()
		return l
	}
	u.mu.Unlock()
	l := latestInfo{at: time.Now()}
	req, _ := http.NewRequest("GET", githubAPI+"/repos/"+updateRepo+"/commits?sha=master&path=emby-proxy&per_page=1", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		l.Err = "连不上 GitHub：" + simplifyNetErr(err)
	} else {
		var out []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message   string `json:"message"`
				Committer struct {
					Date string `json:"date"`
				} `json:"committer"`
			} `json:"commit"`
		}
		err := json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		switch {
		case resp.StatusCode != 200:
			l.Err = fmt.Sprintf("GitHub 返回 HTTP %d（可能是请求太频繁，稍后再试）", resp.StatusCode)
		case err != nil || len(out) == 0:
			l.Err = "GitHub 返回内容无法解析"
		default:
			msg, _, _ := strings.Cut(out[0].Commit.Message, "\n")
			l.SHA, l.Date, l.Message = out[0].SHA, out[0].Commit.Committer.Date, msg
		}
	}
	u.mu.Lock()
	u.latest = l
	u.mu.Unlock()
	return l
}

// requestUpdate 让宿主机更新到指定提交。
func (m *Manager) requestUpdate(sha string) error {
	if !shaRe.MatchString(sha) {
		return errors.New("提交号不正确")
	}
	if !m.hasUpdater() {
		return errors.New("宿主机没有安装更新服务：在服务器上重新执行一次安装命令即可（之后就能在面板一键更新）")
	}
	if st := m.updateStatus(); st.State == "running" && time.Since(time.Unix(st.Time, 0)) < 15*time.Minute {
		return errors.New("正在更新中，请稍候")
	}
	if err := os.MkdirAll(m.updateDir(), 0o755); err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(m.updateDir(), "request"), []byte(sha+"\n"))
}

type updateStatus struct {
	State  string `json:"state,omitempty"` // running | ok | fail
	Time   int64  `json:"time,omitempty"`
	Target string `json:"target,omitempty"`
	Log    string `json:"log,omitempty"`
}

// updateStatus 读宿主机 update.sh 写的状态（第 1 行状态、第 2 行时间、第 3 行目标提交），以及日志末尾。
func (m *Manager) updateStatus() updateStatus {
	var st updateStatus
	data, err := os.ReadFile(filepath.Join(m.updateDir(), "status"))
	if err != nil {
		return st
	}
	lines := strings.SplitN(string(data), "\n", 4)
	if len(lines) >= 3 {
		st.State, st.Target = strings.TrimSpace(lines[0]), strings.TrimSpace(lines[2])
		fmt.Sscan(lines[1], &st.Time)
	}
	if lg, err := os.ReadFile(filepath.Join(m.updateDir(), "log")); err == nil {
		if len(lg) > 4000 {
			lg = lg[len(lg)-4000:]
		}
		st.Log = string(lg)
	}
	return st
}

// followMaster：从节点发现主节点版本不同时自动更新过去。
func (m *Manager) followMaster(masterCommit string) {
	if masterCommit == "" || commit == "" || masterCommit == commit || !shaRe.MatchString(masterCommit) || !m.hasUpdater() {
		return
	}
	u := &m.upd
	u.mu.Lock()
	if u.followed == nil {
		u.followed = map[string]time.Time{}
	}
	if t, ok := u.followed[masterCommit]; ok && time.Since(t) < 6*time.Hour {
		u.mu.Unlock()
		return
	}
	u.followed[masterCommit] = time.Now()
	u.mu.Unlock()
	if err := m.requestUpdate(masterCommit); err != nil {
		log.Printf("跟随主节点更新失败：%v", err)
		return
	}
	log.Printf("主节点版本 %s 和本机 %s 不同，已请求自动更新", shortSHA(masterCommit), shortSHA(commit))
}

// ---- 面板 ----

func (a *Admin) updateInfo(w http.ResponseWriter, r *http.Request) {
	m := a.m
	l := m.checkLatest(r.URL.Query().Get("force") == "1")
	writeJSON(w, 200, map[string]any{
		"version": appVersion(), "commit": commit, "latest": l,
		"behind":      commit != "" && l.SHA != "" && l.SHA != commit,
		"unknown":     commit == "",
		"has_updater": m.hasUpdater(), "status": m.updateStatus(),
	})
}

func (a *Admin) startUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SHA string `json:"sha"`
	}
	readJSON(r, &body)
	if body.SHA == "" {
		l := a.m.checkLatest(true)
		if l.SHA == "" {
			writeErr(w, 502, l.Err)
			return
		}
		body.SHA = l.SHA
	}
	if err := a.m.requestUpdate(body.SHA); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "target": body.SHA})
}
