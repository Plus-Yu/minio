# MinIO S3 协议栈 Breakdown 插桩文档

> 版本: v1 · 日期: 2026-07-09 · 基准: `/home/yu/projects/minio` 主线

---

## 1. 概述

在 MinIO 请求链路中插入计时点，将每个 S3 请求的 wall-clock 延迟分解为 4 个阶段，以 Prometheus histogram 暴露。

**目标**: P0（标准 S3/HTTP/TCP/MinIO 全栈）vs P1（S3-Lite 二进制/TCP）的协议栈开销对比。

### 4 个输出指标

| Prometheus label | 名义含义 | 实际测量 |
|-----------------|---------|---------|
| `phase="http_parse"`   | HTTP 解析 | 最外层中间件入口 → Auth 中间件入口 |
| `phase="auth_crypto"`  | 认证/加密 | Auth 中间件外层耗时（日期校验 + 类型检测） |
| `phase="handler_logic"`| 业务逻辑 | Handler 执行时间 − I/O 等待 |
| `phase="io_wait"`      | I/O 等待 | 所有 Read/Write 系统调用的用户态阻塞时间 |

---

## 2. 文件变更清单

| 文件 | 操作 | 行数 | 说明 |
|------|------|------|------|
| `cmd/breakdown-timing.go` | **新建** | 98 行 | `BreakdownTiming` 结构体、`Prometheus HistogramVec`、最外层中间件 |
| `cmd/breakdown-io.go` | **新建** | 49 行 | `timingReadCloser`、`timingResponseWriter` |
| `cmd/routers.go` | **修改** | +4 行 | `breakdownTimingMiddleware` 插入 `globalMiddlewares` 首位 |
| `cmd/auth-handler.go` | **修改** | +11 行 | `setAuthMiddleware`: 入口打 T1，3 处 `h.ServeHTTP` 前打 T2 |
| `cmd/http-tracer.go` | **修改** | +4 行 | `httpTrace` 中将 `tc.FuncName` 写入 `BreakdownTiming.Operation` |
| `cmd/metrics-v3.go` | **修改** | +2 行 | 添加 `/s3/breakdown` 路径，注册到 V3 子 Registry |

### 变更详情

#### 2.1 `cmd/breakdown-timing.go`

```go
type BreakdownTiming struct {
    T0, T1, T2, T3 time.Time  // 4 个时间戳
    Operation      string     // 由 httpTrace 设置 (e.g. "s3.GetObjectHandler")
    IOWaitTotal    int64      // atomic，由 I/O 包裹器累加 (ns)
}

var breakdownDuration = prometheus.NewHistogramVec(
    prometheus.HistogramOpts{
        Name:    "minio_s3_breakdown_duration_seconds",
        Buckets: prometheus.ExponentialBuckets(0.00001, 2, 20), // 10µs ~ 5s
    },
    []string{"phase", "operation", "method"},
)
```

**中间件逻辑**: 创建 `BreakdownTiming` → 注入 context → 包裹 `ResponseWriter` + `r.Body` → `h.ServeHTTP` → 读 `bt.Operation` 和 `bt.IOWaitTotal` → 写 4 个 Prometheus 观测值。

#### 2.2 `cmd/breakdown-io.go`

两个 Decorator，每次 `Read()`/`Write()` 通过 `atomic.AddInt64` 累加 wall-clock 耗时到 `bt.IOWaitTotal`：

```go
func (t *timingReadCloser) Read(p []byte) (int, error) {
    start := time.Now()
    n, err := t.ReadCloser.Read(p)
    atomic.AddInt64(&t.bt.IOWaitTotal, int64(time.Since(start)))
    return n, err
}
```

`timingResponseWriter.Write()` 同理。

#### 2.3 `cmd/routers.go`

```go
var globalMiddlewares = []mux.MiddlewareFunc{
    breakdownTimingMiddleware,   // ← 最外层 (新增)
    addCustomHeadersMiddleware,
    httpTracerMiddleware,
    setAuthMiddleware,
    // ...
}
```

#### 2.4 `cmd/auth-handler.go`

```go
func setAuthMiddleware(h http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // T1: HTTP 解析结束，Auth 开始
        if bt := getBreakdown(r); bt != nil { bt.T1 = time.Now() }
        // ... 原有 authType 判断 ...
        // T2: Auth 结束，Handler 入口 (3 处 h.ServeHTTP 之前)
        if bt := getBreakdown(r); bt != nil { bt.T2 = time.Now() }
        h.ServeHTTP(w, r)
    })
}
```

#### 2.5 `cmd/http-tracer.go`

```go
tc.FuncName = getOpName(runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name())
// propagate to breakdown timing
if bt, ok := r.Context().Value(breakdownCtxKey{}).(*BreakdownTiming); ok {
    bt.Operation = tc.FuncName
}
```

#### 2.6 `cmd/metrics-v3.go`

```go
breakdownCollectorPath collectorPath = "/s3/breakdown"

collectors := map[collectorPath]prometheus.Collector{
    debugGoCollectorPath:   collectors.NewGoCollector(),
    breakdownCollectorPath: breakdownDuration,   // 新增
}
```

---

## 3. 使用方法

### 3.1 构建 & 启动

```bash
cd /home/yu/projects/minio
go build -o minio .
MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD=admin123 ./minio server /tmp/data
```

### 3.2 获取认证 Token

V3 metrics 端点需要 Bearer Token 认证：

