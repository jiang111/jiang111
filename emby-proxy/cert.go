package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 上传的证书保存在数据目录 ssl/ 下，nginx 配置引用其在宿主机上的路径。

func (m *Manager) sslDir() string { return filepath.Join(filepath.Dir(m.path), "ssl") }

// hostSSLDir 返回宿主机上看到的 ssl 目录：Docker 里运行时由 HOST_DATA_DIR 指定（compose 里已设置），
// 直接运行二进制时就是数据目录的绝对路径。
func (m *Manager) hostSSLDir() string {
	if d := os.Getenv("HOST_DATA_DIR"); d != "" {
		return filepath.Join(d, "ssl")
	}
	d, err := filepath.Abs(m.sslDir())
	if err != nil {
		return m.sslDir()
	}
	return d
}

type CertInfo struct {
	Names    []string `json:"names"`
	NotAfter int64    `json:"not_after"`
	Issuer   string   `json:"issuer"`
	Uploaded bool     `json:"uploaded"`
}

func parseLeaf(certPEM, keyPEM []byte) (*x509.Certificate, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		msg := err.Error()
		switch {
		case strings.Contains(msg, "private key does not match"):
			return nil, errors.New("证书和私钥不匹配")
		case strings.Contains(msg, "failed to find any PEM data in certificate"):
			return nil, errors.New("证书文件不是 PEM 格式（应为 fullchain.pem / .crt / .cer）")
		case strings.Contains(msg, "failed to find any PEM data in key"), strings.Contains(msg, "failed to find \"PRIVATE KEY\""):
			return nil, errors.New("私钥文件不是 PEM 格式（应为 privkey.pem / .key）")
		}
		return nil, errors.New("证书或私钥无效: " + msg)
	}
	return x509.ParseCertificate(pair.Certificate[0])
}

func certInfoOf(leaf *x509.Certificate) *CertInfo {
	names := leaf.DNSNames
	if len(names) == 0 && leaf.Subject.CommonName != "" {
		names = []string{leaf.Subject.CommonName}
	}
	issuer := leaf.Issuer.CommonName
	if len(leaf.Issuer.Organization) > 0 {
		issuer = leaf.Issuer.Organization[0]
	}
	return &CertInfo{Names: names, NotAfter: leaf.NotAfter.Unix(), Issuer: issuer}
}

// uploadedCert 读取已上传的证书（没有则返回 nil）。按文件修改时间缓存，面板每 3 秒轮询不必每次解析。
func (m *Manager) uploadedCert() *x509.Certificate {
	cp, kp := filepath.Join(m.sslDir(), "fullchain.pem"), filepath.Join(m.sslDir(), "privkey.pem")
	ci, err1 := os.Stat(cp)
	ki, err2 := os.Stat(kp)
	if err1 != nil || err2 != nil {
		return nil
	}
	key := ci.ModTime().String() + "|" + ki.ModTime().String() + "|" + fmt.Sprint(ci.Size(), ki.Size())
	m.certMu.Lock()
	defer m.certMu.Unlock()
	if key == m.certKey {
		return m.certLeaf
	}
	m.certKey, m.certLeaf = key, nil
	c, err1 := os.ReadFile(cp)
	k, err2 := os.ReadFile(kp)
	if err1 == nil && err2 == nil {
		m.certLeaf, _ = parseLeaf(c, k)
	}
	return m.certLeaf
}

func readUpload(r *http.Request, field string) ([]byte, error) {
	f, _, err := r.FormFile(field)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 1<<20))
}

func writeFileAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// uploadCert 接收浏览器选择的证书和私钥文件，校验后保存，并让 nginx 配置引用它们。
func (a *Admin) uploadCert(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	if err := r.ParseMultipartForm(4 << 20); err != nil {
		writeErr(w, 400, "上传失败: "+err.Error())
		return
	}
	certPEM, err1 := readUpload(r, "cert")
	keyPEM, err2 := readUpload(r, "key")
	if err1 != nil || err2 != nil {
		writeErr(w, 400, "请同时选择证书文件和私钥文件")
		return
	}
	leaf, err := parseLeaf(certPEM, keyPEM)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if time.Now().After(leaf.NotAfter) && r.FormValue("allow_expired") != "1" {
		writeErr(w, 400, "证书已过期（"+leaf.NotAfter.Format("2006-01-02")+"），如仍要使用请勾选「允许上传已过期的证书」")
		return
	}
	m := a.m
	if err := os.MkdirAll(m.sslDir(), 0o700); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := writeFileAtomic(filepath.Join(m.sslDir(), "privkey.pem"), keyPEM); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	if err := writeFileAtomic(filepath.Join(m.sslDir(), "fullchain.pem"), certPEM); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	m.mu.Lock()
	m.cfg.NginxCert = filepath.Join(m.hostSSLDir(), "fullchain.pem")
	m.cfg.NginxKey = filepath.Join(m.hostSSLDir(), "privkey.pem")
	m.mu.Unlock()
	if err := m.save(); err != nil {
		writeErr(w, 500, "保存失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "cert": certInfoOf(leaf)})
}

// certInfo 返回 nginx 配置当前引用的证书信息：引用的是上传的证书时能读取到详情。
func (m *Manager) certInfo() *CertInfo {
	leaf := m.uploadedCert()
	if leaf == nil {
		return nil
	}
	info := certInfoOf(leaf)
	info.Uploaded = m.cfg.NginxCert == filepath.Join(m.hostSSLDir(), "fullchain.pem")
	return info
}
