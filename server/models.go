package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// ModelInfo 描述 Claude 模型元数据
type ModelInfo struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// DefaultOfflineModels 当前在售的官方 Claude 原生型号，不硬编码特定旧日期后缀
var DefaultOfflineModels = []ModelInfo{
	{
		ID:          "claude-3-7-sonnet-latest",
		Type:        "model",
		DisplayName: "Claude 3.7 Sonnet",
		CreatedAt:   "2025-02-19T00:00:00Z",
	},
	{
		ID:          "claude-3-5-sonnet-latest",
		Type:        "model",
		DisplayName: "Claude 3.5 Sonnet",
		CreatedAt:   "2024-10-22T00:00:00Z",
	},
	{
		ID:          "claude-3-5-haiku-latest",
		Type:        "model",
		DisplayName: "Claude 3.5 Haiku",
		CreatedAt:   "2024-10-22T00:00:00Z",
	},
	{
		ID:          "claude-3-opus-latest",
		Type:        "model",
		DisplayName: "Claude 3 Opus",
		CreatedAt:   "2024-02-29T00:00:00Z",
	},
}

// ModelManager 负责从上游 /v1/models 动态定期刷新模型清单，失败时自动保底使用缓存
type ModelManager struct {
	client      *http.Client
	upstreamURL string
	apiKey      string
	interval    time.Duration
	mu          sync.RWMutex
	cachedJSON  []byte
	models      []ModelInfo
	lastRefresh time.Time
	stopCh      chan struct{}
	stopped     bool
}

// NewModelManager 构造模型管理器并初始化离线保底缓存
func NewModelManager(client *http.Client, upstreamURL, apiKey string, interval time.Duration) *ModelManager {
	if interval <= 0 {
		interval = 1 * time.Hour
	}

	offlinePayload := map[string]interface{}{
		"data":     DefaultOfflineModels,
		"has_more": false,
		"first_id": DefaultOfflineModels[0].ID,
		"last_id":  DefaultOfflineModels[len(DefaultOfflineModels)-1].ID,
	}
	initialBytes, _ := json.Marshal(offlinePayload)

	mm := &ModelManager{
		client:      client,
		upstreamURL: upstreamURL,
		apiKey:      apiKey,
		interval:    interval,
		cachedJSON:  initialBytes,
		models:      append([]ModelInfo(nil), DefaultOfflineModels...),
		stopCh:      make(chan struct{}),
	}

	return mm
}

// Start 启动后台定期刷新任务
func (mm *ModelManager) Start(ctx context.Context) {
	// 启动时在后台立即尝试刷新一次
	go func() {
		_ = mm.Refresh(ctx)
	}()

	go func() {
		ticker := time.NewTicker(mm.interval)
		defer ticker.Stop()

		for {
			select {
			case <-ticker.C:
				_ = mm.Refresh(context.Background())
			case <-mm.stopCh:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
}

// Stop 停止后台刷新协程
func (mm *ModelManager) Stop() {
	mm.mu.Lock()
	if !mm.stopped {
		mm.stopped = true
		close(mm.stopCh)
	}
	mm.mu.Unlock()
}

// Refresh 向上游发起 /v1/models 请求并更新缓存
func (mm *ModelManager) Refresh(ctx context.Context) error {
	if mm.upstreamURL == "" {
		return nil
	}

	targetURL, err := url.JoinPath(mm.upstreamURL, "/v1/models")
	if err != nil {
		return err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, targetURL, nil)
	if err != nil {
		return err
	}

	if mm.apiKey != "" {
		req.Header.Set("x-api-key", mm.apiKey)
	}
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Accept", "application/json")

	resp, err := mm.client.Do(req)
	if err != nil {
		log.Printf("[ModelManager Debug] 上游 /v1/models 刷新失败，使用缓存保底: %v", err)
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[ModelManager Debug] 上游 /v1/models 返回状态码 %d，使用缓存保底", resp.StatusCode)
		return nil
	}

	var parsed struct {
		Data    []ModelInfo `json:"data"`
		HasMore bool        `json:"has_more"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		log.Printf("[ModelManager Debug] 解析上游模型列表失败: %v", err)
		return err
	}

	if len(parsed.Data) > 0 {
		encoded, err := json.Marshal(parsed)
		if err == nil {
			mm.mu.Lock()
			mm.cachedJSON = encoded
			mm.models = parsed.Data
			mm.lastRefresh = time.Now()
			mm.mu.Unlock()
			log.Printf("[ModelManager Info] 成功刷新模型列表 (共 %d 个模型)", len(parsed.Data))
		}
	}

	return nil
}

// GetModelsJSON 获取模型列表的 JSON 响应字节
func (mm *ModelManager) GetModelsJSON() []byte {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	return mm.cachedJSON
}

// GetModels 获取当前模型列表切片
func (mm *ModelManager) GetModels() []ModelInfo {
	mm.mu.RLock()
	defer mm.mu.RUnlock()
	res := make([]ModelInfo, len(mm.models))
	copy(res, mm.models)
	return res
}
