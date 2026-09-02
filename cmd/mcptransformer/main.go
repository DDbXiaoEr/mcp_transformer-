package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	configPath := flag.String("config", "config.yaml", "path to YAML config file (defaults to config.yaml in the working directory)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	seen := map[string]bool{}
	for _, sc := range cfg.Servers {
		if err := validateServer(&sc); err != nil {
			log.Fatalf("invalid server config %q: %v", sc.Name, err)
		}
		if seen[sc.Endpoint] {
			log.Fatalf("duplicate endpoint: %s", sc.Endpoint)
		}
		seen[sc.Endpoint] = true
	}

	statusPath, controlPath := adminPaths(cfg.Admin)
	if err := validateAdminPath(statusPath, "admin.statusPath"); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	if err := validateAdminPath(controlPath, "admin.controlPath"); err != nil {
		log.Fatalf("invalid config: %v", err)
	}
	if statusPath == controlPath {
		log.Fatalf("admin.statusPath and admin.controlPath must differ")
	}

	mgr := newManager(ctx, cfg.Servers)

	mcpMux := http.NewServeMux()
	for _, name := range mgr.names() {
		ms, _ := mgr.get(name)
		mcpMux.Handle(ms.cfg.Endpoint, ms)
		if err := ms.start(); err != nil {
			log.Printf("skip server %q (%s): %v", ms.cfg.Name, ms.cfg.Endpoint, err)
			continue
		}
		log.Printf("mounted %q -> endpoint %s", ms.cfg.Name, ms.cfg.Endpoint)
	}

	adminMux := http.NewServeMux()
	adminMux.HandleFunc(statusPath, mgr.handleStatus)
	adminMux.HandleFunc(controlPath, mgr.handleControl)
	log.Printf("admin status API -> GET %s", statusPath)
	log.Printf("admin control API -> POST %s", controlPath)

	mcpAddr := cfg.Listen
	if mcpAddr == "" {
		mcpAddr = defaultListen
	}
	adminAddr := cfg.Admin.Listen
	if adminAddr == "" {
		adminAddr = defaultAdminListen
	}

	var servers []*http.Server
	if adminAddr == mcpAddr {
		// 同一地址时合并挂载，避免端口冲突。
		mcpMux.HandleFunc(statusPath, mgr.handleStatus)
		mcpMux.HandleFunc(controlPath, mgr.handleControl)
		servers = []*http.Server{{Addr: mcpAddr, Handler: mcpMux}}
		log.Printf("listening on %s (MCP + admin)", mcpAddr)
	} else {
		servers = []*http.Server{
			{Addr: mcpAddr, Handler: mcpMux},
			{Addr: adminAddr, Handler: adminMux},
		}
		log.Printf("listening on %s (MCP)", mcpAddr)
		log.Printf("listening on %s (admin)", adminAddr)
	}

	for _, srv := range servers {
		go func(srv *http.Server) {
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("http server: %v", err)
			}
		}(srv)
	}

	<-ctx.Done()
	log.Println("shutting down...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(shutdownCtx)
	}

	mgr.closeAll()
	log.Println("bye")
}
