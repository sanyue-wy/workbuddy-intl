package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
)

// Config 定义 WorkBuddy 国际版（Anthropic / Claude 协议核心）专属配置
type Config struct {
	// 基础监听与转发
	ListenAddr        string            `json:"listen_addr"`
	UpstreamURL       string            `json:"upstream_url"`
	APIKey            string            `json:"api_key"`
	InboundToken      string            `json:"inbound_token"`
	ClaudeCodeVersion string            `json:"claude_code_version"`
	ModelMapping      map[string]string `json:"model_mapping"`
	LogLevel          string            `json:"log_level"`
	TimeoutSeconds    int               `json:"timeout_seconds"`
	MaxBodyMB         int               `json:"max_body_mb"`

	// 高性能流式传输与连接池参数
	MaxIdleConns          int  `json:"max_idle_conns"`
	MaxIdleConnsPerHost   int  `json:"max_idle_conns_per_host"`
	ResponseHeaderTimeout int  `json:"response_header_timeout"`
	BufferSizeKB          int  `json:"buffer_size_kb"`
	EnableWarmup          bool `json:"enable_warmup"`
	WarmupIntervalSeconds int  `json:"warmup_interval_seconds"`
	WarmupConcurrency     int  `json:"warmup_concurrency"`

	// 防风控与拟人化流控参数
	EnableFingerprint    bool    `json:"enable_fingerprint"`
	EnablePacing         bool    `json:"enable_pacing"`
	RatePerSec           float64 `json:"rate_per_sec"`
	BurstCapacity        int     `json:"burst_capacity"`
	EnableJitter         bool    `json:"enable_jitter"`
	JitterMedianMs       int     `json:"jitter_median_ms"`
	JitterP95Ms          int     `json:"jitter_p95_ms"`
	EnableCircuitBreaker bool    `json:"enable_circuit_breaker"`

	// WorkBuddy 专用通道适配器配置 (参考 dsh-our-free-model 架构)
	WorkBuddy WorkBuddyConfig `json:"workbuddy"`
}

var (
	globalConfig Config
	configLock   sync.RWMutex
)

// DefaultConfig 返回 WorkBuddy 国际版专属生产默认配置
func DefaultConfig() Config {
	return Config{
		ListenAddr:        "127.0.0.1:8780",
		UpstreamURL:       "https://api.anthropic.com",
		APIKey:            "",
		InboundToken:      "",
		ClaudeCodeVersion: "2.1.306",
		ModelMapping: map[string]string{
			"claude-3-7-sonnet": "claude-3-7-sonnet-latest",
			"claude-3-5-sonnet": "claude-3-5-sonnet-latest",
			"claude-3-5-haiku":  "claude-3-5-haiku-latest",
			"claude-3-opus":     "claude-3-opus-latest",
		},
		LogLevel:              "info",
		TimeoutSeconds:        180,
		MaxBodyMB:             32,
		MaxIdleConns:          200,
		MaxIdleConnsPerHost:   50,
		ResponseHeaderTimeout: 60,
		BufferSizeKB:          32,
		EnableWarmup:          true,
		WarmupIntervalSeconds: 30,
		WarmupConcurrency:     2,
		EnableFingerprint:     false,
		EnablePacing:          false,
		RatePerSec:            2.0,
		BurstCapacity:         5,
		EnableJitter:          false,
		JitterMedianMs:        300,
		JitterP95Ms:           650,
		EnableCircuitBreaker:  true,
		WorkBuddy:             DefaultWorkBuddyConfig(),
	}
}

// LoadConfig 从指定 JSON 路径读取并解析配置，支持 UTF-8 BOM 自动清洗
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			configLock.Lock()
			globalConfig = cfg
			configLock.Unlock()
			return &cfg, nil
		}
		return nil, fmt.Errorf("读取配置文件失败 (%s): %w", path, err)
	}

	// 清洗 UTF-8 BOM（针对 Windows 环境下编辑文件引入的 EF BB BF）
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))

	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("解析配置文件 JSON 失败: %w", err)
	}

	// 补全必要默认字段
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = "127.0.0.1:8780"
	}
	if cfg.UpstreamURL == "" {
		cfg.UpstreamURL = "https://api.anthropic.com"
	}
	if cfg.ClaudeCodeVersion == "" {
		cfg.ClaudeCodeVersion = "2.1.306"
	}
	if cfg.MaxBodyMB <= 0 {
		cfg.MaxBodyMB = 32
	}
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 180
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.BufferSizeKB <= 0 {
		cfg.BufferSizeKB = 32
	}
	if cfg.RatePerSec <= 0 {
		cfg.RatePerSec = 2.0
	}
	if cfg.BurstCapacity < 2 {
		cfg.BurstCapacity = 2
	}
	if cfg.WarmupIntervalSeconds <= 0 {
		cfg.WarmupIntervalSeconds = 30
	}
	if cfg.WarmupConcurrency <= 0 {
		cfg.WarmupConcurrency = 2
	}

	configLock.Lock()
	globalConfig = cfg
	configLock.Unlock()

	return &cfg, nil
}

// IsLoopbackAddr 校验监听地址是否为本地回环地址
func IsLoopbackAddr(addr string) bool {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	host = strings.TrimSpace(host)
	if host == "" {
		// 例如 ":8787" 绑定所有网卡，不是回环地址
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		return true
	}
	return false
}

// GetConfig 获取当前全局配置副本
func GetConfig() Config {
	configLock.RLock()
	defer configLock.RUnlock()
	return globalConfig
}
