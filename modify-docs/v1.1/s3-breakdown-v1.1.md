# MinIO S3 协议栈 Breakdown 插桩 — v1.1

> 日期: 2026-07-09 · 上一版: v1.0 (P2 connection-level timing)

## v1.1 改进

| 改进项 | v1.0 | v1.1 |
|--------|------|------|
| Phase 数量 | 4 | **5**（新增 `http_headers`） |
| HTTP 响应头 | 混入 `handler_logic` | **独立 phase**: `(HTTPHeaderSent−T2)−AuthTotal` |
| http_parse | `T0−TCPAccept`（keep-alive 下错误） | **`T0−LastReadTime`**（conn Read 完成→中间件入口） |
| 未覆盖区域 | 未文档化 | **6 区域明确定义** |

---

## 1. 完整测量视图

```
                          请求生命周期
┌──── 内核 ────┐  ┌─────────── Go/用户态 ───────────────────────────────┐  ┌─内核─┐
│              │  │                                                     │  │      │
│ TCP 三次握手  │  │ TLS   Go HTTP  middlewares  Auth    Handler        │  │POST  │
│              │  │ 握手   解析请求                                         │  │flush │
│ accept()     │  │ Read()  readReq  T0 T05 T1 T2  sigV4  WriteHdr  EC  │  │Write │
│    ↓         │  │  ↓        ↓      ↓  ↓   ↓  ↓    ↓       ↓      ↓   │  │  ↓   │
├────A─────────┤  ├──B───┬───C──────┬─D──┬───E───┬───F───┬───G──────H───┤  ├──I───┤
│  ❌ 不可测    │  │  ❌  │  ⚠️部分  │ ✅ │  ✅   │  ✅  │     ✅       │  │  ❌   │
└──────────────┘  └──────┴──────────┴────┴───────┴──────┴──────────────┘  └──────┘

时间戳: TCPAccept  LastReadTime  T0  T05  T1  T2  AuthTotal  HTTPHdrSent  T3
```

## 2. 5 Phase 定义

| Phase | 公式 | 覆盖 | 测量对象 |
|-------|------|------|---------|
| `http_parse` | **T0 − LastReadTime**（回退: T05−T0）| C | Go HTTP 请求解析（conn Read 完成→中间件入口） |
| `http_headers` | **(HTTPHeaderSent − T2) − AuthTotal** | F | HTTP 响应头构造（Header.Set + WriteHeader） |
| `auth_crypto` | **(T2 − T1) + AuthTotal** | D+E | 外层日期校验 + 内层完整 SigV4（4×HMAC） |
| `handler_logic` | **(T3 − HTTPHeaderSent) − IOWaitTotal** | G+H | EC 解码 + 元数据 + S3 语义（WriteHeader 之后） |
| `io_wait` | **ConnTiming Read/Write diff (T0→T3)** | — | 原始 socket Read/Write wall-clock |

**互斥性**: 五个 phase 互为补集，无重叠，无遗漏服务端 CPU 时间。

## 3. 未覆盖区域

| ID | 区域 | 量级 | 方案 |
|----|------|------|------|
| A | TCP 三次握手 | 0.1–3ms | eBPF `kprobe:tcp_accept` |
| B | TLS 握手 | 首次 1–5ms, 后续 0 | eBPF `uprobe:crypto/tls.Handshake` |
| C | Go HTTP 请求解析（准确值） | ~20–50μs | `http_parse` 捕获，但 keep-alive 下 bufio 预读污染 |
| D | middleware 开销 (T05→T1) | ~15–20μs | 散落在 http_parse/handler_logic 中 |
| I | POST-T3 响应 flush | <4KB: ~250μs | Go 1.20+ `ResponseController.AfterFlush` |
| J | 客户端开销 | ~70–100μs | warp RTT − breakdown Σ |

## 4. 时间戳来源

| 时间戳 | 文件 | 说明 |
|--------|------|------|
| TCPAccept | `internal/http/conn-hooks.go` | timingListener.Accept() 返回（TLS 之后） |
| LastReadTime | `internal/http/conn-hooks.go` | connTimingWrapper.Read() 每次完成后更新 |
| T0 | `cmd/breakdown-timing.go` | 最外层中间件入口 |
| T05 | `cmd/generic-handlers.go` | addCustomHeadersMiddleware 入口 |
| T1 | `cmd/auth-handler.go` | setAuthMiddleware 入口 |
| T2 | `cmd/auth-handler.go` | 3 处 h.ServeHTTP 前 |
| AuthTotal | `cmd/signature-v4.go` | doesSignatureMatch() defer |
| HTTPHeaderSent | `cmd/breakdown-timing.go` | headerTimingResponseWriter.WriteHeader() |
| T3 | `cmd/breakdown-timing.go` | middleware 出口 |
| IOWaitTotal | conn Read/Write diff | `cmd/breakdown-timing.go` + `internal/http/conn-hooks.go` |

## 5. 累计文件变更

| 文件 | 操作 | 说明 |
|------|------|------|
| `cmd/breakdown-timing.go` | **新建** | 5-phase histogram + middleware + headerTimingResponseWriter |
| `cmd/breakdown-io.go` | 新建 | timingReadCloser/ResponseWriter（保留备用） |
| `cmd/routers.go` | +4 | 中间件插入首位 |
| `cmd/auth-handler.go` | +11 | T1, T2 (3 处) |
| `cmd/http-tracer.go` | +4 | tc.FuncName → bt.Operation |
| `cmd/metrics-v3.go` | +2 | /s3/breakdown 注册 V3 |
| `cmd/signature-v4.go` | +8 | doesSignatureMatch defer |
| `cmd/generic-handlers.go` | +4 | T05 |
| `internal/http/conn-hooks.go` | **新建** | timingListener, ConnTiming (AcceptTime, LastReadTime, Read/Write accumulators) |
| `internal/http/server.go` | +3 | 注入 timingListener + ConnContext |

## 6. 使用方法

```bash
cd /home/yu/projects/minio && go build -o minio .
MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD=admin123 ./minio server /tmp/data &
TOKEN=$(~/go/bin/mc admin prometheus generate myminio 2>&1 | awk '/bearer_token/{print $2}')
~/go/bin/warp get --host=localhost:9000 --access-key=admin --secret-key=admin123 \
  --obj.size=1KiB --duration=30s --concurrent=1
./modify-docs/breakdown.sh
```

## 7. 已知限制

| 限制 | 影响 | 说明 |
|------|------|------|
| keep-alive 下 http_parse 偏高 | LastReadTime 被 bufio 预读污染 | 看首请求值更准确 |
| <4KB 响应 io_wait=0 | bufio 未 flush | >4KB 对象才有 io_wait |
| TLS + HTTP parse 不可分 | 都在 conn.Read 内部 | eBPF 可拆 |
