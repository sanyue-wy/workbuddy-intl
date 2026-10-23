package server

import (
	"context"
	"math"
	"math/rand"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// 1. GCRA (Generic Cell Rate Algorithm) 漏桶平滑削峰限流器
// ---------------------------------------------------------------------------

// GCRA 实现通用信元速率算法 (Generic Cell Rate Algorithm, ITU-T I.371)
// 使用单标量 TAT (Theoretical Arrival Time) 跟踪，O(1) 内存空间与细粒度同步
type GCRA struct {
	mu           sync.Mutex
	rate         float64       // 每秒放行的事件数 (T = 1/rate)
	emissionUnit time.Duration // 单信元发射间隔 T
	tat          time.Duration // 理论到达时刻 (纳秒时间戳)
	tau          time.Duration // 突发提前容忍额度 = (capacity - 1) * T
	capacity     int           // 突发容量 (必须 >= 2，杜绝 capacity=1 的零方差机器人节拍)

	// 统计指标
	totalReserved uint64
	totalDelayed  uint64
	totalRefunded uint64
}

// GCRAStats GCRA 运行统计快照
type GCRAStats struct {
	RatePerSec    float64       `json:"rate_per_sec"`
	Capacity      int           `json:"capacity"`
	EmissionUnit  time.Duration `json:"emission_unit"`
	TotalReserved uint64        `json:"total_reserved"`
	TotalDelayed  uint64        `json:"total_delayed"`
	TotalRefunded uint64        `json:"total_refunded"`
}

// NewGCRA 创建 GCRA 漏桶平滑削峰限流器
// ratePerSec: 稳态每秒放行速率
// capacity: 突发容忍上限 (若传入 < 2 则强制抬升至 2，防止形成恒定周期机器指纹)
func NewGCRA(ratePerSec float64, capacity int) *GCRA {
	if ratePerSec <= 0 {
		ratePerSec = 2.0
	}
	// capacity 严格 >= 2，防零方差机器特征
	if capacity < 2 {
		capacity = 2
	}

	t := time.Duration(float64(time.Second) / ratePerSec)
	return &GCRA{
		rate:         ratePerSec,
		emissionUnit: t,
		tau:          time.Duration(capacity-1) * t,
		capacity:     capacity,
	}
}

// Reserve 申请在当前时刻放行 1 个基准事件，返回需要等待的时长
func (g *GCRA) Reserve(now time.Time) time.Duration {
	return g.ReserveCost(now, 1.0)
}

// ReserveCost 按可变成本 (如预估 Token 或请求权重) 申请放行，返回需等待的时长
func (g *GCRA) ReserveCost(now time.Time, costUnits float64) time.Duration {
	if costUnits <= 0 {
		costUnits = 1.0
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	atomic.AddUint64(&g.totalReserved, 1)

	base := now.UnixNano()
	tat := g.tat.Nanoseconds()
	if tat < base {
		tat = base
	}

	allowAt := tat - g.tau.Nanoseconds()
	var wait time.Duration
	if allowAt > base {
		wait = time.Duration(allowAt - base)
		atomic.AddUint64(&g.totalDelayed, 1)
	}

	// 推进 TAT: 增加 costUnits * T
	increment := time.Duration(costUnits * float64(g.emissionUnit))
	g.tat = time.Duration(tat) + increment

	return wait
}

// Refund 预留-对账机制：在实际 usage (如生成 Token 数) 小于初始预估时，回拨 TAT
// 避免因前期高估而对后续请求产生过度压制
func (g *GCRA) Refund(reservedUnits, actualUnits float64) {
	if reservedUnits <= actualUnits || reservedUnits <= 0 {
		return
	}

	savedUnits := reservedUnits - actualUnits
	g.mu.Lock()
	defer g.mu.Unlock()

	delta := time.Duration(savedUnits * float64(g.emissionUnit))
	g.tat -= delta

	// TAT 不应低于当前物理时间
	now := time.Duration(time.Now().UnixNano())
	if g.tat < now {
		g.tat = now
	}
	atomic.AddUint64(&g.totalRefunded, 1)
}

// UpdateFromHeaders 根据上游返回的 RateLimit 响应头动态微调 GCRA 状态
// 支持解析 anthropic-ratelimit-requests-remaining 等官方限流头
func (g *GCRA) UpdateFromHeaders(h http.Header) {
	if h == nil {
		return
	}

	// 检查剩余请求数
	remStr := strings.TrimSpace(h.Get("anthropic-ratelimit-requests-remaining"))
	if remStr != "" {
		if rem, err := strconv.Atoi(remStr); err == nil {
			if rem <= 2 {
				// 剩余配额逼近极限，动态加垫缓冲，将 TAT 向后推延 1 个周期避免触发 429
				g.mu.Lock()
				now := time.Duration(time.Now().UnixNano())
				if g.tat < now {
					g.tat = now
				}
				g.tat += g.emissionUnit
				g.mu.Unlock()
			}
		}
	}
}

// Stats 返回当前 GCRA 运行统计数据
func (g *GCRA) Stats() GCRAStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	return GCRAStats{
		RatePerSec:    g.rate,
		Capacity:      g.capacity,
		EmissionUnit:  g.emissionUnit,
		TotalReserved: atomic.LoadUint64(&g.totalReserved),
		TotalDelayed:  atomic.LoadUint64(&g.totalDelayed),
		TotalRefunded: atomic.LoadUint64(&g.totalRefunded),
	}
}

// ---------------------------------------------------------------------------
// 2. 带方差补偿的对数正态 + OU 过程拟人化微抖动 (Jitter)
// ---------------------------------------------------------------------------

// Jitter 生成符合人类思考与间歇停顿特征的右偏重尾随机延迟
// 引入 Ornstein-Uhlenbeck (OU) 均值回归随机过程模拟短期思考自相关性，
// 并通过数学方差补偿确保实际 P50/P95 严格锚定目标分布
type Jitter struct {
	mu      float64    // 对数空间均值 (ln(median))
	sigma   float64    // 补偿后的对数空间标准差
	floor   float64    // 最小延迟下限截断 (秒)
	ceil    float64    // 最大延迟上限截断 (秒)
	ouSpeed float64    // OU 过程回归速率 theta (0~1)
	ouNoise float64    // OU 过程扩散系数 sigma_ou
	ouState float64    // 当前 OU 状态
	r       *rand.Rand // 独立随机数发生器
	lock    sync.Mutex // 并发互斥锁

	// 统计指标
	sampleCount uint64
	totalTime   time.Duration
}

// NewJitter 创建带方差补偿的对数正态+OU过程拟人化抖动发生器
// medianMs: 目标中位数延迟 (毫秒，例如 300ms)
// p95Ms: 目标 95分位延迟 (毫秒，例如 650ms)
// floorMs: 硬下界截断 (毫秒，例如 50ms)
// ceilMs: 硬上界截断 (毫秒，例如 2000ms)
// ouNoise: OU 扩散噪声强度 (推荐 0.15~0.20，0 表示纯对数正态)
// seed: 随机种子 (传 0 时使用纳秒时间戳)
func NewJitter(medianMs, p95Ms, floorMs, ceilMs, ouNoise float64, seed int64) *Jitter {
	if medianMs <= 0 {
		medianMs = 300.0
	}
	if p95Ms <= medianMs {
		p95Ms = medianMs * 2.16
	}
	if floorMs <= 0 {
		floorMs = 30.0
	}
	if ceilMs <= p95Ms {
		ceilMs = p95Ms * 3.0
	}
	if ouNoise < 0 {
		ouNoise = 0
	}

	const ouSpeed = 0.15 // 默认 OU 回归速度
	if seed == 0 {
		seed = time.Now().UnixNano()
	}

	// 1. 计算离散 OU 过程稳态方差:
	// x_{t+1} = (1 - ouSpeed)*x_t + ouNoise*N(0,1)
	// Var(OU) = ouNoise^2 / (1 - (1 - ouSpeed)^2)
	a := 1.0 - ouSpeed
	ouVar := 0.0
	if ouNoise > 0 {
		ouVar = (ouNoise * ouNoise) / (1.0 - a*a)
	}

	// 2. 计算无 OU 时目标对数正态方差:
	// log(P95) - log(P50) = 1.6448536 * totalSigma
	medianSec := medianMs / 1000.0
	p95Sec := p95Ms / 1000.0
	mu := math.Log(medianSec)
	targetLogShift := math.Log(p95Sec) - mu
	const z95 = 1.6448536269514722
	totalVar := math.Pow(targetLogShift/z95, 2)

	// 3. 【核心数学创新】：方差补偿 (Variance Compensation)
	// 由于 Var(ln Y) = sigma^2 + Var(OU)，为保证加入 OU 过程后总方差不膨胀，
	// 必须从对数正态方差中扣除 OU 的方差贡献！
	compensatedVar := totalVar - ouVar
	if compensatedVar < 1e-9 {
		compensatedVar = 1e-9
	}
	sigma := math.Sqrt(compensatedVar)

	return &Jitter{
		mu:      mu,
		sigma:   sigma,
		floor:   floorMs / 1000.0,
		ceil:    ceilMs / 1000.0,
		ouSpeed: ouSpeed,
		ouNoise: ouNoise,
		ouState: 0.0,
		r:       rand.New(rand.NewSource(seed)),
	}
}

// Sample 生成一个拟人化随机延迟采样值，并发安全
func (j *Jitter) Sample() time.Duration {
	j.lock.Lock()
	defer j.lock.Unlock()

	// 1. Ornstein-Uhlenbeck 离散状态转移
	if j.ouNoise > 0 {
		j.ouState = (1.0-j.ouSpeed)*j.ouState + j.ouNoise*j.r.NormFloat64()
	}

	// 2. 混合对数正态分布与 OU 状态调制
	z := j.r.NormFloat64()
	valSec := math.Exp(j.mu + j.sigma*z + j.ouState)

	// 3. 极值平滑截断与软反射
	if valSec < j.floor {
		valSec = j.floor
	}
	if valSec > j.ceil {
		valSec = j.ceil
	}

	dur := time.Duration(valSec * float64(time.Second))
	j.sampleCount++
	j.totalTime += dur

	return dur
}

// Stats 返回 Jitter 运行统计
func (j *Jitter) Stats() (uint64, time.Duration) {
	j.lock.Lock()
	defer j.lock.Unlock()
	return j.sampleCount, j.totalTime
}

// ---------------------------------------------------------------------------
// 3. 综合流量起搏器 (Pacer)
// ---------------------------------------------------------------------------

// Pacer 统一调度 GCRA 稳态削峰限流与拟人化 Jitter 延迟注入
type Pacer struct {
	cfg    *Config
	gcra   *GCRA
	jitter *Jitter
}

// NewPacer 根据全局配置构建起搏器
func NewPacer(cfg *Config) *Pacer {
	var gcra *GCRA
	if cfg.EnablePacing {
		gcra = NewGCRA(cfg.RatePerSec, cfg.BurstCapacity)
	}

	var jitter *Jitter
	if cfg.EnableJitter {
		jitter = NewJitter(
			float64(cfg.JitterMedianMs),
			float64(cfg.JitterP95Ms),
			30.0, // floor 30ms
			float64(cfg.JitterP95Ms*3),
			0.15, // 0.15 OU 拟人扰动
			time.Now().UnixNano(),
		)
	}

	return &Pacer{
		cfg:    cfg,
		gcra:   gcra,
		jitter: jitter,
	}
}

// Pace 在请求转发前计算并执行防风控等待，支持外部 Context 取消
func (p *Pacer) Pace(ctx context.Context) error {
	return p.PaceWithCost(ctx, 1.0)
}

// PaceWithCost 支持指定权重成本的防风控等待
func (p *Pacer) PaceWithCost(ctx context.Context, costUnits float64) error {
	var totalWait time.Duration

	// 1. GCRA 平滑削峰等待
	if p.gcra != nil {
		totalWait += p.gcra.ReserveCost(time.Now(), costUnits)
	}

	// 2. 累加带方差补偿的对数正态+OU 拟人化抖动
	if p.jitter != nil {
		totalWait += p.jitter.Sample()
	}

	if totalWait <= 0 {
		return nil
	}

	select {
	case <-time.After(totalWait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// GCRA 获取底层 GCRA 实例
func (p *Pacer) GCRA() *GCRA {
	return p.gcra
}

// Jitter 获取底层 Jitter 实例
func (p *Pacer) Jitter() *Jitter {
	return p.jitter
}
