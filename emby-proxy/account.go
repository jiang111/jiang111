package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 站点账号：每个站点可以记一组上游 Emby 的用户名和密码（只做记录，登录面板后查看）。
// 隐私：
//   - 存在 config.json 的 accounts（站点 ID → 密文），不在站点结构里：站点列表、同步给从节点、导出都带不出去。
//   - 用户名和密码一起用 AES-256-GCM 加密，密钥单独放 data/account.key（0600）：只拿到 config.json 解不开。
//     附加数据是站点 ID，密文挪到别的站点下解不开。
//   - /api/state 只返回用户名和「有没有密码」；看明文要再输一次面板密码，输错和登录共用失败次数限额。
//   - 不记录账号内容，只记录谁在什么时候查看过；从节点不保存、不接受修改。

const (
	accountPrefix  = "v1:"
	maxAccountUser = 128
	maxAccountPass = 256
)

var errNoAccountKey = errors.New("缺少 account.key，已保存的账号无法解密，请重新填写")

type siteAccount struct {
	Username string `json:"u"`
	Password string `json:"p"`
}

// accountView 是面板列表里显示的账号信息（不含密码）。
type accountView struct {
	Username    string `json:"username,omitempty"`
	HasPassword bool   `json:"has_password"`
	Error       string `json:"error,omitempty"`
}

func (m *Manager) accountKeyPath() string { return filepath.Join(filepath.Dir(m.path), "account.key") }

// accountKey 返回加密账号用的密钥；create 为 true 时文件不存在就生成一个。
func (m *Manager) accountKey(create bool) ([]byte, error) {
	m.acctMu.Lock()
	defer m.acctMu.Unlock()
	if m.acctKey != nil {
		return m.acctKey, nil
	}
	data, err := os.ReadFile(m.accountKeyPath())
	if errors.Is(err, os.ErrNotExist) {
		if !create {
			return nil, errNoAccountKey
		}
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := writeFileAtomic(m.accountKeyPath(), []byte(hex.EncodeToString(key)+"\n")); err != nil {
			return nil, err
		}
		m.acctKey = key
		return key, nil
	}
	if err != nil {
		return nil, err
	}
	key, err := hex.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != 32 {
		return nil, errors.New("account.key 格式不正确")
	}
	m.acctKey = key
	return key, nil
}

func accountGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealAccount(key []byte, id string, a siteAccount) (string, error) {
	gcm, err := accountGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	pt, _ := json.Marshal(a)
	return accountPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, pt, []byte(id))), nil
}

func openAccount(key []byte, id, enc string) (siteAccount, error) {
	var a siteAccount
	bad := errors.New("账号无法解密（account.key 不是加密时的那个），请重新填写")
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(enc, accountPrefix))
	if !strings.HasPrefix(enc, accountPrefix) || err != nil {
		return a, bad
	}
	gcm, err := accountGCM(key)
	if err != nil {
		return a, err
	}
	if len(raw) < gcm.NonceSize() {
		return a, bad
	}
	pt, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], []byte(id))
	if err != nil || json.Unmarshal(pt, &a) != nil {
		return a, bad
	}
	return a, nil
}

// accountViewOf 在持有 m.mu 读锁时调用；key、keyErr 要在拿锁之前取好（accountKey 有自己的锁）。
func (m *Manager) accountViewOf(id string, key []byte, keyErr error) *accountView {
	enc := m.cfg.Accounts[id]
	if enc == "" {
		return nil
	}
	if keyErr != nil {
		return &accountView{Error: keyErr.Error()}
	}
	a, err := openAccount(key, id, enc)
	if err != nil {
		return &accountView{Error: err.Error()}
	}
	return &accountView{Username: a.Username, HasPassword: a.Password != ""}
}

// pruneAccounts 删掉已不存在的站点的账号，调用方持有 m.mu 写锁。
func (m *Manager) pruneAccounts() {
	if len(m.cfg.Accounts) == 0 {
		return
	}
	ids := map[string]bool{}
	for _, s := range m.cfg.Sites {
		ids[s.ID] = true
	}
	for id := range m.cfg.Accounts {
		if !ids[id] {
			delete(m.cfg.Accounts, id)
		}
	}
}

// siteAccountState 返回站点名称和已保存的密文；站点不存在时 ok 为 false。
func (m *Manager) siteAccountState(id string) (name, enc string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, s := range m.cfg.Sites {
		if s.ID == id {
			return s.Name, m.cfg.Accounts[id], true
		}
	}
	return "", "", false
}

// saveAccount：PUT /api/sites/{id}/account，密码留空则保留原来的密码。
func (a *Admin) saveAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	body.Username = strings.TrimSpace(body.Username)
	switch {
	case body.Username == "":
		writeErr(w, 400, "请填写用户名")
		return
	case len(body.Username) > maxAccountUser || len(body.Password) > maxAccountPass:
		writeErr(w, 400, "用户名或密码太长")
		return
	}
	m := a.m
	id := r.PathValue("id")
	_, old, ok := m.siteAccountState(id)
	if !ok {
		writeErr(w, 404, "站点不存在")
		return
	}
	key, err := m.accountKey(true)
	if err != nil {
		writeErr(w, 500, "读取 account.key 失败: "+err.Error())
		return
	}
	acc := siteAccount{Username: body.Username, Password: body.Password}
	if acc.Password == "" && old != "" {
		prev, err := openAccount(key, id, old)
		if err != nil {
			writeErr(w, 400, "原来的密码无法解密，请重新填写密码")
			return
		}
		acc.Password = prev.Password
	}
	enc, err := sealAccount(key, id, acc)
	if err != nil {
		writeErr(w, 500, "加密失败: "+err.Error())
		return
	}
	m.mu.Lock()
	exists := false
	for _, s := range m.cfg.Sites {
		exists = exists || s.ID == id
	}
	if exists {
		if m.cfg.Accounts == nil {
			m.cfg.Accounts = map[string]string{}
		}
		m.cfg.Accounts[id] = enc
	}
	m.mu.Unlock()
	if !exists {
		writeErr(w, 404, "站点不存在")
		return
	}
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *Admin) deleteAccount(w http.ResponseWriter, r *http.Request) {
	m := a.m
	m.mu.Lock()
	delete(m.cfg.Accounts, r.PathValue("id"))
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// revealAccount：POST /api/sites/{id}/account/reveal，带面板密码，返回明文。
// 输错计入登录失败次数（同一限额），防止借这个接口猜面板密码。
func (a *Admin) revealAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := readJSON(r, &body); err != nil {
		writeErr(w, 400, "请求格式错误")
		return
	}
	ip := clientIP(r)
	if !a.reserveLogin(ip) {
		writeErr(w, http.StatusTooManyRequests, "失败次数过多，请 10 分钟后再试")
		return
	}
	a.m.mu.RLock()
	pw := a.m.cfg.AdminPassword
	a.m.mu.RUnlock()
	if subtle.ConstantTimeCompare([]byte(body.Password), []byte(pw)) != 1 {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusForbidden, "面板密码错误")
		return
	}
	a.releaseLogin(ip)
	id := r.PathValue("id")
	name, enc, ok := a.m.siteAccountState(id)
	if !ok || enc == "" {
		writeErr(w, 404, "这个站点没有保存账号")
		return
	}
	key, err := a.m.accountKey(false)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	acc, err := openAccount(key, id, enc)
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	log.Printf("面板：%s 查看了站点「%s」的账号", ip, name)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"username": acc.Username, "password": acc.Password})
}
