package main

import (
	_ "embed"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const version = "1.0.0"

//go:embed web/index.html
var indexHTML []byte

func main() {
	cfgPath := flag.String("config", envOr("CONFIG", "data/config.json"), "配置文件路径")
	flag.Parse()
	log.SetFlags(log.LstdFlags)

	cfg, created, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("读取配置失败: %v", err)
	}
	if created {
		log.Printf("已生成配置文件 %s", *cfgPath)
		log.Printf("面板密码: %s （可在面板「设置」里修改）", cfg.AdminPassword)
	}

	m := NewManager(*cfgPath, cfg)
	m.mu.Lock()
	m.reload()
	m.mu.Unlock()

	admin := &Admin{m: m, failures: map[string][]time.Time{}}
	go serve("面板", cfg.AdminListen, admin.Handler())
	go serve("入口(路径/域名模式)", cfg.Listen, m)

	go func() {
		for range time.Tick(time.Minute) {
			m.saveStats()
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	m.saveStats()
	log.Println("已退出")
}

func serve(name, addr string, h http.Handler) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("%s 监听 %s 失败: %v", name, addr, err)
	}
	log.Printf("%s 监听 %s", name, addr)
	log.Fatal(newServer(h).Serve(ln))
}
