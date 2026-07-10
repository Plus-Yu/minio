# MinIO S3 协议栈 Breakdown 插桩 — v1.2

> 日期: 2026-07-10 · 上一版: v1.1

## v1.2 变更

| 项目 | v1.1 | v1.2 |
|------|------|------|
| timingListener 位置 | TLS 之后 | **TLS 之前** — TCPAccept = 纯 TCP accept |
| TLS 握手 | 不可测 | **首请求 conn.Read() 捕获**（I/O 累加器自动包含） |
| connTimingContext | 直接匹配 `*connTimingWrapper` | **解包 `*tls.Conn.NetConn()`** |

---

## 1. 运行时架构

### 监听器包裹链

```
raw TCP listener
  → timingListener              ← TCPAccept = Accept() 返回时刻（纯 TCP）
    → tls.NewListener (可选)     ← conn Read/Write 累加器捕获 TLS 握手
      → Go http.Server.Serve()
```

### 请求级时间戳链

```
TCPAccept ─→ [TLS] ─→ LastReadTime ─→ T0 ─→ T05 ─→ T1 ─→ T2 ─→ [SigV4] ─→ HTTPHdrSent ─→ [EC+I/O] ─→ T3
    ↑          ↑           ↑           ↑     ↑      ↑     ↑       ↑           ↑            ↑          ↑
 timing     conn.Read   conn.Read    最外层 addCst  auth  auth   doesSig-  WriteHeader   handler    middleware
 Listener   累加器     完成时刻      中间件  Hdrs  entry dispatch natureMatch 被拦截      返回       出口
```

---

## 2. 五个 Phase

### 公式

| Phase | 公式 | 覆盖 |
|-------|------|------|
| `http_parse` | **T0 − LastReadTime** | Go HTTP 请求解析（conn Read 完成→中间件入口） |
| `http_headers` | **(HTTPHeaderSent − T2) − AuthTotal** | HTTP 响应头构造（Header.Set + WriteHeader） |
| `auth_crypto` | **(T2 − T1) + AuthTotal** | 外层日期校验 + 内层完整 SigV4（4×HMAC） |
| `handler_logic` | **(T3 − HTTPHeaderSent) − IOWaitTotal** | EC 解码 + 元数据 + S3 语义 |
| `io_wait` | **conn Read/Write diff (T0→T3)** | socket 级 Read/Write wall-clock |

五个 phase 无重叠、无遗漏（T0→T3 窗口内的服务端 CPU 时间）。

### 各 phase 内部构成

**http_parse** — Go HTTP 解析:
- `net/http.readRequest()` — 请求行 + Header 解析
- goroutine 调度延迟（conn reader → handler goroutine）
- ⚠️ keep-alive: bufio 预读导致 LastReadTime 偏旧 → 值偏大

**http_headers** — HTTP 响应头:
- bucket/object 路径解析 (`mux.Vars(r)`)
- S3 metadata header 构造 (`w.Header().Set("ETag",...)` 等)
- `w.WriteHeader(200)` — 状态行 + Header 序列化

**auth_crypto** — SigV4 验证:
- 外层 (T2−T1): `getRequestAuthType()` + `parseAmzDateHeader()` + 时间偏差检查
- 内层 (AuthTotal): `parseSignV4()` → `extractSignedHeaders()` → `getCanonicalRequest()` → `getStringToSign()` → `getSigningKey()` → `getSignature()` + 比对

**handler_logic** — 业务逻辑:
- `ObjectLayer.GetObject()` → EC 引擎
- 读取 xl.meta → 并行 ReadFile × K → Bitrot 校验 → Reed-Solomon 解码

**io_wait** — socket I/O:
- `conn.Read()` 耗时: TCP 等待 + 内核 copy_to_user + TLS 解密
- `conn.Write()` 耗时: 内核 copy_from_user + TLS 加密 + TCP 发送
- ⚠️ 首请求 conn.Read 包含 TLS 握手
- ⚠️ <4KB 响应: bufio 缓冲 → T3 之后 flush → io_wait=0

---

## 3. 覆盖全景

```
┌─── 内核 ──┐  ┌────────────── Go/用户态 ────────────────────────────┐  ┌─内核─┐
│            │  │                                                    │  │      │
│ TCP 3次握手 │  │ TLS    Go HTTP   mw      Auth    HTTP-Hdr   EC     │  │flush │
│  accept()  │  │ 握手   解析请求                                      │  │      │
│     ↓      │  │ Read() readReq  T0..T1  T1..T2  T2..Hdr   Hdr..T3 │  │Write │
├──A─────────┤  ├─B────┬─C─────┬──D────┬──E─────┬──F──────┬──G──────┤  ├──H───┤
│ ✅ TCPAcpt │  │ ✅   │  ⚠️    │  ❌   │  ✅    │  ✅     │  ✅     │  │  ❌  │
│ =纯TCP完成 │  │I/O acc│http_  │散落各 │auth_   │http_    │handler  │  │POST  │
│            │  │捕获TLS│parse  │phase中│crypto  │headers  │_logic   │  │flush  │
└────────────┘  └──────┴───────┴───────┴────────┴─────────┴─────────┘  └───────┘
```