```bash
~/go/bin/mc alias set myminio http://localhost:9000 admin admin123
TOKEN=$(~/go/bin/mc admin prometheus generate myminio 2>&1 | awk '/bearer_token/{print $2}')
```

### 3.3 压测 + 查指标

```bash
# warp 压测
~/go/bin/warp get --host=localhost:9000 --access-key=admin --secret-key=admin123 \
  --obj.size=1MiB --duration=30s --concurrent=10

# 查看 breakdown
curl -s -H "Authorization: Bearer $TOKEN" \
  localhost:9000/minio/metrics/v3/s3/breakdown
```

### 3.4 读指标

```
# 典型输出 (1MiB GetObject, 1000 次请求):

_sum{phase="http_parse"}    0.0421    → avg = 42.1 μs
_sum{phase="auth_crypto"}   0.0653    → avg = 65.3 μs
_sum{phase="handler_logic"} 0.3512    → avg = 351.2 μs
_sum{phase="io_wait"}       2.1024    → avg = 2.10 ms
                                    ─────────
                                     total ≈ 2.56 ms
```

### 3.5 对比 P0 vs P1

```
P0 协议栈开销 ≈ P0(http_parse + auth_crypto)
    → P1 中完全不存在这两层

P0 EC/ObjectLayer 开销 ≈ P0(handler_logic + io_wait) − P1(对应时间)
    → Erasure Coding + ObjectLayer 抽象 + 磁盘 I/O 差异
```

---

## 4. 精度评估

### 4.1 测量点覆盖

```
                   ┌── 不在测量范围内 ──┐   ┌────── 在测量范围内 ───────────────────────┐
TCP Accept → TLS 握手 → Go HTTP Parse → T0 → T1 → T2 → Handler + I/O → T3
                                           ↑        ↑
                                     http_parse  auth_crypto
                                     (高估5-20μs) (低估: 内部SigV4归入handler_logic)
```

### 4.2 各 phase 偏差

| phase | 偏差 | 量级 | 原因 |
|-------|------|------|------|
| `http_parse` | **高估** | ~5–20 μs | 包含 addCustomHeaders + httpTracer 开销 |
| `auth_crypto` | **低估** | ~20–60 μs | `doesSignatureMatch()` 在 handler 内执行 |
| `handler_logic` | **高估** | ~20–60 μs | 包含了内部 SigV4 验证 + HTTP 响应构造 |
| `io_wait` (Write) | **低估** | 不定 | 写入 Go bufio.Writer buffer (4KB)，flush 可能不在 Write 调用内 |

**核心 issue**: `setAuthMiddleware` 调用 `h.ServeHTTP(w, r)` 后，请求被转发到 handler 链，真正的 SigV4 签名验证 `doesSignatureMatch()` 在 handler 内部执行（Go 调用栈约 5 层深，位于 `object-handlers.go → signature-v4.go`）。Auth 中间件只能捕获外层的日期校验和类型检测，核心的 4×HMAC-SHA256 计算被归入 `handler_logic`。

### 4.3 I/O 测量

- **Read 侧（准确）**: `r.Body.Read()` 在 socket 无数据时阻塞，返回时数据已到达用户空间。捕获了完整的内核等待 + 拷贝时间。
- **Write 侧（低估）**: `ResponseWriter.Write()` 写入 Go 4KB bufio buffer，实际 socket send 在 Flush 时异步发生。大对象最后一个 chunk 的 flush 耗时可能未被计入 io_wait。

### 4.4 Instrumentation 开销

| 操作 | /请求频次 | 单次 | 累积 |
|------|----------|------|------|
| `time.Now()` | 4 | ~100 ns | ~400 ns |
| `atomic.AddInt64` | ~17 (1MiB GET, 64K buf) | ~10 ns | ~170 ns |
| `context.WithValue` | 1 | ~200 ns | ~200 ns |
| Wrapper dispatch | ~18 | ~5 ns | ~90 ns |
| Prometheus Observe | 4 | ~200 ns | ~800 ns |
| **总计** | | | **~1.7 μs/req** |

- **1MiB GetObject (~3ms)**: 开销 < **0.06%**
- **1KiB 小对象 (~200μs)**: 开销 < **0.9%**

---

## 5. 已知限制

| 限制 | 影响 | 解决方案 |
|------|------|---------|
| TLS 开销不可测 | TLS 握手在 `crypto/tls` 包内部，MinIO handler 外部 | P2: 包裹 `net.Listener` 或 eBPF |
| TCP Accept 不可测 | TCP 三次握手在内核 | P2: 包裹 `net.Listener` 或 bpftrace |
| Auth 内部 SigV4 漏测 | `doesSignatureMatch` 归入 handler_logic | P1.5: 在 `signature-v4.go` 打点 |
| Write 侧 I/O 低估 | bufio 缓冲延迟发送 | P2: 包裹 `net.Conn.Write()` |
| 分布式 Grid RPC | 节点间通信 I/O 归入 io_wait | eBPF 拆分 |

---

## 6. 后续改进路线

| 优先级 | 改进 | 文件 | 工时 |
|--------|------|------|------|
| P1.5 | `doesSignatureMatch` 打点，分离 SigV4 核心开销 | `signature-v4.go` | 30min |
| P2 | 包裹 `net.Listener` + `net.Conn` | `internal/http/server.go` | 2h |
| 外部 | eBPF 追踪 TCP/kernel 栈 | bpftrace 脚本 | 3h |
