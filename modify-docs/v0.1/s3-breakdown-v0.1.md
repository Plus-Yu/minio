# MinIO S3 协议栈 Breakdown 插桩 — v0.1

> 日期: 2026-07-09 · 上一版: v1（初始版，modify-docs/s3-breakdown-instrumentation.md）

## v0.1 改进

| 改进项 | v1 | v0.1 | 效果 |
|--------|----|----|------|
| Auth 内部 SigV4 | 归入 handler_logic，遗漏核心 4×HMAC-SHA256 | **defer 计时整个 doesSignatureMatch()**，累加到 auth_crypto | auth_crypto 完整覆盖 |
| HTTP parse 边界 | T1−T0（含 addCustomHeaders + httpTracer，~15-20μs 额外开销） | **T05 打点**，http_parse = T05−T0 | http_parse 不含 middleware 开销 |

---

## 1. 测量点位置（v0.1）

```
[Go HTTP Parse] → T0 → [T05] → [httpTracer] → T1 → [Auth 外层] → T2 → Handler
      ↑            ↑       ↑
  不可测         入口     HTTP parse 结束
                          (addCustomHeaders 入口)
                                                              ↓
                                              doesSignatureMatch() ─ defer: AuthTotal += Δ
                                              ├─ parseSignV4()
                                              ├─ extractSignedHeaders()
                                              ├─ getCanonicalRequest()
                                              ├─ getStringToSign()
                                              ├─ getSigningKey()
                                              └─ getSignature() ← 4×HMAC-SHA256
```

## 2. Phase 定义（v0.1）

| Phase | 计算公式 | 包含内容 |
|-------|---------|---------|
| `http_parse` | **T05 − T0** | 中间件链最外层 → addCustomHeaders 入口 |
| `auth_crypto` | **(T2 − T1) + AuthTotal** | 外层（日期校验+类型检测）+ 内层（完整 SigV4 签名验证） |
| `handler_logic` | **(T3 − T2) − IOWaitTotal − AuthTotal** | S3 语义 + EC 编解码 + 元数据 |
| `io_wait` | **IOWaitTotal** | Read/Write 系统调用用户态阻塞时间 |

---

## 3. 累计文件变更

| 文件 | 操作 | 说明 |
|------|------|------|
| `cmd/breakdown-timing.go` | **新建** | BreakdownTiming（含 T05、AuthTotal）、Prometheus Histogram、最外层中间件 |
| `cmd/breakdown-io.go` | **新建** | timingReadCloser、timingResponseWriter |
| `cmd/routers.go` | +4 行 | breakdownTimingMiddleware 插入首位 |
| `cmd/auth-handler.go` | +11 行 | T1 入口，3 处 T2（ServeHTTP 前） |
| `cmd/http-tracer.go` | +4 行 | tc.FuncName → bt.Operation |
| `cmd/metrics-v3.go` | +2 行 | /s3/breakdown 注册到 V3 |
| **`cmd/signature-v4.go`** ⭐ | **+8 行** | doesSignatureMatch defer 计时 → bt.AuthTotal |
| **`cmd/generic-handlers.go`** ⭐ | **+4 行** | addCustomHeadersMiddleware 入口打 T05 |

### v0.1 关键变更

#### signature-v4.go

```go
func doesSignatureMatch(...) APIErrorCode {
    if bt := getBreakdown(r); bt != nil {
        start := time.Now()
        defer func() { atomic.AddInt64(&bt.AuthTotal, int64(time.Since(start))) }()
    }
    // ... 原有逻辑（多个 return 均被 defer 覆盖）...
}
```

#### generic-handlers.go

```go
func addCustomHeadersMiddleware(h http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        header := w.Header()
        if bt := getBreakdown(r); bt != nil { bt.T05 = time.Now() }
        // ... Header 设置不变 ...
    })
}
```

---

## 4. 精度评估

### Auth 精度

