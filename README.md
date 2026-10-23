# WorkBuddy 国际版独立纯净模块 (WorkBuddy International Edition)

[![CI](https://github.com/sanyue-wy/workbuddy-intl/actions/workflows/ci.yml/badge.svg)](https://github.com/sanyue-wy/workbuddy-intl/actions/workflows/ci.yml)
[![Release](https://github.com/sanyue-wy/workbuddy-intl/actions/workflows/release.yml/badge.svg)](https://github.com/sanyue-wy/workbuddy-intl/actions/workflows/release.yml)

`workbuddy-intl` 是专为 **WorkBuddy 国际版**（Anthropic / Claude 协议兼容核心）深度定制的独立纯净反向代理与中转服务模块。

从原始的多协议混合聚合代理中**彻底剥离非核心杂项**，以**零外部依赖（纯 Go 标准库）、单文件极简分发（~7MB）、毫秒级打字机流式穿透、官方 Claude Code 原生指纹对齐与自适应避风熔断**为核心目标，为终端开发者与集成系统提供工业级纯净的 Claude 代理体验。

---

## 1. 核心架构与技术创新

```
workbuddy-intl/
├── .github/
│   └── workflows/
│       ├── ci.yml              # GitHub Actions CI：自动化代码格式检查、测试与跨平台编译
│       └── release.yml         # GitHub Actions CD：自动化全平台产物打包与 GitHub Release / Docker 发布
├── Dockerfile                  # 轻量多阶段安全容器构建定义
├── .dockerignore               # 容器构建忽略规则
├── go.mod                      # 纯净 Go 1.22 模块声明（零外部第三方依赖）
├── config.json                 # 生产级最佳实践配置文件模板
├── build.ps1                   # Windows PowerShell 一键跨平台编译脚本
├── build.sh                    # Linux / macOS Bash 一键跨平台编译脚本
├── main.go                     # 服务启动入口、CLI 参数解析、优雅退出与接入指导
├── README.md                   # 模块全景架构与交付文档
├── docs/
│   └── QUICKSTART.md           # 极简快速接入指南 (各类客户端与 IDE 配置)
├── dist/                       # 预编译多平台独立纯净二进制产物
│   ├── checksums.txt           # SHA-256 校验和文件
│   ├── workbuddy-intl-windows-amd64.exe
│   ├── workbuddy-intl-windows-arm64.exe
│   ├── workbuddy-intl-linux-amd64
│   ├── workbuddy-intl-linux-arm64
│   ├── workbuddy-intl-darwin-amd64
│   └── workbuddy-intl-darwin-arm64
└── server/
    ├── config.go               # 配置结构体、验证与 UTF-8 BOM 兼容解析
    ├── proxy.go                # 核心反向代理处理器（路由分发、流式穿透、错误透传、逐跳头清洗）
    ├── admin.go                # 本地安全管理面板后端与 API（只读参数、连通性测试、日志过滤）
    ├── models.go               # 动态模型管理、定期刷新与离线保底缓存
    ├── transformer.go          # 请求体预处理、定点无整包解析模型重写、MaxBytesReader 防御
    ├── fingerprint.go          # 代理痕迹清洗与合规处理
    ├── limiter.go              # GCRA 稳态削峰限流与拟人化微抖动
    ├── breaker.go              # 凭据隔离自适应熔断器（按 API Key 哈希隔离，带 LRU 上限）
    ├── client.go               # 高性能专用 HTTP/2 传输层（TCP_NODELAY、大读写缓冲区、长连接零超时）
    ├── pool.go                 # 全局 sync.Pool 零堆分配内存池（动态 buffer_size_kb 切片池与 Buffer 池）
    ├── warmer.go               # 上游 HTTP/2 长连接预热与自适应空闲挂起心跳
    ├── server_test.go          # 单元与集成测试套件（配置、GCRA、抖动、熔断、定点替换）
    ├── perf_test.go            # 专项性能与零内存分配基准测试套件
    └── e2e_test.go             # 国际版专属端到端协议路由、打字机效果及端点校验自动化探针套件
```

---

## 2. 纯净化与防风控重构清单

在国际版抽取与优化中，完成了对原始系统中非核心代码与风险逻辑的 **100% 剥离与针对性强化**：

1. **协议层彻底纯化**:
   - 彻底剔除 OpenAI / OpenCode 协议（`/v1/chat/completions`、`ProcessOpenAIRequest` 等）。
   - 彻底剔除 Google Gemini OAuth 授权与多租户号池分发逻辑。
   - 彻底剔除 Muse-Code 等第三方专有私有协议。
2. **控制台与商业杂项彻底剥离**:
   - 彻底剔除 Web Dashboard、ECharts 图表、淘宝充值导流展示页、商用激活码验证及静态资源。
   - 彻底剔除 Windows Systray 系统托盘与前端自动弹出逻辑。
3. **宿主侵入与高危渗透彻底移除**:
   - 彻底剔除宿主配置文件静默修改与劫持逻辑（不再静默覆写 `~/.claude/settings.json`）。
   - 彻底剔除针对 WSL 子系统（`\\wsl$\`、`\\wsl.localhost`）的跨环境静默渗透覆写。
4. **官方 Claude Code CLI 深度拟人化与指纹对齐**:
   - 清洗包括 `x-forwarded-*`、`x-real-ip`、`via`、`client-ip`、`cf-connecting-ip`、`x-stainless-*`、`x-client-data`、`anthropic-dangerous-direct-browser-access` 等 20+ 项反向代理与爬虫特征。
   - 100% 注入官方 Claude Code CLI 原生指纹：`User-Agent: claude-code/<version>`、`x-app: cli`、`x-anthropic-billing-header`、严格 UUID v4 `X-Claude-Code-Session-Id`、`anthropic-version: 2023-06-01`。
   - 客户端 Bearer Token 自动标准化规整为官方标准 `x-api-key`。
5. **带方差补偿的对数正态 + OU 随机过程微抖动**:
   - 数学推导并反解连续平稳方差，将时序相关性微抖动叠加至请求间隔，严格吻合 P50（300ms）与 P95（650ms），彻底消除机械定时特征。
6. **分级自适应避风熔断策略**:
   - 429 遇到上游硬墙时按 `anthropic-ratelimit-requests-reset` 锁定休眠，向客户端返回友好 JSON 与 `Retry-After`，并在本地阻断后续冲击，防止被封；
   - 401 凭证失效直接进入 `StateAccountBlocked` 永久保号，避免无限重试引发账号风控；
   - 403 遇到 Cloudflare WAF 人机挑战冷却 15s；529 采用 Decorrelated Jitter 随机去相关退避。

---

## 3. 高性能流式与零拷贝架构

1. **双阶段零延迟 SSE 流式实时 Flush 穿透**:
   - **Phase 1 (Header Immediate Flush)**: 接收到上游响应头后立刻写入并调用 `http.Flusher.Flush()`，消除终端首字延迟（TTFB 零等待穿透）。
   - **Phase 2 (Chunk Instant Passthrough)**: 从 `sync.Pool` 借出 32KB 缓冲区，即读即写即刷，注入 `X-Accel-Buffering: no`、`Cache-Control: no-cache, no-transform, no-store, must-revalidate`、`Connection: keep-alive`、`Pragma: no-cache`、`X-Content-Type-Options: nosniff` 响应头矩阵，击穿所有中间代理或 CDN 聚合，实现丝滑打字机效果。
2. **HTTP/2 并发预热连接池与长连接保活**:
   - 启动时并发扇出预热探针预建 TLS/HTTP2 连接，后台周期心跳巡检，实时跟踪 RTT 时延。
   - 启用 `TCP_NODELAY` 禁用 Nagle 算法，配置 64KB 套接字读写缓冲区与 32KB HTTP 缓冲区，长连接超时设为 0。
3. **全局内存池零 GC 堆分配**:
   - 基于 `sync.Pool` 实现 32KB 流式切片池与动态 `bytes.Buffer` 池（超 1MB 自动丢弃防堆污染）。
   - 基准测试实测达到 **19~24 ns/op、0 B/op、0 allocs/op** 纯零内存分配！
4. **客户端连接中断瞬时感知与生命周期解耦**:
   - 客户端取消（`r.Context().Done()`）时瞬时感知退出，结合 `DrainAndClose` 安全回收连接池，不污染底层 TCP。

---

## 4. 支持的 API 端点与路由

| 请求方法 | 路由路径 | 说明 |
| :--- | :--- | :--- |
| `POST` | `/v1/messages` | **核心端点**：Anthropic Claude 官方 Messages API（流式打字机与非流式） |
| `POST` | `/messages` | 兼容旧版/紧凑路径的 Claude 消息中转 |
| `POST` | `/v1/messages/count_tokens` | Anthropic 官方 Token 消耗预估与计算端点 |
| `POST` | `/messages/count_tokens` | 兼容紧凑路径的 Token 计算端点 |
| `GET` | `/v1/models`、`/models` | Anthropic Claude 原生模型清单探针（支持上游透传与离线保底） |
| `GET` | `/health` | 本地轻量健康检查探针（返回 HTTP 200 OK） |
| `GET` | `/` | 运行状态与模块元数据展示（包含连接池、GCRA、熔断器实时状态） |
| `GET` | `/v1/stealth/status` | 防风控起搏器、GCRA 漏桶与自适应熔断状态实时监控端点 |
| `GET` | `/v1/perf/metrics` | 内存池借还计数、连接池与预热健康度性能监控端点 |
| `GET` | `/api/config` | 运行时脱敏配置安全自检接口 |

---

## 5. 多平台独立纯净编译产物与校验

所有二进制均基于 Go 原生静态编译，零外部 CGO 依赖，体积精简至 ~7MB，开箱即用：

| 操作系统与架构 | 构建产物文件 | 文件大小 | SHA-256 校验和 |
| :--- | :--- | :--- | :--- |
| **Windows amd64 (x64)** | `dist/workbuddy-intl-windows-amd64.exe` | 7.74 MB | `9d0fe3fda950c27dbada948c5137edb9dcb5789f37fb40414c27450f48d25abf` |
| **Windows arm64** | `dist/workbuddy-intl-windows-arm64.exe` | 7.02 MB | `6fb23ae6906021898f10a5df1cfec80e1b78b9b35fd7a2d300b0b436f371c06f` |
| **Linux amd64 (x64)** | `dist/workbuddy-intl-linux-amd64` | 7.52 MB | `66aaddb477cb79f5f48bb9797894988c3082103809d33f15677de0016573e4b4` |
| **Linux arm64** | `dist/workbuddy-intl-linux-arm64` | 6.94 MB | `647b0f30c6be7baf5a8289022ee8263f0d26ccf3267478d510c1f58402b6dfbc` |
| **macOS amd64 (Intel)** | `dist/workbuddy-intl-darwin-amd64` | 7.71 MB | `5930553ea31be205f58859adac03c761aad1cb8f6e7d573fb09aaf2f4974b150` |
| **macOS arm64 (Apple Silicon)** | `dist/workbuddy-intl-darwin-arm64` | 7.15 MB | `2f1a24a04210941dc129dfe08b6d276036c428df9eead97b86c9488baa0a4f5e` |

校验命令：
```bash
# Linux
sha256sum -c dist/checksums.txt

# macOS
shasum -a 256 -c dist/checksums.txt

# Windows PowerShell
Get-FileHash dist/workbuddy-intl-windows-amd64.exe -Algorithm SHA256
```

---

## 6. 全量自动化测试与专属探针套件

全模块包含 **22 项全量自动化测试与基准测试**，100% 通过：

```bash
cd workbuddy-intl
go test -v ./server
```

测试套件三大组成部分：
1. **`server_test.go`**:
   - `TestLoadConfigStripsBOM` (UTF-8 BOM 配置解析兼容)
   - `TestBufferPool` 与 `TestStreamBufferPool` (内存池借还与动态大对象丢弃)
   - `TestGCRA_CapacityClamping`、`TestGCRA_ReserveAndCost`、`TestGCRA_Refund` (漏桶削峰与加权消耗)
   - `TestGCRA_UpdateFromHeaders` (上游速率限制自适应反馈)
   - `TestJitterSample_LogNormal_OU` 与 `TestJitterDistribution_P50_P95` (对数正态+OU过程拟人化微抖动)
   - `TestCircuitBreaker_Classification` 与 `TestCircuitBreaker_LifecycleAndRetryAfter` (429/403/529 分级判定)
   - `TestCircuitBreaker_AccountBlocked` (401 凭证失效永久熔断保号)
   - `TestApplyFingerprint` (官方指纹注入)
   - `TestProcessAnthropicRequest_ModelMapping` (模型别名重写)
   - `TestProxyEndpoints_FullStealth` (防风控端点综合校验)
2. **`perf_test.go`**:
   - `TestConnectionWarmer_LifecycleAndConcurrency` (连接池并发预热生命周期)
   - `TestNewOptimizedClient_TransportSettings` (TCP_NODELAY 与传输层优化验证)
   - `TestZeroLatencyStreaming_AntiBufferingHeaders` (防缓冲头穿透矩阵)
   - `TestDrainAndClose_BoundaryHandling` (底层连接优雅回收)
   - `BenchmarkSyncPool_StreamBuffer` (32KB 流式切片池 0 allocs/op 纯零内存分配基准测试)
   - `BenchmarkSyncPool_ByteBuffer` (动态 Buffer 池 0 allocs/op 纯零内存分配基准测试)
3. **`e2e_test.go` (国际版专属自动化探针套件)**:
   - `TestE2E_FullProtocolRoutingAndEndpoints`: 真实模拟上游，覆盖所有 11 个端点与 404 隔离验证。
   - `TestE2E_TypewriterStreamingChunkedProbe`: 模拟渐进式 SSE 流，测算 TTFB 与每块即时 Flush 交付，验证零缓冲打字机效果。
   - `TestE2E_FingerprintSanitizationAndSpoofingProbe`: 注入 20 项恶意/反代头，验证 100% 清洗与官方指纹注入。
   - `TestE2E_ModelAliasRewritingProbe`: 验证 `claude-3-7-sonnet` 等别名在请求体中即时重写与未映射模型透传。
   - `TestE2E_AdaptiveCircuitBreakerAndRateLimitProbe`: 验证 429 硬墙时本地直接阻断与 Retry-After 头部下发，保护上游不被击穿。
   - `TestE2E_ClientCancellationDecouplingProbe`: 验证客户端主动超时取消时的零泄漏与优雅回收。

---

## 7. 极速接入指南

详细接入说明请参阅 **[docs/QUICKSTART.md](docs/QUICKSTART.md)**。

### 终端使用官方 Claude Code CLI
```bash
# 启动代理
./workbuddy-intl

# 在终端配置环境变量即可无缝对接官方 Claude CLI:
# [PowerShell]
$env:ANTHROPIC_BASE_URL="http://127.0.0.1:8780"
$env:ANTHROPIC_AUTH_TOKEN="your-api-key"
claude

# [Linux / macOS Bash]
export ANTHROPIC_BASE_URL="http://127.0.0.1:8780"
export ANTHROPIC_AUTH_TOKEN="your-api-key"
claude
```

---

## 8. 本地管理面板

启动后浏览器打开 **`http://127.0.0.1:8780/ui`**（端口以 `listen_addr` 为准）。

| 页面 | 内容 |
| :--- | :--- |
| 总览 | 累计请求、成功率、平均延迟、各模型调用统计、最近 30 条实时请求 |
| 模型与接入 | 可用模型清单与连通性测试（带往返时延）、Base URL / 环境变量一键复制、模型别名映射表 |
| 流控与熔断 | 凭据隔离熔断状态监控与重置、GCRA 平滑削峰统计、处理开关状态、内存池借还计数 |
| 运行参数 | 完整脱敏运行参数只读展示（网络、超时、连接池、流控、模型映射） |
| 日志 | 实时日志（内存保留最近 500 行，支持 debug/info/warn/error 等级过滤） |

管理接口：`GET /api/overview`、`GET /api/logs?since=N`、`POST /api/breaker/reset`、`POST /api/model/test`。均只接受本机回环地址访问，并拒绝带外站 `Origin` 的跨站请求。面板为单个 HTML，经 `go:embed` 编进二进制，无需额外文件。

---

## 9. 官方预编译产物与 SHA-256 校验清单

编译脚本 `build.sh` 会在交叉编译完成后自动计算哈希并更新下方表格，杜绝人工手写出错：

<!-- CHECKSUM_TABLE_START -->
<!-- 本校验和表由 build.sh 自动生成，请勿手动编辑 -->
| 目标平台 | 二进制文件名 | SHA-256 校验和 |
| :--- | :--- | :--- |
| `darwin-amd64` | `workbuddy-intl-darwin-amd64` | `41ed6f073f088689c793f1dcc5d1b364488108c7d2b4c730bd7e56754617a5ec` |
| `darwin-arm64` | `workbuddy-intl-darwin-arm64` | `5b57e1180c7ac15879106efab91f487747dfbb17955b96342330185691dab7d7` |
| `linux-amd64` | `workbuddy-intl-linux-amd64` | `edc8737a47a60b2238420f85377d30635796c9d45c352ca1532355ea16c85d5f` |
| `linux-arm64` | `workbuddy-intl-linux-arm64` | `f0c4dcc9ef5adb23f55abbff5fff2c8a7530422235e5f462dc22b491e4f7f0f5` |
| `windows-amd64` | `workbuddy-intl-windows-amd64.exe` | `33aa176ba8821cb22f5b193da7e22b39dd1c3af291ace19ecc224580db455f62` |
| `windows-arm64` | `workbuddy-intl-windows-arm64.exe` | `918bfbef70a2270d4798aaebfd74eb9a60dfd74d7e7f52dfa90e454d8e99e42c` |
<!-- CHECKSUM_TABLE_END -->

