package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
)

var (
	defaultSessionID string
	sessionOnce      sync.Once
	uuidRegex        = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// getOrCreateSessionID 获取或初始化全局会话 ID（UUID v4 格式）
func getOrCreateSessionID() string {
	sessionOnce.Do(func() {
		b := make([]byte, 16)
		if _, err := rand.Read(b); err != nil {
			defaultSessionID = "00112233-4455-6677-8899-aabbccddeeff"
			return
		}
		// 格式化为标准 UUID v4: 8-4-4-4-12
		b[6] = (b[6] & 0x0f) | 0x40 // Version 4
		b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
		defaultSessionID = fmt.Sprintf("%s-%s-%s-%s-%s",
			hex.EncodeToString(b[0:4]),
			hex.EncodeToString(b[4:6]),
			hex.EncodeToString(b[6:8]),
			hex.EncodeToString(b[8:10]),
			hex.EncodeToString(b[10:16]),
		)
	})
	return defaultSessionID
}

// GenerateUUID 生成一个新的标准 UUID v4 字符串
func GenerateUUID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "00112233-4455-6677-8899-aabbccddeeff"
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]),
	)
}

// IsValidUUID 校验字符串是否为合法 UUID 格式
func IsValidUUID(u string) bool {
	return uuidRegex.MatchString(strings.TrimSpace(u))
}

// NegativeHeaderPrefixes 必须彻底剔除的第三方 SDK / 代理前缀
var NegativeHeaderPrefixes = []string{
	"x-stainless-",
	"x-workbuddy-",
	"x-openai-",
	"x-envoy-",
	"x-nginx-",
}

// NegativeHeaders 明确必须剔除的代理痕迹与特征泄露请求头
var NegativeHeaders = []string{
	"x-forwarded-for",
	"x-forwarded-host",
	"x-forwarded-proto",
	"x-forwarded-server",
	"x-real-ip",
	"via",
	"forwarded",
	"client-ip",
	"x-client-ip",
	"x-cluster-client-ip",
	"true-client-ip",
	"cf-connecting-ip",
	"cf-visitor",
	"fastly-client-ip",
	"anthropic-dangerous-direct-browser-access",
	"x-anthropic-source",
	"x-client-data",
}

// ApplyFingerprint 清理各类代理转发头（去除 x-forwarded-* 等转发痕迹，不伪造任何客户端身份特征）
func ApplyFingerprint(req *http.Request, cfg *Config) {
	if !cfg.EnableFingerprint {
		return
	}

	// 1. 只保留去除 x-forwarded-* 等转发头的功能，不新增任何伪装能力
	for _, h := range NegativeHeaders {
		req.Header.Del(h)
	}

	for k := range req.Header {
		lowerK := strings.ToLower(k)
		for _, prefix := range NegativeHeaderPrefixes {
			if strings.HasPrefix(lowerK, prefix) {
				req.Header.Del(k)
				break
			}
		}
	}

	// 2. 默认协议版本保底与 API Key 规整
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	if cfg.APIKey != "" {
		req.Header.Set("x-api-key", cfg.APIKey)
		req.Header.Del("Authorization")
	} else if auth := req.Header.Get("Authorization"); auth != "" {
		token := strings.TrimPrefix(auth, "Bearer ")
		token = strings.TrimPrefix(token, "bearer ")
		if strings.TrimSpace(token) != "" {
			req.Header.Set("x-api-key", strings.TrimSpace(token))
			req.Header.Del("Authorization")
		}
	}
}

// VerifyFingerprint 验证请求转发头已清洗干净，返回发现的违规项列表（空切片表示完美通过）
func VerifyFingerprint(req *http.Request, expectedVersion string) []string {
	var violations []string

	// 检查负向头残留
	for _, h := range NegativeHeaders {
		if req.Header.Get(h) != "" {
			violations = append(violations, fmt.Sprintf("残留负向代理头: %s", h))
		}
	}
	for k := range req.Header {
		lowerK := strings.ToLower(k)
		for _, prefix := range NegativeHeaderPrefixes {
			if strings.HasPrefix(lowerK, prefix) {
				violations = append(violations, fmt.Sprintf("残留黑名单前缀头: %s", k))
			}
		}
	}

	return violations
}