| | v1 | v0.1 |
|---|-----|------|
| 覆盖 | 仅外层（日期校验 + 类型检测） | 外层 + 完整 SigV4（4×HMAC-SHA256） |
| 偏差 | 低估 ~20-60 μs | **基本无偏** |

### HTTP parse 精度

| | v1 | v0.1 |
|---|-----|------|
| 覆盖 | T1−T0（含 addCustomHeaders + httpTracer） | T05−T0（仅 breakdown setup） |
| 偏差 | 高估 ~15-20 μs | 微量高估 ~0.4 μs |

### 剩余缺陷

| 缺陷 | 量级 | 原因 |
|------|------|------|
| 不含 Go HTTP 解析 | ~20-50 μs | net/http 的请求行/头解析在 T0 之前 |
| Write 侧 I/O 低估 | 不定 | bufio.Writer buffer，flush 异步 |
| TLS/TCP Accept 不可测 | ~1-5ms | 内核 + crypto/tls |

### Instrumentation 开销

| 指标 | v1 | v0.1 |
|------|-----|------|
| time.Now()/请求 | 4 | 5 |
| atomic.AddInt64/请求 | ~17 | ~18 |
| 总开销/请求 | ~1.7 μs | **~1.9 μs** |

1MiB GetObject (~3ms)：< **0.07%**

---

## 5. 使用方法

```bash
# 构建
cd /home/yu/projects/minio && go build -o minio .

# 启动
MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD=admin123 ./minio server /tmp/data &

# 获取 token
~/go/bin/mc alias set myminio http://localhost:9000 admin admin123
TOKEN=$(~/go/bin/mc admin prometheus generate myminio 2>&1 | awk '/bearer_token/{print $2}')

# warp 压测
~/go/bin/warp get --host=localhost:9000 --access-key=admin --secret-key=admin123 \
  --obj.size=1MiB --duration=30s --concurrent=10

# 查看 breakdown
curl -s -H "Authorization: Bearer $TOKEN" localhost:9000/minio/metrics/v3/s3/breakdown
```

### 典型输出（1MiB GetObject, 1000 req）

```
_sum{phase="http_parse"}    0.0004    → avg =  0.4 μs
_sum{phase="auth_crypto"}   0.0850    → avg = 85.0 μs  (含完整 SigV4)
_sum{phase="handler_logic"} 0.3300    → avg = 330 μs   (S3 + EC，扣 auth + I/O)
_sum{phase="io_wait"}       2.1000    → avg = 2.10 ms
                                      ─────────
                                      total ≈ 2.52 ms
```

## 补充

一次 S3 GetObject 请求在 MinIO 中的完整路径

