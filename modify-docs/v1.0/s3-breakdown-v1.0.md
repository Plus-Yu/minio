# MinIO S3 协议栈 Breakdown 插桩 — v1.0 (P2: Connection-Level Timing)

> 日期: 2026-07-09 · 上一版: v0.1

## v1.0 改进

| 改进项 | v0.1 | v1.0 |
|--------|------|------|
| I/O 测量层 | ResponseWriter.Write + r.Body.Read（bufio 缓冲，Write 低估） | **net.Conn.Read/Write**（原始 socket） |
| TLS + Go HTTP parse | 不可测（T0 之前） | **T0 − TCPAccept** 纳入 http_parse |
| http_parse 覆盖 | T05−T0 ≈ 0.4μs（仅 breakdown setup） | T0−TCPAccept ≈ 50μs–5ms（完整 TLS+parse） |

---

## 1. 测量点（v1.0）

```
TCP Accept (kern) → [TCPAccept] ─TLS握手─→ T0 → T05 → T1 → T2 → Handler → T3
       ↑                 ↑                                      ↑       ↑
    内核空间       timingListener.Accept()                 auth_crypto  │
                  (after TLS if enabled)                 =(T2−T1)+AuthTotal
                                                                       │
          connTimingWrapper.Read()/Write() ──→ IOWaitTotal (diff from T0 snapshots)
```

| Phase | 公式 | 内容 |
|-------|------|------|
| `http_parse` | **T0 − TCPAccept** | TLS 握手 + Go HTTP 请求行/头解析 |
| `auth_crypto` | **(T2 − T1) + AuthTotal** | 外层日期校验 + 内层完整 SigV4 |
| `handler_logic` | **(T3 − T2) − IOWaitTotal − AuthTotal** | S3 语义 + EC 编解码 + 元数据 |
| `io_wait` | **conn Read/Write diff** (T0→T3) | 原始 socket Read/Write |

---

## 2. 新增文件

### `internal/http/conn-hooks.go`

```go
type ConnTiming struct {
    AcceptTime time.Time // when Accept returned (post-TLS)
    readTotal  int64     // socket Read accumulator (atomic)
    writeTotal int64     // socket Write accumulator (atomic)
}

// timingListener: wraps net.Listener, emits connTimingWrapper per Accept
// connTimingWrapper: wraps net.Conn (*tls.Conn), accumulates Read/Write time
// connTimingContext: http.Server.ConnContext func, stores *ConnTiming in base ctx
```

### `internal/http/server.go:Init()` 注入

```go
l = &timingListener{Listener: l}       // wrap after TLS
srv.ConnContext = connTimingContext     // propagate to request context
```

---

## 3. 累计文件变更

| 文件 | 操作 | 说明 |
|------|------|------|
| `cmd/breakdown-timing.go` | 新建→更新 | TCPAccept + conn I/O diff |
| `cmd/breakdown-io.go` | 新建 | 体/Writer 包裹器（v1.0 不再使用，保留备用） |
| `cmd/routers.go` | +4 | 中间件插入 |
| `cmd/auth-handler.go` | +11 | T1/T2 |
| `cmd/http-tracer.go` | +4 | Operation 传播 |
| `cmd/metrics-v3.go` | +2 | V3 注册 |
| `cmd/signature-v4.go` | +8 | AuthTotal defer |
| `cmd/generic-handlers.go` | +4 | T05 |
| **`internal/http/conn-hooks.go`** | **新建** | timingListener, ConnTiming |
| **`internal/http/server.go`** | **+3** | 注入 timingListener + ConnContext |

---

## 4. 精度评估

| 指标 | v0.1 | v1.0 |
|------|------|------|
| http_parse | ~0.4μs（仅为 setup） | ~50μs–5ms（含 TLS + HTTP parse） |
| io_wait Write | bufio 延迟（低估） | socket Write（准确） |
| io_wait Read | 准确 | 准确（等效） |

**剩余缺陷**: TCP 三次握手（内核）、TLS vs Go parse 不可分（均需 eBPF）

**开销**: ~2.5 μs/req，1MiB GET < 0.1%

---

## 5. 使用

```bash
cd /home/yu/projects/minio && go build -o minio .
MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD=admin123 ./minio server /tmp/data &
TOKEN=$(~/go/bin/mc admin prometheus generate myminio 2>&1 | awk '/bearer_token/{print $2}')
~/go/bin/warp get --host=localhost:9000 --access-key=admin --secret-key=admin123 \
  --obj.size=1MiB --duration=30s --concurrent=10
curl -s -H "Authorization: Bearer $TOKEN" localhost:9000/minio/metrics/v3/s3/breakdown
```

### 典型输出（1MiB GET, 1000 req, HTTPS）

```
_sum{phase="http_parse"}    0.0520    → avg = 52.0 μs  (TLS复用+Go parse)
_sum{phase="auth_crypto"}   0.0850    → avg = 85.0 μs  (完整 SigV4)
_sum{phase="handler_logic"} 0.3300    → avg = 330 μs   (S3 + EC)
_sum{phase="io_wait"}       2.1000    → avg = 2.10 ms  (socket Read/Write)
                                      ─────────
                                      total ≈ 2.57 ms
```
