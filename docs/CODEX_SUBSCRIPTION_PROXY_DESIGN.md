# Codex ChatGPT 订阅本地代理设计

状态：Draft

日期：2026-08-04

范围：Codex CLI，以及经过兼容性验收的 Codex App build 使用 ChatGPT 登录时，经本地 `apid` 转发 Responses API 请求。

## 1. 结论

该能力可以在 `apid` 现有架构上实现，且不需要接管 Codex 的登录或刷新令牌逻辑。推荐方案是：

1. Codex 继续负责 ChatGPT 登录、令牌存储与刷新。
2. 在 Codex 自定义 provider 中配置本地 `base_url`、`wire_api = "responses"` 和 `requires_openai_auth = true`。
3. Codex 把它持有的 OpenAI `Authorization` 发给本地 `apid`。
4. `apid` 仅在显式的 `codex_subscription` 认证模式下，把该头以相同值转发到固定的官方 Codex 后端。
5. 请求和响应保持 Responses 原协议，不做 Responses ↔ Chat 字段转换；订阅路径在不改写 model 的前提下保留请求 payload 原始字节，同时继续采集不含凭证的状态码、模型、SSE、TTFT 和 token 指标。

这不是 HTTPS 中间人代理，也不读取 `~/.codex/auth.json`。它只是利用 Codex 官方支持的自定义 provider，把模型请求的目标地址改成本机。

MVP 必须把它实现为一种显式、高约束的上游认证模式，不能简单依赖现有的“`api_key` 为空时透传客户端认证”。订阅令牌的敏感度远高于普通网关请求，必须固定上游、禁止重定向、限制监听地址并默认关闭请求体 TRACE。

本文统一使用“协议透传”描述该路径，其含义是“不改变 Responses 协议语义”，不是 TCP/HTTP wire-level 的透明代理：

- 请求 payload 字节在订阅 inference/compact 路径保持不变；
- 请求头会按安全策略过滤和重建，`Host`、`Content-Length`、连接头、头部大小写和顺序不保证保持；
- HTTP 版本、分块边界、连接复用方式和 SSE 网络 chunk 边界不保证保持；
- 响应允许由 Go transport 做标准内容编码协商和透明解压，因此保证响应语义与 SSE 事件内容，不承诺压缩后的响应字节完全相同；
- 如果未来需要响应 payload 字节级保真，必须单独启用 `DisableCompression`、同步透传 `Content-Encoding`，并重新设计压缩 SSE 的指标采集。

## 2. 已核实的依据

### 2.1 OpenAI Codex 官方契约

Codex 官方配置支持自定义 model provider，包括 `base_url`、`wire_api`、额外 HTTP 头和认证方式。官方认证文档明确说明：

- `requires_openai_auth = true` 会让自定义 provider 使用 OpenAI 登录凭证；
- 用户可以使用 ChatGPT 登录或 API Key 登录；
- 该模式适用于通过 LLM proxy 访问 OpenAI 模型；
- 开启后 `env_key` 会被忽略。

OpenAI Codex 仓库还包含一个官方 `responses-api-proxy` 示例，其配置同样是把 `base_url` 指向本地服务并使用 Responses wire API。这证明“Codex → 本地 Responses 代理”的接入方式是官方支持的扩展点。

官方材料能证明的是“自定义 provider + OpenAI 认证 + 本地 Responses 代理”这一扩展契约；它不承诺 `chatgpt.com/backend-api/codex` 或 `/responses/compact` 的第三方稳定性。后两项来自当前 Codex/cc-switch 行为观察，必须按兼容性实现而不是公共 API 实现。

参考：

