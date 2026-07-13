# MinIO S3 协议栈 Breakdown 插桩 — v1.3

> 日期: 2026-07-10 · 上一版: v1.2

## v1.3: 拆分 EC 元数据与 HTTP 响应头

| 改进 | v1.2 | v1.3 |
|------|------|------|
| Phase 数 | 5 | **6** (+`ec_metadata`) |
| http_headers 范围 | `(HTTPHeaderSent−T2)−AuthTotal` 含 ~195μs xl.meta I/O | **`HTTPHeaderSent−ECMetaDone`** 纯 HTTP ~40μs |
| EC 元数据 | 混入 http_headers | **独立 phase** `ec_metadata = ECMetaDone−T2` |

---

## 1. 时间戳链

```
T2 → [EC: ReadFile(xl.meta)+msgp] → ECMetaDone → [HTTP: Header.Set×15+WriteHeader] → HTTPHeaderSent → [EC decode+io.Copy] → T3
      ←── ec_metadata ──────────→   ←──── http_headers ──────────────→   ←─ handler_logic ────→
```

## 2. Phase

| Phase | 公式 | 内容 |
|-------|------|------|
| `http_parse` | T0 − LastReadTime | Go HTTP 请求解析 |
| ✨ `ec_metadata` | ECMetaDone − T2 | xl.meta ReadFile + msgp 反序列化 |
| `http_headers` | HTTPHeaderSent − ECMetaDone | 纯 HTTP 响应头构造 |
| `auth_crypto` | (T2 − T1) + AuthTotal | SigV4 |
| `handler_logic` | (T3 − HTTPHeaderSent) − IOWaitTotal | EC 解码 + 响应体 |
| `io_wait` | conn Read/Write diff | socket I/O |

## 3. 变更

| 文件 | 说明 |
|------|------|
| `cmd/object-handlers.go` | +4 — `getObjectNInfo` 完成后、首个 `w.Header().Set` 前打 `ECMetaDone` |
| `cmd/breakdown-timing.go` | +PhaseECMetadata + ECMetaDone 字段 + 更新公式 |

## 4. 预期

| Phase | v1.2 | v1.3 |
|-------|------|------|
| `ec_metadata` | — | ~195 μs (新增) |
| `http_headers` | ~280 μs | ~40 μs (去除 EC I/O) |
