package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"workbuddy-intl/server"
)

const version = "1.0.0"

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	listenAddr := flag.String("listen", "", "本地监听地址 (默认 127.0.0.1:8780)")
	upstreamURL := flag.String("upstream", "", "上游 Anthropic API 服务地址 (默认 https://api.anthropic.com)")
	apiKey := flag.String("key", "", "上游 Anthropic API 访问密钥 (可选，留空则透传客户端请求头中的密钥)")
	inboundToken := flag.String("token", "", "入站访问令牌 (X-Proxy-Token，监听非回环地址时必填)")
	showVersion := flag.Bool("version", false, "显示版本号")
	flag.Parse()

	if *showVersion {
		fmt.Printf("WorkBuddy International Edition v%s\n", version)
		os.Exit(0)
	}

	log.SetFlags(log.Ldate | log.Ltime | log.Lshortfile)
	server.InstallLogRing()
	server.AppVersion = version
	fmt.Printf(`
==============================================================
 WorkBuddy International Edition (Claude Core Forwarder) v%s
 Pure Anthropic / Claude Protocol Proxy & Anti-Risk Engine
==============================================================
`, version)

	cfg, err := server.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("载入配置失败: %v", err)
	}

	// 命令行参数覆盖
	if *listenAddr != "" {
		cfg.ListenAddr = *listenAddr
	}
	if *upstreamURL != "" {
		cfg.UpstreamURL = *upstreamURL
	}
	if *apiKey != "" {
		cfg.APIKey = *apiKey
	}
	if *inboundToken != "" {
		cfg.InboundToken = *inboundToken
	}

	// 安全约束：监听地址不是回环地址且未设置 inbound_token 时，拒绝启动
	if !server.IsLoopbackAddr(cfg.ListenAddr) && cfg.InboundToken == "" {
		log.Fatalf("[WorkBuddy-Intl Fatal] 监听地址非回环地址 (%s) 且未设置 inbound_token，拒绝启动以保障安全", cfg.ListenAddr)
	}

	fmt.Printf("[配置] 监听地址: %s\n", cfg.ListenAddr)
	fmt.Printf("[配置] 上游端点: %s\n", cfg.UpstreamURL)
	fmt.Printf("[配置] 拟真版本: claude-code/%s\n", cfg.ClaudeCodeVersion)
	fmt.Printf("[配置] 防风控起搏: GCRA=%v (%.1f req/s), Jitter=%v (~%dms)\n",
		cfg.EnablePacing, cfg.RatePerSec, cfg.EnableJitter, cfg.JitterMedianMs)
	fmt.Printf("[配置] HTTP/2连接: 预热=%v, MaxIdle=%d\n", cfg.EnableWarmup, cfg.MaxIdleConns)
	if cfg.InboundToken != "" {
		fmt.Printf("[配置] 入站安全: 已启用 X-Proxy-Token 鉴权\n")
	}
	if cfg.APIKey != "" {
		fmt.Printf("[配置] API 密钥: 已配置 (将自动注入上游 x-api-key)\n")
	} else {
		fmt.Printf("[配置] API 密钥: 未配置 (透传客户端传递的凭据)\n")
	}

	handler := server.NewProxyHandler(cfg)

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second, // 防范 Slowloris 攻击
		WriteTimeout:      0,                // 关键：流式长连接设为 0，防止回答生成过久被腰斩
	}

	// 打印客户端环境配置指引
	printClientGuide(cfg.ListenAddr)
	fmt.Printf("\n管理面板: http://%s/ui\n", cfg.ListenAddr)

	// 启动后台 HTTP 服务
	go func() {
		log.Printf("代理服务正在启动: http://%s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP 服务异常退出: %v", err)
		}
	}()

	// 监听中断退出信号
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, os.Interrupt, syscall.SIGTERM)
	<-quit

	log.Println("正在优雅关闭 WorkBuddy 国际版代理服务...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("服务关闭出错: %v", err)
	}
	handler.Close()
	log.Println("WorkBuddy 国际版代理服务已安全退出。")
}

func printClientGuide(addr string) {
	fmt.Println("\n--- [Claude Code CLI 客户端接入指引] ---")
	fmt.Printf("若要让 Claude Code CLI 走 WorkBuddy 国际版本地代理，请在终端执行：\n\n")
	fmt.Println("【PowerShell】:")
	fmt.Printf("  $env:ANTHROPIC_BASE_URL=\"http://%s\"\n", addr)
	fmt.Println("  $env:ANTHROPIC_AUTH_TOKEN=\"your-api-key\"")
	fmt.Println("【CMD 命令行】:")
	fmt.Printf("  set ANTHROPIC_BASE_URL=http://%s\n", addr)
	fmt.Println("  set ANTHROPIC_AUTH_TOKEN=your-api-key")
	fmt.Println("【Linux / WSL / macOS Bash】:")
	fmt.Printf("  export ANTHROPIC_BASE_URL=\"http://%s\"\n", addr)
	fmt.Println("  export ANTHROPIC_AUTH_TOKEN=\"your-api-key\"")
	fmt.Println("----------------------------------------")
}