- [Codex Advanced Configuration：Custom model providers](https://learn.chatgpt.com/docs/config-file/config-advanced#custom-model-providers)
- [Codex Authentication：Alternative model providers](https://learn.chatgpt.com/docs/auth#alternative-model-providers)
- [Codex Configuration Reference](https://learn.chatgpt.com/docs/config-file/config-reference)
- [OpenAI Codex responses-api-proxy](https://github.com/openai/codex/blob/main/codex-rs/responses-api-proxy/README.md)

### 2.2 cc-switch 的已验证实现

当前 `cc-switch` 主线已经采用同一条认证与目标链路：创建本地 Responses provider、设置 `requires_openai_auth = true`、保留 Codex 发来的 `Authorization`，并将请求固定转发到 `https://chatgpt.com/backend-api/codex`。

这里的“同一条链路”只表示认证来源、固定目标和 Responses 协议相同，不表示 cc-switch 是字节级透明代理。当前 cc-switch 会：

- 解压 Codex 的压缩请求体并解析成 JSON；
- 递归过滤部分 `_` 前缀私有字段，并规范化 JSON key 顺序；
- 重新序列化请求体和重建请求头；
- 对非流式压缩响应做透明解压，SSE 则在透传字节的同时解析事件和统计。

因此 cc-switch 的官方 Codex 路径属于“Responses 语义透传”。`apid` 本设计复用其认证安全边界，但对 inference/compact 请求采用更窄的“原始 payload 字节透传”，不复制上述 JSON 清洗行为。

值得复用的不是其完整代理框架，而是下面这些安全边界：

- 只有明确标记为官方 Codex 的 provider 才能透传订阅认证；
- 官方上游地址是固定值，不接受请求动态指定；
- `/responses` 与 `/responses/compact` 分开处理；
- 官方上游的 401/403 直接返回，不做账号或供应商故障转移；
- WebSocket 显式关闭，先走 HTTP/SSE；
- 不向 Codex 的 `auth.json` 写占位 API Key。

参考实现固定在本次调研的提交 `492245dcb9196b0169e227d9eae2ab91466c0058`：

- [Codex 官方 provider 配置投影](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/codex_config.rs#L1728)
- [官方 Codex 上游识别与固定地址](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/providers/codex.rs#L225)
- [Authorization 特例透传](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/forwarder.rs#L1972)
- [Responses 路由](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/server.rs#L327)
- [Codex 请求解压与 JSON 解析](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/handlers.rs#L650)
- [私有字段过滤与规范化](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/forwarder.rs#L1579)
- [请求重新序列化](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/forwarder.rs#L2148)
- [非流式响应透明解压](https://github.com/farion1231/cc-switch/blob/492245dcb9196b0169e227d9eae2ab91466c0058/src-tauri/src/proxy/response_processor.rs#L77)

### 2.3 稳定性说明

Codex 的自定义 provider 与 `requires_openai_auth` 是公开配置能力；`chatgpt.com/backend-api/codex` 则不是面向第三方承诺稳定性的公共 API。后者可能随 Codex 版本调整，因此本功能应标记为 experimental，并把官方后端地址作为代码内受审查的常量，而不是普通用户可自由配置的订阅令牌目的地。

`requires_openai_auth = true` 既可以使用 ChatGPT 登录，也可以使用 API Key 登录。`apid` 只看到 Bearer 字符串，无法可靠判断它来自哪种登录方式、账号是否为 Pro/Plus、属于哪个套餐或是否具备某个模型权限。`codex_subscription` 是上游安全策略的名称，不是订阅资格证明；MVP 不解析 JWT，也不根据 token 内容作路由决定。

### 2.4 兼容性基线

内部后端和兼容端点可能随客户端版本变化。每次实现或发布该功能时必须记录：

| 项目 | 基线要求 |
|---|---|
| cc-switch 参考 | `3.19.1` / `492245dcb9196b0169e227d9eae2ab91466c0058` |
| Codex CLI | 记录人工验收使用的精确版本及观察到的 endpoint/header/content-encoding |
| Codex App | 记录 build/平台；未验证时明确标记 unsupported，而不是从 CLI 结果推断 |
| 官方配置契约 | 以验收时的 Codex Configuration Reference / Authentication 文档为准 |

文档中的 `/responses/compact` 是当前兼容性基线，不应被描述为 OpenAI 对第三方长期承诺的公共接口。MVP 不提供 `/models`；所支持的 Codex 版本必须在人工验收中证明能依赖 Codex 自带目录或用户配置完成启动和选模。

## 3. 目标与非目标

### 3.1 目标

- 支持已通过 ChatGPT 登录的 Codex CLI，以及支持矩阵中明确列出的 Codex App build，把 Responses HTTP/SSE 请求交给本地 `apid`。
- 支持 `POST /responses` 和 `POST /responses/compact`。
- 保留请求 payload、Responses 语义、流式事件和受大小限制的官方错误响应；不承诺 HTTP wire-level 字节一致。
- 保留 `apid` 现有的路由、统计、实时请求和拓扑能力。
- 确保该路径收到的 OpenAI Bearer 只能到达固定的官方 Codex 后端。
- 不影响现有 API Key 上游、Anthropic 上游和 Responses → Chat 转换路径。

### 3.2 非目标

- 不实现 Codex 登录、OAuth callback、令牌刷新或账号切换。
- 不读取、修改或备份 `~/.codex/auth.json`。
- MVP 不自动修改 `~/.codex/config.toml`；先由用户手工配置，后续再考虑独立的 install/uninstall 子命令。
- 不支持 WebSocket；Codex provider 必须设置 `supports_websockets = false`。
- 不支持 Claude Pro/Max 订阅接管。Claude Code 官方订阅没有对应的安全自定义 provider 契约。
- 不把 ChatGPT 订阅后端包装成对公网开放的通用 OpenAI API。
- 不在多个订阅账号、普通 API 上游或第三方供应商之间自动故障转移。
- 不判断或保证调用者拥有 Pro/Plus，也不把 ChatGPT 订阅凭证等同于 OpenAI API Key。
- 不实现 `/models`、动态 model catalog 或模型目录同步；客户端必须使用 Codex 自带目录或显式用户配置。

## 4. 核心不变量

实现过程中必须保持以下不变量：

1. `Authorization` 仅能在 path 已确定为订阅 route、并解析到 `auth_mode = "codex_subscription"` 的上游后保留。
2. `codex_subscription` 的目标只能是代码固定的 HTTPS origin 和两个固定 operation；配置 URL 不允许 userinfo、query 或 fragment。
3. 同一条 route 不得混用订阅上游和普通上游，避免认证策略由可伪造的 `model` 字段切换。
4. 订阅 inference/compact route 必须且只能有一条 catch-all model 规则；无法解析、没有 body 或使用压缩 body 的请求也必须落到唯一订阅上游。
5. 订阅模式禁止 model 改写和请求 JSON 清洗，保证 inference/compact 请求 payload 原始字节不变。
6. `X-Apid-Key` 只用于本地网关认证，绝不发往上游；OpenAI `Authorization` 只用于官方上游认证，不能兼作 `apid` 的客户端密钥。
7. inference/compact 必须恰好收到一个合法的 Bearer Authorization。
8. 生产路径不记录令牌、完整请求体、完整上游错误体、认证相关头、原始 query 或 `ChatGPT-Account-Id`。
9. 401/403 的状态和 body 返回给 Codex；`apid` 不重试、不故障转移，但 Codex 客户端可能按自身配置重新发起请求。
10. 上游 3xx 不自动跟随，也不把 `Location` 传给 Codex；MVP 将其映射为固定的 502 错误，避免客户端产生不可控的第二跳。
11. 客户端 query 只能作为 `RawQuery` 附加到代码固定 URL，不能改变 scheme、host 或 path，并且永不进入日志、SQLite 或 TRACE。
12. MVP 只允许 loopback 监听。

## 5. 总体架构

```text
Codex 登录/刷新
    │  凭证始终由 Codex 管理
    ▼
Codex custom provider
    │  POST http://127.0.0.1:19092/codex/v1/responses
    │  Authorization: Bearer <OpenAI credential>
    │  X-Apid-Key: <optional local gateway key>
    ▼
apid route
    │  路由级客户端认证
    │  解析唯一 codex_subscription upstream
    │  丢弃 X-Apid-Key，保留 Authorization
    ▼
apid upstream client
    │  固定 HTTPS、禁止 redirect、请求 payload / Responses 语义透传
    ▼
https://chatgpt.com/backend-api/codex/responses
```

`apid` 不参与上图最上方的登录生命周期。若令牌过期，官方上游返回 401；响应回到 Codex 后，Codex 可以按自身认证逻辑刷新、重新发起请求或要求重新登录。这是新的客户端请求，不是 `apid` 重试。

## 6. 配置设计

### 6.1 新增上游认证模式

在 `config.Upstream` 增加：

```go
type AuthMode string

const (
	AuthModeDefault           AuthMode = ""
	AuthModeCodexSubscription AuthMode = "codex_subscription"
)
```

```go
type Upstream struct {
	// existing fields...
	AuthMode AuthMode `toml:"auth_mode"`
}
```

空值保持现有行为，保证配置向后兼容。`codex_subscription` 是唯一新增值，不引入笼统的 `pass_client_auth = true`，从类型和校验层面缩小令牌可达范围。该名称表示“采用固定 ChatGPT Codex 后端的凭证透传策略”，不表示 `apid` 已验证 token 一定来自某种订阅。

订阅模式的校验规则：

- `protocol` 必须是 `openai_responses`；
- `base_url` 必须精确等于 `https://chatgpt.com/backend-api/codex`；
- `path` 必须精确等于 `/responses`；
- `base_url` 和 `path` 不得包含 userinfo、query、fragment、反斜杠或可产生歧义的转义路径；
- `api_key`、`model`、`supports_responses`、`responses_path` 必须为空或关闭；
- 所有引用它的 model rule 都必须强制透传 model，即 `model = ""`；
- 任何引用它的 route 都必须是 `openai_responses`，且该 route 不能再引用其他认证模式的 upstream；
- inference/compact route 必须且只能有一条 `match = "*"` 的规则，不允许精确或 glob 规则。

运行时也应从解析后的代码常量 `url.URL` 构造最终 URL；配置中的地址用于显式展示与启动校验，不能绕过固定目标。请求的 `RawQuery` 只能赋给该常量 URL 的副本，不得用客户端 URL 做 `ResolveReference` 或字符串拼接目标地址。

### 6.2 新增 Responses 操作类型

当前 route 只有入口协议，而同一个 Responses upstream 还需要 `/responses/compact`。不应复制两份 upstream，因此给 route 增加一个窄枚举：

```go
type RouteOperation string

const (
	RouteOperationInference        RouteOperation = ""
	RouteOperationResponsesCompact RouteOperation = "responses_compact"
)
```

- 空值保持现有推理请求行为；
- `responses_compact` 仅允许 `input_protocol = "openai_responses"`；
- 上游路径从 Responses path 安全派生为 `<responses path>/compact`，不接受请求提供任意子路径；
- compact 请求仍走同一 upstream、认证策略和统计链路。

该枚举只开放两个已知操作，不提供任意 HTTP method/path 代理能力。`responses_compact` 属于当前客户端兼容行为，不是从自定义 provider 公共契约推导出的通用代理端点。若未来需要 `/models` 或动态 model catalog，应另立设计，不纳入本方案。

### 6.3 `apid` 配置示例

```toml
# 可选。开启后，订阅 route 必须用 X-Apid-Key 提交该值，
# Authorization 留给 Codex 的 OpenAI 登录凭证。
client_api_key = "local-apid-key"

[[upstream]]
name = "openai-codex-subscription"
protocol = "openai_responses"
base_url = "https://chatgpt.com/backend-api/codex"
path = "/responses"
auth_mode = "codex_subscription"

[[route]]
path = "/codex/v1/responses"
input_protocol = "openai_responses"

  [[route.model]]
  match = "*"
  upstream = "openai-codex-subscription"
  model = ""

[[route]]
path = "/codex/v1/responses/compact"
input_protocol = "openai_responses"
operation = "responses_compact"

  [[route.model]]
  match = "*"
  upstream = "openai-codex-subscription"
  model = ""

```

建议使用专用 `/codex/v1` 前缀，不与现有普通 `/v1/responses` 路由共用入口。这样认证策略在读取 body 前就能确定，也降低误配风险。

### 6.4 Codex 配置示例

写入用户级 `~/.codex/config.toml`：

```toml
model_provider = "apid-codex-subscription"

[model_providers.apid-codex-subscription]
name = "OpenAI"
base_url = "http://127.0.0.1:19092/codex/v1"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false

# apid 自身不会重试。若需要端到端也不做透明网络/SSE 重试，显式覆盖
# Codex 当前的 provider 默认值（普通请求 4 次、SSE 中断 5 次）。
request_max_retries = 0
stream_max_retries = 0

# 明确保留 5 分钟 SSE 静默超时；可按实际长推理场景调整。
stream_idle_timeout_ms = 300000

# 仅当 apid 配置了 client_api_key 时需要。
# Codex 从环境变量读取值，避免把本地网关密钥再写一份到 config.toml。
env_http_headers = { "X-Apid-Key" = "APID_CODEX_PROXY_KEY" }
```

使用前提：用户已经执行 Codex 的 ChatGPT 登录流程。`apid` 不需要 OpenAI API Key，也不应在自己的 TOML 中保存 ChatGPT 访问令牌。

官方配置允许 `requires_openai_auth = true` 同时适配 ChatGPT 登录和 API Key 登录，因此上述前提依赖用户当前的 Codex 登录状态，不能由 `apid` 从 Bearer 内容验证。`request_max_retries` / `stream_max_retries` 只控制 Codex provider 的透明重试；认证层在收到 401 后刷新凭证并重新发起请求属于 Codex 自身行为，仍可能发生。

## 7. 请求处理设计

### 7.1 启动阶段

配置加载时完成全部安全校验，错误即拒绝启动：

1. 校验 `auth_mode` 和 `operation` 枚举；inference/compact 只允许 POST。
2. 校验固定 base URL/path、协议、model 透传和 route 认证模式一致性。
3. 在环境变量和 TOML 合并成完整 `config.Config` 后检查监听地址；不能只在当前仅接收 upstream/routes 的 `validateConfig` 内校验。MVP 只接受 IP literal `127.0.0.0/8` 或 `[::1]`，拒绝空 host、`localhost`、zone IPv6、`:port`、`0.0.0.0` 和其他网卡地址。当前默认 `:19092` 会绑定所有接口，因此使用该功能时必须显式设置 `APID_LISTEN=127.0.0.1:19092`。
4. 构造订阅专用 `http.Client`：系统 TLS 校验、无总超时、保留连接/TLS/响应头超时，`CheckRedirect` 明确拒绝自动重定向。
5. 订阅专用 transport 默认 `Proxy = nil`，不读取 `HTTP_PROXY`/`HTTPS_PROXY`。如未来需要企业代理，应另加显式开关并在文档中说明凭证暴露边界。
6. 一次 handler 尝试只调用一次上游 `Do`。不做应用层重试，不为订阅 POST 暴露可重放的 `GetBody`；客户端断开时必须通过 request context 取消上游请求。
7. 启动日志只输出固定 upstream 名称和不含 query 的规范化 endpoint，并打印一次 `experimental / credential passthrough` 风险提示。

### 7.2 客户端认证

当前 `withClientAuth` 会把 `Authorization`、`X-Api-Key` 等都视为 `apid` 的客户端密钥，随后统一删除。这与订阅代理冲突。

修改为 route-aware 策略：

| route 类型 | `apid` 客户端认证 | 发给上游的认证 |
|---|---|---|
| 普通 route | 保持现状，接受 Bearer / X-Api-Key / Api-Key | 由 upstream `api_key` 或现有回退逻辑决定 |
| 订阅 inference/compact | 若配置 `client_api_key`，只接受 `X-Apid-Key` | 只保留 Codex 的一个 Bearer `Authorization` |

订阅 route 不允许用 `Authorization` 通过 `apid` 自身认证。这样一个请求中的两个密钥不会争用同一个头。

Bearer 校验必须检查 `Header.Values("Authorization")`，而不是只调用 `Header.Get`：值数量必须恰好为 1，scheme 必须不区分大小写地等于 `Bearer`，token 去除首尾空白后必须非空；重复头、多个 scheme 或合并成逗号列表的值均拒绝。`apid` 不解析 JWT payload、不判断账号、不缓存令牌；真实性和权限由官方上游判断。

### 7.3 请求读取与路由

处理顺序调整为：

1. 根据 path 确定 route 的认证类别并完成本地网关认证。
2. 用 `http.MaxBytesReader` 或等价的有界 reader 限制请求体大小，保留收到的原始字节；已知超限的 `Content-Length` 应在分配大 buffer 前拒绝。
3. 对未压缩 JSON 尝试提取 `model` 和 `stream`，失败时保留“未知”状态，而不是默认为 `stream = false`。
4. 通过订阅 route 的唯一 catch-all 规则解析目标。
5. 根据目标 `AuthMode` 生成上游头：删除所有 `apid` 密钥，仅在订阅模式保留一个合法的 Bearer `Authorization`。
6. inference 请求发到固定 `/responses`；compact 请求发到固定 `/responses/compact`。
7. 收到上游响应头后，以实际 Content-Type 更新最终 stream 状态并选择响应管道。

`stream` 三态必须落实到数据结构，而不是只停留在处理流程说明中：

```go
type streamHint uint8

const (
	streamUnknown streamHint = iota
	streamNo
	streamYes
)
```

handler 和 live registry 在响应头到达前保留三态；最终持久化统计以实际响应 Content-Type 为准，可以继续投影为 bool，避免仅为短暂的 unknown 状态修改 SQLite schema。若 UI 需要展示在途请求，则应显示 `unknown`，不能暂时伪装成非流式。

### 7.4 Query 策略

为兼容 Codex provider 的 query 参数，inference/compact 可以保留客户端 `RawQuery`，但必须满足：

- 从代码固定的 `url.URL` 复制值后只赋 `RawQuery`，不使用客户端 URL 做相对路径解析，也不直接拼接完整目标字符串；
- query 无论内容如何都不能改变 HTTPS scheme、`chatgpt.com` host 或两个固定 path；
- access log、`stats.Record.ClientPath`、`stats.Record.UpstreamURL`、SQLite、TRACE 和错误文本只记录不含 query 的规范化 path/endpoint；
- 测试使用含 `access_token`、换行转义、绝对 URL 和重复参数的 query，确认它至多作为固定官方 endpoint 的 query 发出，且不会落入任何可观测性介质。

### 7.5 压缩请求

Codex App 的请求可能使用 `Content-Encoding`。MVP 采用“原始 body 透传、元数据尽力解析”的策略：

- 保留原始 body 和 `Content-Encoding`，不为统计目的重新序列化请求；
- 能解析时采集 model/stream，不能解析时 model 留空；
- inference 请求在 stream 未知时不添加 300 秒总超时，避免把实际 SSE 当成非流请求提前中止；
- compact 操作始终按非流请求使用现有总超时；
- 上游响应是否为流式以解析后的实际 `Content-Type: text/event-stream` 为准，而不是只信请求中的 `stream`；Content-Type 参数、大小写和空白使用 `mime.ParseMediaType` 处理。

该方案不需要为了 zstd/brotli 元数据解析立即引入新依赖，也不会破坏压缩字节。请求大小上限作用于收到的压缩字节；后续若对副本做 gzip/deflate/zstd/brotli 解码，必须同时限制压缩大小、解压后大小和压缩比，不能让统计解析引入压缩炸弹或改变主转发路径。

### 7.6 上游请求头策略

订阅请求允许透传正常业务头，例如 `User-Agent`、`ChatGPT-Account-Id`、`OpenAI-Beta` 和会话相关头；继续删除以下类别：

- `X-Apid-Key`、`X-Api-Key`、`Api-Key`、`Proxy-Authorization`；
- `Cookie`、`Set-Cookie` 和其他本地 origin 凭证；
- Host、Content-Length、Transfer-Encoding 和 hop-by-hop 头；
- `Forwarded`、`X-Forwarded-*`、CDN 来源 IP 和代理追踪头；
- 由 Go transport 重新协商的 `Accept-Encoding`。

保留客户端的原始 `Content-Encoding` 和 `Content-Type`；仅在 Content-Type 缺失时补 `application/json`。`ChatGPT-Account-Id` 属于允许转发但禁止记录的敏感账号标识。

`Authorization` 的例外必须由 `authModeCodexSubscription` 的专用转发函数添加，不能放宽通用的 `copyForwardHeaders`。普通 upstream 客户端继续沿用现有 header/auth 行为，避免订阅特例扩大到其他目标。

### 7.7 响应头、响应体与错误

- 2xx + `text/event-stream`：按现有 SSE 管道逐事件透传，并采集 TTFT/usage；网络 chunk 边界不作保证。
- 2xx 非 SSE：按现有 body 大小上限返回语义等价的 body 并提取 usage。由于 Go transport 可以协商并透明解压响应，这里不承诺压缩字节一致。
- 401/403/429/5xx：在大小上限内返回上游状态和 body；401/403 标记为认证失败，`apid` 不重试、不切换上游。
- 3xx：订阅专用 client 不跟随；handler 不把 `Location` 返回给 Codex，而是返回固定 `502 upstream redirect blocked`，避免客户端产生第二跳。
- 网络错误：返回 502；对客户端、日志和数据库只暴露错误类别及固定 upstream 名称，不包含可能带 query 的完整 URL。
- 错误 body 超过上限时不能静默截断后仍伪装成完整上游响应；应返回固定 502、标记 `upstream_error_body_too_large`，并关闭上游 body。

响应头采用显式策略：

- 允许返回 `Content-Type`、仍与 body 一致的 `Content-Encoding`、`Retry-After`、官方 request id 和 rate-limit 诊断头；
- 删除 hop-by-hop、失效的 `Content-Length`、`Set-Cookie` 和 3xx `Location`；
- 若 Go transport 已透明解压，不能再返回原始 `Content-Encoding`；
- 诊断头可用于响应和有界指标，但不能与认证头、Cookie 或账号标识一起记录。

当前 `passUpstreamError` 会把完整上游错误 body 写入日志，并忽略 `readLimited` 的截断错误。订阅模式必须改为敏感错误策略：body 可以在大小上限内返回客户端，但日志和数据库只保存状态码、OpenAI request id 和代码定义的错误类别。Phase 1 不保存任何从错误 body 提取的“脱敏摘要”，因为正则脱敏无法可靠保证 prompt、账号或 token 不残留。

### 7.8 资源与超时预算

MVP 继续整包缓存 inference/compact 请求，以便保留 payload 字节并做有界元数据 sniff，但必须显式记录其代价：

- 当前 `maxRequestBody` / `maxResponseBody` 为 10 MiB，`maxErrorBodySize` 为 1 MiB；不能直接假设 10 MiB 足以覆盖 Codex 长上下文和 compact，人工验收必须包含接近上限的真实请求；
- 同时在途请求的最坏请求体内存约为 `并发数 × maxRequestBody`，后续如需提高上限应优先考虑订阅 route 专用配置或流式上传设计，不能无评估地全局提高到 cc-switch 的 200 MiB；
- 连接、TLS 和响应头继续使用有界超时；compact 使用非流式总超时，inference 在 stream unknown/yes 时不使用总超时；
- SSE 增加可配置的静默超时 `APID_CODEX_SSE_IDLE_TIMEOUT`（Go duration，默认 `5m`），与示例中的 Codex `stream_idle_timeout_ms = 300000` 对齐；每次收到上游字节后重置，客户端断开和服务关闭必须立即取消；
- 现有 `bufio.Reader.ReadBytes('\n')` 允许单行无限增长，订阅 SSE 必须增加有界事件/单行大小；超限后终止流并记录固定错误类别，不能把事件内容写入日志；
- 请求体读取也应有独立 deadline，并在开始向客户端返回长 SSE 前清除，避免用全局 `ReadTimeout` 误杀正常流式响应。

## 8. 对现有包的改动

| 位置 | 设计改动 |
|---|---|
| `config/config.go` | 增加 `AuthMode`、`RouteOperation`、固定目标校验、订阅 route 单一 catch-all；在完整 Config 加载后做 loopback 校验 |
| `config/config_test.go` | 表驱动覆盖全部合法/非法组合和向后兼容配置 |
| `upstream/client.go` | 增加明确认证策略；订阅 client 使用代码固定 URL、直连、禁止 redirect/POST replay；新增 compact 转发入口 |
| `upstream/client_test.go` | 验证 Bearer 只到固定目标、本地 key/Cookie 被删除、业务头保留、环境代理失效、重定向不跟随 |
| `server/server.go` | 路由级认证；目标解析后再生成上游头；stream 使用三态；按 operation 选择 endpoint；订阅指标不记录 query |
| `server/forward.go` | 以实际响应 Content-Type 决定 SSE；订阅模式不改写 body；应用响应头策略；3xx 映射 502；敏感错误不落 body，超限不静默截断；SSE 单行/静默期有界 |
| `stats` / `server/live.go` | stream unknown 仅保留在响应头到达前，最终记录实际类型；订阅 ClientPath/UpstreamURL 使用无 query 的规范值 |
| `trace` 调用点 | 订阅 upstream 跳过 request/upstream body dump，且不把原始 query 写入 trace metadata |
| `server/topology.go` | 展示 `auth_mode = codex_subscription`，继续隐藏任何密钥；固定 endpoint 可展示 |
| `config.example.toml` | 增加实验性订阅代理示例和 loopback 警告 |
| `README.md` | 增加 Codex 侧配置、登录前提、无 `/models` 前提、兼容版本、稳定性和安全边界 |

不建议新增一个宽泛的 `proxy` 包。认证决策属于 `config`，HTTP 凭证应用属于 `upstream`，请求编排属于 `server`，符合当前分层。

## 9. 安全模型

| 风险 | 控制措施 |
|---|---|
| 配置把订阅令牌转发到恶意域名 | `codex_subscription` 只接受代码固定的 HTTPS origin/path，运行时仍从代码常量构造 URL |
| 上游重定向导致凭证越界或客户端第二跳 | 专用 client 禁止自动跟随 3xx；handler 映射 502 且删除 Location |
| 本地网关密钥覆盖 OpenAI Bearer | 订阅 route 只用 `X-Apid-Key` 做本地认证 |
| 订阅接口被局域网其他机器调用 | MVP 强制 loopback 监听；拒绝 `:port`、`0.0.0.0` 等地址 |
| model 字段把请求切换到普通/恶意 upstream | 一条订阅 route 只能引用订阅模式 upstream，且必须恰好一条 catch-all |
| TRACE 泄露 prompt 或工具输出 | 订阅模式默认不写请求体 TRACE；后续若开放必须另设显式危险开关 |
| query、错误 body 或账号头进入日志/SQLite | 订阅指标只存规范化无 query URL；不记录错误 body、认证头和 `ChatGPT-Account-Id` |
| 本地 Cookie 或其他 origin 凭证被带到官方后端 | 订阅专用请求头策略显式删除 Cookie、本地 API key 和代理认证头 |
| 环境代理看到凭证 | 订阅 transport 默认直连，不继承代理环境变量 |
| 请求重试产生重复副作用 | `apid` 不做应用层重试或 POST replay；示例把 Codex provider 的 request/stream retries 设为 0；认证刷新重发单独记录 |
| 压缩炸弹或超大上下文耗尽内存 | 原始 body 有硬上限；未来元数据解压同时限制压缩大小、解压大小和压缩比 |
| 把 auth mode 误当成订阅资格证明 | 不解析 token，不展示套餐结论；权限完全由官方上游的响应决定 |
| 本地进程窃取 Codex 凭证 | 无法由 HTTP 代理彻底消除；loopback 不是进程隔离。用户必须信任运行 `apid` 的主机和二进制 |

最后一项是该方案固有边界：任何被 Codex 配置为 provider 的本地程序都能看到 Bearer token。文档和启动日志必须明确提醒用户。

## 10. 可观测性

保留：

- upstream 名称、无 query 的固定 URL、客户端/上游模型；
- 状态码、总耗时、TTFT、以实际响应类型为准的 stream 标记；
- Responses usage、cached tokens；
- OpenAI request id 等不含凭证的诊断头。

降级或关闭：

- 压缩且未解码请求的 model 可能为空；
- 订阅 route 不写 TRACE body；
- 原始 query、`ChatGPT-Account-Id` 和认证相关头不进入 access log、SQLite 或 TRACE；
- 上游错误 body 完全不写 access log/SQLite；`stats.Record.Error` 只写代码定义的类别。

建议在 topology 中把该上游标为 `experimental / credential passthrough`，让运维侧能看出它与普通 API Key upstream 的差异。

## 11. 测试计划

### 11.1 配置测试

- 合法订阅 upstream + inference/compact routes 能加载。
- 非固定 host、HTTP scheme、错误 path、userinfo、配置 query/fragment、非 Responses 协议均拒绝。
- 设置 `api_key`、upstream model 或规则 model 改写时拒绝。
- 订阅 route 混入普通 upstream 时拒绝。
- 无 catch-all、多个 catch-all、额外精确/glob 规则、非 loopback 监听时拒绝。
- `localhost`、空 host、zone IPv6、`:port`、`0.0.0.0` 均拒绝；`127.0.0.1` 和 `[::1]` 接受。
- 未设置 `auth_mode` 的旧配置行为不变。

### 11.2 Upstream 单元测试

- 普通模式仍执行现有 API Key 替换/回退。
- 订阅模式只透传恰好一个 Bearer Authorization；缺失、空值、重复头、多个 scheme 和逗号列表均拒绝。
- `X-Apid-Key`、`X-Api-Key`、Cookie、代理头不会到达测试上游。
- `ChatGPT-Account-Id`、`OpenAI-Beta` 等业务头保留。
- inference/compact 只能命中两个允许路径。
- body 与 `Content-Encoding` 保持不变；使用 SHA-256 断言未压缩和压缩 payload 都没有被重新序列化。
- 客户端 query 只能附加到固定官方 path，不能改变 origin/path。
- 设置假的 `HTTP_PROXY`/`HTTPS_PROXY` 后请求仍不经过该代理。
- 302 到另一 host 时只产生一次上游请求，client 返回首个 302 response 供 server 分类。
- 一次 handler 尝试最多一次 outbound round trip，取消客户端 context 会关闭上游请求。

测试时允许通过仅测试可见的构造函数注入 `httptest.Server`；生产构造函数仍必须固定官方目标。

### 11.3 Server 集成测试

- `client_api_key` 开启时，`X-Apid-Key` 认证成功且 OpenAI Bearer 不被删除。
- 用 Authorization 代替 `X-Apid-Key` 时本地认证失败。
- SSE 请求逐事件透传，首 token 及时 flush，TTFT/usage 正确。
- 请求中 `stream` 不可解析，但响应为 SSE 时仍按流转发且不触发非流总超时。
- stream unknown 在响应头到达前不被记录成 false，最终 stats 使用实际响应类型。
- compact 请求命中 `/responses/compact`。
- 请求体在上限边界成功、超过上限 1 字节时返回 413；并发请求的内存预算通过测试或 benchmark 记录。
- 401/403、429、5xx 的状态和有界 body 返回正确；超限错误 body 返回固定 502，不静默截断。
- 上游 302 被映射为固定 502 且下游没有 Location；安全响应头正确返回，`Set-Cookie`、hop-by-hop、失效 Content-Length 被删除。
- SSE 静默超时在收到字节后重置；单行超限会终止但不会把事件正文写入日志。
- query 中放置测试 secret，确认 access log、SQLite、stats API、TRACE 和错误文本均搜索不到该 secret。
- Authorization、Cookie、`ChatGPT-Account-Id` 和上游敏感错误 body 使用不同 sentinel，逐一确认日志和数据库均不存在。
- 开启全局 TRACE 时，普通 route 仍可落盘，订阅 route 不产生 body 文件且 metadata 不含 query。

### 11.4 人工验收

使用一次性测试账号或专门的测试订阅：

1. `APID_LISTEN=127.0.0.1:19092 go run .`。
2. 记录 Codex CLI 精确版本；若宣称支持 App，另行记录 App build 和平台。
3. Codex 已用 ChatGPT 登录，切换到 `apid-codex-subscription` provider，并确认无需 `/models` 也能启动和选择目标模型。
4. 完成普通回答、工具调用、长 SSE、上下文 compact，并记录实际 endpoint/header/content-encoding。
5. 确认官方订阅侧产生对应使用量，`apid` stats 可见请求但文件、日志和 SQLite 中搜索不到 Bearer/query/account-id sentinel。
6. 停止 `apid` 后确认 Codex 明确报本地连接失败，而不是静默绕过到其他上游。

CI 不保存真实 ChatGPT 凭证，也不调用内部订阅后端。

## 12. 分阶段交付

### Phase 1：安全 MVP

- 显式 `codex_subscription` auth mode；
- loopback 强制校验；
- `/responses`、`/responses/compact`；
- HTTP/SSE、固定目标、3xx 映射 502、无 `apid` 重试/POST replay；
- 路由级 `X-Apid-Key`；
- query/响应头策略和敏感 TRACE/错误日志保护；
- stream 三态、请求/响应/SSE 资源上限和静默超时；
- 手工 Codex 配置、无 `/models` 前提和兼容版本记录。

### Phase 2：兼容性与易用性

- 对压缩请求副本做有界 gzip/deflate/zstd/brotli 元数据解析；
- 增加 `apid codex install/status/uninstall`，以原子方式修改和恢复用户级 Codex 配置；
- 提供令牌不落盘的诊断检查与兼容性报告。

### Phase 3：受控远程部署（可选）

只有出现明确需求后再设计。至少需要 TLS、独立访问控制、可信反向代理边界和显式风险开关；不能通过简单允许 `0.0.0.0` 完成。

## 13. 验收标准

实现满足以下条件才算完成：

- Codex 使用 ChatGPT 官方登录，能经本地 `apid` 完成 Responses 普通与 SSE 请求。
- compact 可用，WebSocket 明确关闭；支持矩阵中的 Codex 版本无需 `/models` 也能正常启动和选模。
- 订阅 inference/compact 请求 payload 的 SHA-256 与客户端原始 body 一致；文档不声称 HTTP framing 或响应压缩字节一致。
- `apid` 不读取或修改 Codex 的认证文件，不保存或打印 OpenAI Bearer、原始 query、Cookie 或 `ChatGPT-Account-Id`。
- 对任意配置、model、query、redirect 输入，订阅 Bearer 都不能到达固定官方 origin 之外。
- 上游 3xx 不产生第二个 outbound request，也不向 Codex 返回 Location。
- 一次 `apid` handler 尝试至多一个 outbound round trip；Codex 侧透明重试是否关闭由示例配置和验收记录明确说明。
- 开启现有 `client_api_key` 时，订阅流量仍能同时完成本地认证和上游认证。
- 现有协议转换、普通纯转发和 Anthropic 路由测试全部通过。
- 文档记录已验证的 Codex CLI/App 版本，并明确标注实验性、内部后端稳定性、无 `/models` 前提和账号风险边界。

## 14. 推荐的实现决策

首版应采用“手工配置 + 本地安全代理”，不要同时实现 Codex 配置文件改写、账号管理、模型目录或远程共享。核心代码改动仍集中在 `config`、`upstream`、`server`，只为 stream 三态和 query 脱敏对 stats/live 做必要的窄调整。

后续实现时，优先顺序应是：配置不变量和安全测试 → 专用 upstream 认证/固定 URL → route-aware 客户端认证 → compact/SSE 与 stream 三态 → 3xx/响应头/资源限制 → 敏感可观测性 → 兼容版本验收与使用文档。任何自动配置或模型目录功能都应在代理核心稳定后另立设计。