| ID | 区域 | 量级 | 状态 | 说明 |
|----|------|------|------|------|
| A | TCP 三次握手 | 0.1–3ms | ✅ TCPAccept 标记 | timingListener Accept 返回 = 握手完成 |
| B | TLS 握手 | 首次 1–5ms | ✅ I/O 累加器 | 首请求 conn.Read 内部触发 Handshake |
| C | Go HTTP 请求解析 | ~20–50μs | ⚠️ http_parse | keep-alive 下 bufio 预读污染 |
| D | middleware (T05→T1) | ~15–20μs | ❌ 散落 | addCustomHeaders + httpTracer |
| E | SigV4 完整验证 | ~30–80μs | ✅ auth_crypto | 外层+内层 (doesSignatureMatch) |
| F | HTTP 响应头 | ~10–50μs | ✅ http_headers | Header.Set + WriteHeader |
| G | 业务逻辑 | 可变 | ✅ handler_logic | EC + S3 语义 |
| H | POST-T3 flush | <4KB:~250μs | ❌ 漏 | bufio 在 T3 之后发送 |

---

## 4. 使用方法

```bash
# 构建
cd /home/yu/projects/minio && go build -o minio .

# 启动
MINIO_ROOT_USER=admin MINIO_ROOT_PASSWORD=admin123 ./minio server /tmp/data &

# 压测 (1KiB GET, 单并发)
~/go/bin/warp get --host=localhost:9000 --access-key=admin --secret-key=admin123 \
  --obj.size=1KiB --duration=30s --concurrent=1

# 查询
./modify-docs/breakdown.sh s3.GetObject GET
```

### 输出解读

```
phase             avg(us)       %     count
--------------------------------------------
auth_crypto          59.3   13.2%     32634     ← SigV4 签名验证
handler_logic        18.3    4.1%     32634     ← EC 解码 (1KiB 极小)
http_headers        280.7   62.3%     32634     ← HTTP 响应头构造
http_parse           92.3   20.5%     32634     ← Go HTTP 解析 (含 bufio 噪音)
io_wait               0.0    0.0%     32634     ← 全在 <4KB bufio 缓冲
--------------------------------------------
total               450.5us

"HTTP 协议开销" = http_parse + http_headers ≈ 373 μs (83%)
"S3 协议开销"   = auth_crypto               ≈  59 μs (13%)
"业务处理"      = handler_logic             ≈  18 μs ( 4%)
"I/O 等待"      = io_wait                   ≈   0 μs (0%, <4KB bufio)
```

---

## 5. 已知限制

| 限制 | 影响 | 缓解 |
|------|------|------|
| keep-alive 下 http_parse 偏高 | bufio 预读污染 LastReadTime | 连接首请求的值更准确 |
| <4KB 响应 io_wait=0 | bufio 在 T3 后 flush | >4KB 对象 io_wait 相对准确 |
| middleware 开销未单独拆分 | ~15–20μs 散落 | 量级小且稳定 |
| Write 最后 flush 未捕获 | 最后 <4KB chunk 在 T3 后 | >4KB 响应误差 <0.5% |

## 6. 文件清单

| 文件 | 说明 |
|------|------|
| `cmd/breakdown-timing.go` | BreakdownTiming + 5-phase histogram + middleware + headerTimingResponseWriter |
| `cmd/breakdown-io.go` | timingReadCloser/ResponseWriter（保留备用） |
| `cmd/routers.go` | breakdownTimingMiddleware 插入首位 |
| `cmd/auth-handler.go` | T1, T2 (3 处) |
| `cmd/http-tracer.go` | tc.FuncName → bt.Operation |
| `cmd/metrics-v3.go` | /s3/breakdown 注册 V3 |
| `cmd/signature-v4.go` | doesSignatureMatch defer → AuthTotal |
| `cmd/generic-handlers.go` | T05 |
| `internal/http/conn-hooks.go` | timingListener, ConnTiming, connTimingWrapper, connTimingContext (含 TLS 解包) |
| `internal/http/server.go` | timingListener 注入 (TLS 之前) + ConnContext |
| `modify-docs/breakdown.sh` | 查询脚本 |
