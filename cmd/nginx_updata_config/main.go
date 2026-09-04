package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"nginx_updata_config/internal/application/publisher"
	"nginx_updata_config/internal/config"
	"nginx_updata_config/internal/infrastructure/applog"
	"nginx_updata_config/internal/interfaces/httpapi"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		applog.LogError("HTTP 发布服务退出", "service_failed", map[string]any{"error": err.Error()})
		os.Exit(1)
	}
}
func run() error {
	configPath := flag.String("config", getenv("NGINX_RELEASE_CONFIG", "configs/service.example.yaml"), "HTTP 发布服务配置文件")
	showVersion := flag.Bool("version", false, "打印版本")
	check := flag.Bool("check-config", false, "只校验配置，不启动或改动发布状态")
	adopt := flag.String("adopt-target", "", "离线导入旧 latest -> 完整提交；指定 target_id")
	adoptCommit := flag.String("adopt-commit", "", "旧快照的完整提交 ID")
	adoptBranch := flag.String("adopt-branch", "", "旧提交所属允许分支")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return nil
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		return fmt.Errorf("加载配置: %w", err)
	}
	if err = applog.Init(cfg.LogFile); err != nil {
		return err
	}
	if *check {
		applog.LogInfo("配置有效", "config_valid", map[string]any{"node_id": cfg.NodeID, "targets": len(cfg.Targets)})
		return nil
	}
	if *adopt != "" {
		ctx, cancel := context.WithTimeout(context.Background(), cfg.ExecutionTimeout.Value()+cfg.RecoveryTimeout.Value())
		defer cancel()
		return publisher.AdoptBaseline(ctx, cfg, *adopt, *adoptBranch, *adoptCommit)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	r, err := publisher.New(cfg)
	if err != nil {
		return err
	}
	defer r.Close()
	if ctx.Err() != nil {
		return nil
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	server := &http.Server{ErrorLog: log.New(applog.ErrorWriter{}, "", 0), Addr: cfg.ListenAddr, Handler: httpapi.New(r, cfg).Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	applog.LogInfo("HTTP 发布服务启动", "service_started", map[string]any{"listen_addr": listener.Addr().String(), "node_id": cfg.NodeID, "env": cfg.Env, "release_contract": 2})
	select {
	case err = <-done:
		if err != http.ErrServerClosed {
			return err
		}
		return nil
	case <-ctx.Done():
	}
	r.Stop()
	// Shutdown must finish in the main goroutine, including detached activation/recovery work.
	shutdown, cancel := context.WithTimeout(context.Background(), cfg.ExecutionTimeout.Value()+2*cfg.RecoveryTimeout.Value()+cfg.CleanupTimeout.Value()+10*time.Second)
	defer cancel()
	err = server.Shutdown(shutdown)
	if waitErr := r.Wait(shutdown); waitErr != nil {
		return waitErr
	}
	if err != nil {
		return err
	}
	applog.LogInfo("HTTP 发布服务已停止", "service_stopped", nil)
	return nil
}
func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