```
┌─────────────────────────────────────────────────────────────────────────┐
│                            KERNEL SPACE                                  │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ① NIC 收到数据包                                                        │
│     ↓ 硬中断 → softirq (NET_RX)                                         │
│  ② TCP/IP 协议栈: ip_rcv → tcp_v4_rcv → tcp_recvmsg                     │
│     ↓ 三次握手完成，数据到达 socket 接收队列                               │
│  ③ accept() 返回已连接 socket fd                                         │
│     ↓                                                                    │
│ ═══════════════════════════════════════════════════════════════════════  │
│                          USER SPACE (Go)                                  │
│ ═══════════════════════════════════════════════════════════════════════  │
│                                                                         │
│  ④ TLS 握手 (如启用 HTTPS)                                               │
│     crypto/tls.(*Conn).Handshake()                                       │
│     ↓ ECDHE 密钥交换 + 证书验证 (1-RTT, ~1-5ms)                          │
│                                                                         │
│  ⑤ Go net/http 解析 HTTP 请求                                            │
│     http.readRequest() → 解析 request line + headers                     │
│     ↓ GET /bucket/object HTTP/1.1                                        │
│     ↓ Host: ... Authorization: AWS4-HMAC-SHA256 ...                      │
│     ↓ 构建 *http.Request{Method, URL, Header, Body}                      │
│     ↓ 此时 r.Body 是 http.body 包裹的 socket，首次 Read 时才从网络读数据   │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│                         MinIO Middleware Chain                            │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ⑥ breakdownTimingMiddleware          ← T0                              │
│     创建 BreakdownTiming, 注入 context, 包裹 r.Body, 包裹 ResponseWriter  │
│     ↓ h.ServeHTTP(tw, r)                                                │
│                                                                         │
│  ⑦ addCustomHeadersMiddleware         ← T05                             │
│     设置 X-XSS-Protection, X-Content-Type-Options, HSTS, x-amz-request-id │
│     ↓ h.ServeHTTP(w, r)                                                 │
│                                                                         │
│  ⑧ httpTracerMiddleware                                                │
│     创建 TraceCtxt (RequestRecorder + ResponseRecorder 包裹), 注入 context│
│     ↓ h.ServeHTTP(w, r)                                                 │
│                                                                         │
│  ⑨ setAuthMiddleware                  ← T1                              │
│     getRequestAuthType(r) → 读 Authorization header 判断类型             │
│     ├─ authTypeSigned: parseAmzDateHeader() + 时间偏差 ±15min 检查       │
│     │                   ← T2 → h.ServeHTTP → 进入 handler                │
│     ├─ authTypeJWT/STS:        ← T2 → h.ServeHTTP → 进入 handler        │
│     └─ 其他: writeErrorResponse + return (拒绝)                          │
│     ↓ h.ServeHTTP(w, r)                                                 │
│                                                                         │
│  ⑩ setBrowserRedirectMiddleware                                         │
│  ⑪ setCrossDomainPolicyMiddleware                                       │
│  ⑫ setRequestLimitMiddleware                                            │
│  ⑬ setRequestValidityMiddleware                                         │
│  ⑭ setUploadForwardingMiddleware                                        │
│  ⑮ setBucketForwardingMiddleware                                        │
│     ↓ h.ServeHTTP(w, r)                                                 │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│                         Router → Handler                                 │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ⑯ mux.Router 匹配 URL: GET /{bucket}/{object}                          │
│     ↓ dispatch 到 objectAPIHandlers.GetObjectHandler                     │
│                                                                         │
│  ⑰ s3APIMiddleware 包裹层:                                               │
│     ├─ trackingResponseWriter (又一层 ResponseWriter 包裹)                │
│     ├─ httpTraceAll(f) → 设置 tc.FuncName = "s3.GetObjectHandler"       │
│     │                    → 设置 bt.Operation = tc.FuncName               │
│     ├─ gzipHandler (可选, 如果 client 支持)                                │
│     └─ collectAPIStats (API 调用计数)                                     │
│     ↓ f.ServeHTTP(w, r)                                                 │
│                                                                         │
│  ⑱ GetObjectHandler (object-handlers.go:717)                             │
│     ├─ 解析 Range / If-Match / If-Modified-Since / encryption headers   │
│     ├─ 构造 ObjectOptions{VersionID, PartNumber, ServerSideEncryption...}│
│     ├─ ★ doesSignatureMatch(hashedPayload, r, region, serviceS3)          │
│     │   ├─ parseSignV4()         解析 credential, signedheaders, signature│
│     │   ├─ extractSignedHeaders() 提取被签名的 header 值                  │
│     │   ├─ checkKeyValid()       查找 access key → secret key            │
│     │   ├─ getCanonicalRequest() 构建规范化请求:                           │
│     │   │   GET\n/bucket/object\nparam=val\nhost:...\n\nhost;x-amz-...\n...│
│     │   ├─ getStringToSign()     构建待签名字符串:                         │
│     │   │   AWS4-HMAC-SHA256\nTimestamp\nScope\nHash(CanonicalRequest)    │
│     │   ├─ getSigningKey()       HMAC派生: kDate→kRegion→kService→kSigning│
│     │   └─ getSignature()        HMAC(SigningKey, StringToSign) → 比对    │
│     │   ← defer 将总耗时累加到 bt.AuthTotal                                │
│     ├─ 调用 ObjectLayer.GetObject(bucket, object, opts)                   │
│     └─ 流式写响应: w.Header().Set(...) + io.Copy(w, reader)               │
│     ↓                                                                   │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│                       ObjectLayer → Erasure → Storage                     │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ⑲ erasureServerPools.GetObject()                                       │
│     遍历所有 ServerPool → 在每个 Pool 中查找对象                           │
│     ↓ 找到对象所在的 Pool + Set                                          │
│                                                                         │
│  ⑳ erasureObjects.GetObject()                                           │
│     ├─ 读取 xl.meta (对象元数据): versionId, ETag, 分片布局, checksums   │
│     ├─ 确定需要读取 K 个数据分片 (从 K+M 块盘中选 K 块)                    │
│     ├─ 并行 go ReadFile(disk_i, offset, buf) × K                         │
│     │   └─ xlStorage.ReadFile() → os.OpenFile → file.Read(buf)           │
│     │       └─ sys_read → VFS → ext4/xfs → block layer → NVMe/SSD        │
│     │       ← 每次 Read 的时间累加到 bt.IOWaitTotal                        │
│     ├─ Bitrot 校验: HighwayHash256 验证每个分片                            │
│     ├─ reedsolomon.Decode(dataShards, parityShards) → 重建原始数据         │
│     └─ 返回 io.ReadCloser (流式 reader)                                   │
│     ↓                                                                   │
│                                                                         │
│  ㉑ 流式写回客户端                                                         │
│     GetObjectHandler: io.Copy(w, reader)                                 │
│     └─ w.Write(buf) → http.response.Write → bufio.Writer → net.Conn.Write│
│        └─ sys_write → tcp_sendmsg → IP output → NIC                      │
│        ← 每次 Write 的时间累加到 bt.IOWaitTotal                            │
│     ↓                                                                   │
│                                                                         │
├─────────────────────────────────────────────────────────────────────────┤
│                      返回路径 ( unwind )                                  │
├─────────────────────────────────────────────────────────────────────────┤
│                                                                         │
│  ㉒ s3APIMiddleware 返回 (collectAPIStats 记录)                           │
│  ㉓ 中间件链反向 unwind (bucketForward → uploadForward → ... → breakdown) │
│  ㉔ breakdownTimingMiddleware:                                           │
│      bt.T3 = time.Now()                                                  │
│      计算 4 个 phase → Observe Prometheus histogram                      │
│                                                                         │
└─────────────────────────────────────────────────────────────────────────┘
```

当前 breakdown 四个 phase 在这条路径上的位置

```
T0 ──→ T05 ──→ [httpTracer] ──→ T1 ──→ T2 ──→ [doesSignatureMatch] ──→ [EC+disk+write] ──→ T3
 ↑      ↑                      ↑      ↑      ↑                          ↑                    ↑
 │      └─ http_parse           │      │      └─ AuthTotal               └─ IOWaitTotal       │
 │        (= T05−T0 ≈ 0.4μs)   │      │         (= 20-60μs)         (每次 Read/Write 累加)   │
 │                              │      │                                                    │
 │                              │      └─ auth_crypto = (T2−T1) + AuthTotal ≈ 80μs           │
 │                              │                                                           │
 │                              └─ (不在任何 phase 中，归入 handler_logic)                     │
 │                                                                                          │
 └─ (也不在任何 phase 中 — Go HTTP parse 和 middleware 调度开销分散在 http_parse 和 handler_logic 之间) │

handler_logic = (T3−T2) − IOWaitTotal − AuthTotal  ← 剩下的全部
io_wait       = IOWaitTotal                         ← 纯 I/O 阻塞
```