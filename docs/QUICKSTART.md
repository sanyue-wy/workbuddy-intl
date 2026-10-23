# WorkBuddy 国际版极简接入与快速上手指南 (Quickstart Guide)

欢迎使用 **WorkBuddy 国际版纯净独立代理服务 (`workbuddy-intl`)**。
本模块专为 Anthropic Claude 协议深度定制，彻底剥离所有杂项、引流、Web 界面与 WSL 劫持逻辑，提供**零外部依赖、极低内存、毫秒级流式打字机穿透与深度防风控拟人化**的极致体验。

---

## 目录
1. [30秒极速上手（三步接入）](#1-30秒极速上手三步接入)
2. [与官方 Claude Code CLI 完美配合](#2-与官方-claude-code-cli-完美配合)
3. [常见开发工具与客户端接入](#3-常见开发工具与客户端接入)
4. [生产环境配置清单 (`config.json`)](#4-生产环境配置清单-configjson)
5. [系统监控与健康自检端点](#5-系统监控与健康自检端点)
6. [多平台独立编译产物与校验](#6-多平台独立编译产物与校验)
7. [常见问题解答 (FAQ)](#7-常见问题解答-faq)

---

## 1. 30秒极速上手（三步接入）

### 第一步：获取独立单二进制文件
从 `dist/` 目录获取对应操作系统的纯净可执行文件（每个仅约 7MB，无任何动态链接库依赖）：
- **Windows (x64)**: `dist/workbuddy-intl-windows-amd64.exe`
- **Windows (ARM64)**: `dist/workbuddy-intl-windows-arm64.exe`
- **Linux (x64)**: `dist/workbuddy-intl-linux-amd64`
- **Linux (ARM64)**: `dist/workbuddy-intl-linux-arm64`
- **macOS (Intel)**: `dist/workbuddy-intl-darwin-amd64`
- **macOS (Apple Silicon M1/M2/M3/M4)**: `dist/workbuddy-intl-darwin-arm64`

### 第二步：准备配置文件
在可执行文件同目录下放置 `config.json`（若不放置会自动采用内置最优默认配置）：
```json
{
  "listen_addr": "127.0.0.1:8780",
  "upstream_url": "https://api.anthropic.com",
  "api_key": "your-anthropic-api-key-here"
}
```
> **提示**：若 `api_key` 留空，代理将直接透明透传客户端请求所携带的 API Key 或 Bearer Token。

### 第三步：启动服务
```bash
# Windows（若从源码编译，文件在 dist 目录下）
.\dist\workbuddy-intl-windows-amd64.exe
# 或者直接运行当前目录下的二进制
.\workbuddy-intl.exe

# Linux / macOS (首次请赋予执行权限)
chmod +x ./dist/workbuddy-intl-linux-amd64
./dist/workbuddy-intl-linux-amd64
```
启动后终端将显示就绪提示与各 Shell 的一键接入命令：
```text
============================================================
  WorkBuddy 国际版独立纯净模块 v1.0.0 已成功启动
  本地监听地址: http://127.0.0.1:8787
  上游转发目标: https://api.anthropic.com
  协议支持: Anthropic Messages / Count Tokens / Models
============================================================
```

---

## 2. 与官方 Claude Code CLI 完美配合

WorkBuddy 国际版对官方 `claude-code` CLI 原生指纹进行了 100% 对齐（User-Agent、x-app: cli、billing header、严格 UUID v4 session ID），彻底剥离 20+ 项反代特征。

只需在终端设置环境变量，即可无感使用官方 Claude Code：

### PowerShell (Windows)
```powershell
$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
$env:ANTHROPIC_AUTH_TOKEN="your-api-key"

# 启动 Claude Code CLI
claude
```

### CMD 命令提示符 (Windows)
```cmd
set ANTHROPIC_BASE_URL=http://127.0.0.1:8787
set ANTHROPIC_AUTH_TOKEN=your-api-key

claude
```

### Linux / macOS (Bash / Zsh)
```bash
export ANTHROPIC_BASE_URL="http://127.0.0.1:8787"
export ANTHROPIC_AUTH_TOKEN="your-api-key"

claude
```

### 项目根目录 `.env` 方式
如果您在项目中使用自动化脚本或 direnv，可在 `.env` 中加入：
```env
ANTHROPIC_BASE_URL=http://127.0.0.1:8787
ANTHROPIC_AUTH_TOKEN=your-api-key
```

---

## 3. 常见开发工具与客户端接入

### A. Cursor IDE
1. 打开 Cursor 设置 (`Ctrl + ,` 或 `Cmd + ,`)。
2. 导航至 **Features** -> **Models**。
3. 展开 **Override Anthropic Base URL**，填写：`http://127.0.0.1:8787`。
4. 填写您的 Anthropic API Key。
5. 开启 `claude-3-7-sonnet` 或 `claude-3-5-sonnet`，即可享受零延迟流式代码生成。

### B. VS Code Continue 插件
在 `~/.continue/config.json` 中配置：
```json
{
  "models": [
    {
      "title": "Claude 3.7 Sonnet (WorkBuddy-Intl)",
      "provider": "anthropic",
      "model": "claude-3-7-sonnet-20250219",
      "apiKey": "your-api-key",
      "apiBase": "http://127.0.0.1:8787"
    }
  ]
}
```

### C. Roo Code / Cline 插件
1. 提供商选择 **Anthropic**。
2. Custom Base URL 填写：`http://127.0.0.1:8787`。
3. 输入 API Key，保存即可。

### D. Cherry Studio / Chatbox / NextChat
- **API 协议**：选择 `Anthropic`。
- **API 地址 (Host / Base URL)**：`http://127.0.0.1:8787`。
- **API Key**：填写您的有效 Key。

---

## 4. 生产环境配置清单 (`config.json`)

以下为全量推荐生产配置（已默认预设最优性能与防风控策略）：

```json
{
  "listen_addr": "127.0.0.1:8780",
  "upstream_url": "https://api.anthropic.com",
  "api_key": "",
  "claude_code_version": "2.1.306",
  "model_mapping": {
    "claude-3-7-sonnet": "claude-3-7-sonnet-20250219",
    "claude-3-5-sonnet": "claude-3-5-sonnet-20241022",
    "claude-3-5-haiku": "claude-3-5-haiku-20241022"
  },
  "log_level": "info",
  "timeout_seconds": 180,
  
  "enable_fingerprint": true,
  
  "enable_pacing": true,
  "rate_per_sec": 2.0,
  "burst_capacity": 5,
  "enable_jitter": true,
  "jitter_median_ms": 300,
  "jitter_p95_ms": 650,
  "enable_circuit_breaker": true,
  
  "enable_warmup": true,
  "warmup_interval_seconds": 30,
  "warmup_concurrency": 2,
  "max_idle_conns": 200,
  "max_idle_conns_per_host": 50,
  "response_header_timeout": 60,
  "buffer_size_kb": 32
}
```

### 关键配置项详解

| 参数项 | 类型 | 默认值 | 作用说明 |
| :--- | :--- | :--- | :--- |
| `listen_addr` | 字符串 | `"127.0.0.1:8787"` | 本地服务监听地址与端口 |
| `upstream_url` | 字符串 | `"https://api.anthropic.com"` | Anthropic 上游官方 API 地址 |
| `api_key` | 字符串 | `""` | 全局覆盖 Key（留空则透传客户端 Key） |
| `claude_code_version` | 字符串 | `"2.1.306"` | 拟真的官方 Claude Code 版本号 |
| `model_mapping` | 字典 | `{...}` | 请求模型别名自动映射字典 |
| `enable_fingerprint` | 布尔 | `true` | 是否启用 100% 官方指纹注入与反代清洗 |
| `enable_pacing` | 布尔 | `true` | 是否启用 GCRA 漏桶平滑削峰与防突发风控 |
| `enable_jitter` | 布尔 | `true` | 是否启用带方差补偿的对数正态+OU过程拟人化微抖动 |
| `enable_circuit_breaker`| 布尔 | `true` | 是否启用多级自适应避风熔断器 |
| `enable_warmup` | 布尔 | `true` | 是否启用 HTTP/2 连接池后台定时并发预热 |
| `buffer_size_kb` | 整数 | `32` | 内存池每块缓冲区大小（KB），推荐 32KB |

---

## 5. 系统监控与健康自检端点

服务内置零外部开销的探针端点，支持 Prometheus、curl 或健康探测探针直接拉取：

- **`GET /health`**：健康探测，快速返回 `200 OK`。
- **`GET /`**：服务主元数据看板，包含当前版本、连接池吞吐、GCRA 漏桶排队与熔断器指标。
- **`GET /v1/stealth/status`**：防风控专属监控，实时反馈熔断器状态（Closed/Open/HalfOpen/AccountBlocked）、GCRA 剩余容量及抖动统计。
- **`GET /v1/perf/metrics`**：流式性能与内存池监控，实时查看 `sync.Pool` 的借出与归还计数（验证 0 GC 分配）及预热连接 RTT。
- **`GET /api/config`**：当前运行配置（已脱敏 API Key）。

---

## 6. 多平台独立编译产物与校验

所有二进制均基于 Go 1.22 原生编译，构建参数：`CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath`，保证纯静态无 DLL 依赖：

### 预编译二进制清单

| 平台架构 | 交付文件 | 文件大小 | SHA-256 校验码 |
| :--- | :--- | :--- | :--- |
| **Windows amd64** | `dist/workbuddy-intl-windows-amd64.exe` | ~7.7 MB | `9d0fe3fda950c27dbada948c5137edb9dcb5789f37fb40414c27450f48d25abf` |
| **Windows arm64** | `dist/workbuddy-intl-windows-arm64.exe` | ~7.0 MB | `6fb23ae6906021898f10a5df1cfec80e1b78b9b35fd7a2d300b0b436f371c06f` |
| **Linux amd64** | `dist/workbuddy-intl-linux-amd64` | ~7.5 MB | `66aaddb477cb79f5f48bb9797894988c3082103809d33f15677de0016573e4b4` |
| **Linux arm64** | `dist/workbuddy-intl-linux-arm64` | ~6.9 MB | `647b0f30c6be7baf5a8289022ee8263f0d26ccf3267478d510c1f58402b6dfbc` |
| **macOS amd64** | `dist/workbuddy-intl-darwin-amd64` | ~7.7 MB | `5930553ea31be205f58859adac03c761aad1cb8f6e7d573fb09aaf2f4974b150` |
| **macOS arm64** | `dist/workbuddy-intl-darwin-arm64` | ~7.1 MB | `2f1a24a04210941dc129dfe08b6d276036c428df9eead97b86c9488baa0a4f5e` |

### 本地编译脚本
若需自行重新编译全部平台：
- **PowerShell (Windows)**: 执行 `.\build.ps1`
- **Bash (Linux / macOS)**: 执行 `./build.sh`

---

## 7. 常见问题解答 (FAQ)

### Q1: 遭遇上游 429 频控或 529 过载时，代理会怎么处理？
**答**：`workbuddy-intl` 内置自适应熔断避风器。
1. 当解析到上游 429 且带有统一重置标头时，熔断器自动进入冷却休眠，并在给客户端的响应中返回标准的 `Retry-After`，同时本地拦截后续密集并发请求，避免恶性冲击上游导致硬封控。
2. 当上游返回 401 鉴权失效时，自动进入 `StateAccountBlocked` 永久熔断保号模式，阻断死循环重试。

### Q2: 为什么打字机流式效果如此流畅？
**答**：本模块采用双阶段流式穿透架构：
- **Phase 1**：响应头首字节强刷（TTFB 零等待穿透），消除首字时延。
- **Phase 2**：从 `sync.Pool` 借用 32KB 缓冲区，即读即写即刷，注入 `X-Accel-Buffering: no`、`Cache-Control: no-cache` 等 5 项防缓冲标头，击穿中间 CDN/反代聚合。

### Q3: 如何自定义监听端口或指定配置文件？
**答**：
```bash
./workbuddy-intl -listen 127.0.0.1:9000 -config /path/to/my-config.json
```
命令行参数优先级高于 `config.json` 文件。
