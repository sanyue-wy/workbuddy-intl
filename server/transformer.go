package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// ErrBodyTooLarge 请求体超出限制错误
var ErrBodyTooLarge = errors.New("request body too large")

// AnthropicRequestMeta 提取请求的关键元数据供流控与流式处理使用
type AnthropicRequestMeta struct {
	Model       string `json:"model"`
	Stream      bool   `json:"stream"`
	MaxTokens   int    `json:"max_tokens"`
	HasMessages bool   `json:"has_messages"`
}

// ReplaceModelFixedPoint 使用 json.Decoder 扫描 model 字段，命中映射时执行定点字节替换，
// 严格保持其余所有字节、字段顺序与格式不变，避免整包反序列化与重新编码的开销。
func ReplaceModelFixedPoint(bodyBytes []byte, mapping map[string]string) ([]byte, string, bool) {
	if len(mapping) == 0 || len(bodyBytes) == 0 {
		return bodyBytes, "", false
	}

	dec := json.NewDecoder(bytes.NewReader(bodyBytes))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return bodyBytes, "", false
	}

	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return bodyBytes, "", false
		}
		key, ok := keyTok.(string)
		if !ok {
			continue
		}
		if key == "model" {
			keyEnd := dec.InputOffset()
			valTok, err := dec.Token()
			if err != nil {
				return bodyBytes, "", false
			}
			valEnd := dec.InputOffset()
			oldModel, ok := valTok.(string)
			if !ok {
				return bodyBytes, "", false
			}

			mappedModel, hit := mapping[oldModel]
			if !hit || mappedModel == oldModel {
				return bodyBytes, oldModel, false
			}

			// 在 bodyBytes[keyEnd:valEnd] 区间内定位冒号、开引号与闭引号
			segment := bodyBytes[keyEnd:valEnd]
			colonIdx := bytes.IndexByte(segment, ':')
			if colonIdx < 0 {
				return bodyBytes, oldModel, false
			}
			afterColon := segment[colonIdx:]
			quoteStartInSeg := bytes.IndexByte(afterColon, '"')
			if quoteStartInSeg < 0 {
				return bodyBytes, oldModel, false
			}
			quoteStart := keyEnd + int64(colonIdx) + int64(quoteStartInSeg)
			quoteEnd := bytes.LastIndexByte(bodyBytes[:valEnd], '"')
			if quoteEnd <= int(quoteStart) {
				return bodyBytes, oldModel, false
			}

			var out bytes.Buffer
			out.Grow(len(bodyBytes) - len(oldModel) + len(mappedModel))
			out.Write(bodyBytes[:quoteStart+1])
			out.WriteString(mappedModel)
			out.Write(bodyBytes[quoteEnd:])
			return out.Bytes(), mappedModel, true
		}

		if err := skipJSONValue(dec); err != nil {
			return bodyBytes, "", false
		}
	}
	return bodyBytes, "", false
}

func skipJSONValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if _, ok := tok.(json.Delim); !ok {
		return nil
	}
	depth := 1
	for depth > 0 {
		t, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := t.(json.Delim); ok {
			if d == '{' || d == '[' {
				depth++
			} else if d == '}' || d == ']' {
				depth--
			}
		}
	}
	return nil
}

// ProcessAnthropicRequestIntoBuffer 针对高并发请求管道的零 GC 分配处理
// 直接将请求体读取入 caller 借出的 targetBuf，并在需要重写时直接复用缓冲
func ProcessAnthropicRequestIntoBuffer(r *http.Request, cfg *Config, targetBuf *bytes.Buffer) ([]byte, error) {
	if r.Body == nil {
		return nil, nil
	}

	targetBuf.Reset()
	_, err := io.Copy(targetBuf, r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) || strings.Contains(strings.ToLower(err.Error()), "too large") {
			return nil, ErrBodyTooLarge
		}
		return nil, err
	}
	_ = r.Body.Close()

	bodyBytes := targetBuf.Bytes()
	if len(bodyBytes) == 0 {
		r.Body = http.NoBody
		return nil, nil
	}

	// 使用定点替换逻辑重写模型，保持字段顺序和原有格式完全一致
	if len(cfg.ModelMapping) > 0 {
		newBytes, mappedModel, replaced := ReplaceModelFixedPoint(bodyBytes, cfg.ModelMapping)
		if replaced {
			log.Printf("[ModelMap] 将请求模型重写为 %s (定点替换保持原结构与顺序)", mappedModel)
			targetBuf.Reset()
			targetBuf.Write(newBytes)
			bodyBytes = targetBuf.Bytes()
		}
	}

	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	return bodyBytes, nil
}

// ProcessAnthropicRequest 预处理 Anthropic 格式的请求体（校验合法性、提取元数据、映射重写模型）
// 采用 BufferPool 降低 GC 逃逸与高频内存分配
func ProcessAnthropicRequest(r *http.Request, cfg *Config) ([]byte, error) {
	tempBuf := GetBuffer()
	defer PutBuffer(tempBuf)

	b, err := ProcessAnthropicRequestIntoBuffer(r, cfg, tempBuf)
	if err != nil || len(b) == 0 {
		return b, err
	}

	cloned := make([]byte, len(b))
	copy(cloned, b)
	return cloned, nil
}

// InspectAnthropicRequest 从已读取的请求体字节中快速嗅探元数据（只读探针）
func InspectAnthropicRequest(bodyBytes []byte) AnthropicRequestMeta {
	var meta AnthropicRequestMeta
	if len(bodyBytes) == 0 {
		return meta
	}

	var raw struct {
		Model     string          `json:"model"`
		Stream    bool            `json:"stream"`
		MaxTokens int             `json:"max_tokens"`
		Messages  json.RawMessage `json:"messages"`
	}

	if err := json.Unmarshal(bodyBytes, &raw); err == nil {
		meta.Model = raw.Model
		meta.Stream = raw.Stream
		meta.MaxTokens = raw.MaxTokens
		meta.HasMessages = len(raw.Messages) > 0
	}
	return meta
}
