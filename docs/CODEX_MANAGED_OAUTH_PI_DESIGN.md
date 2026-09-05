# Codex 托管 OAuth 与 Pi 接入设计

状态：Draft

日期：2026-08-04

范围：`apid` 托管一个 ChatGPT Codex 登录账号，通过本地 OpenAI Responses 兼容入口向 Pi 等客户端提供 Codex 订阅推理能力。

前置设计：[Codex ChatGPT 订阅本地代理设计](./CODEX_SUBSCRIPTION_PROXY_DESIGN.md)。前置设计描述由 Codex 客户端自行持有凭据的 `codex_subscription` 透传模式；本文描述新增且互不替代的 `codex_oauth` 托管模式。

## 1. 结论

在 `apid` 中实现 Codex 单账号托管 OAuth 是可行的，推荐把控制面和数据面分开：

1. `apid-ctl auth codex login/status/logout` 负责交互式登录和本地凭据管理。
2. 独立的 `codexauth` 包负责 OAuth、凭据存储、到期判断、并发刷新和 refresh token 轮换。
3. 常驻的 `apid` 在请求路径上按需取得 access token，提前 5 分钟刷新，并在上游 401 时强制刷新、最多重放一次。
4. 新增 `auth_mode = "codex_oauth"`；客户端只提交 `client_api_key`，OpenAI 凭据不经过 Pi，也不复用 `~/.codex/auth.json`。
5. Pi 通过自定义 `openai-responses` provider 调用本地 `/v1/responses`；同协议请求进入 Codex 私有后端兼容层，不经过通用 Responses → Chat 转换。
6. `/v1/models` 从 apid 配置中的精确模型规则生成，不调用未公开的远端模型目录。

该功能继续标记为 experimental。OAuth 登录本身可以按当前 Codex 契约实现，但 `https://chatgpt.com/backend-api/codex` 不是面向第三方承诺稳定性的公共 API，请求字段、请求头和模型可用性都需要版本化验收。

## 2. 与现有模式的关系

现有 `codex_subscription` 已支持原生 Codex 客户端把自身 OpenAI Bearer 交给 apid，并由 apid 安全地转发到固定后端。托管模式不改变它，而是增加一条适合 Pi 和其他普通 Responses 客户端的路径。

| 项目 | `codex_subscription` | `codex_oauth` |
|---|---|---|
| 凭据持有者 | Codex 客户端 | apid |
| 登录与刷新 | Codex 客户端 | `apid-ctl` 登录，`apid` 刷新 |
| 客户端 Authorization | OpenAI Bearer | apid 本地 API key |
| 请求体 | Codex 原生 payload，语义透传 | 标准 Responses payload，经 Codex 兼容归一化 |
| 典型客户端 | Codex CLI / App | Pi、其他 Responses SDK |
| 路由混用 | 一条 route 只能是订阅透传 | inference route 可与普通 upstream 按精确模型混用 |
| 401 行为 | 原样返回，不刷新 | 强制刷新并重放一次 |

必须保留这两个认证模式的不同语义，不能把 `codex_subscription` 改造成托管模式，否则会破坏现有 Codex provider 配置和安全不变量。

## 3. 目标与非目标

### 3.1 目标

- 支持一个本地 ChatGPT Codex 登录账号，由 apid 自动维护 access token。
- 支持浏览器 PKCE 登录，以及无图形环境下的手动回调输入。
- 支持 `POST /v1/responses` 的文本、图片、reasoning、函数工具调用和 SSE。
- 支持客户端请求 `stream = false`，即使私有上游要求流式，也向客户端返回标准非流式 Responses JSON。
- 支持固定的 `/responses/compact` 操作，作为兼容性能力独立配置。
- 支持 Pi 通过 `~/.pi/agent/models.json` 接入。
- 保留 apid 的模型路由、统计、活跃请求和拓扑能力。
- 凭据刷新不会因并发请求重复消费同一 refresh token。
- `codex_subscription`、普通 API Key upstream 和 Responses → Chat 转换不回归。

### 3.2 非目标

- 不实现多账号、账号池、轮询、额度故障转移或自动换号。
- 不把 Claude Pro/Max 订阅转换成 API。
- 不增加 Anthropic Messages ↔ Responses 转换；Pi 直接使用 Responses。
- 不读取、修改或备份 `~/.codex/auth.json`，也不读取 Pi 自己的 OAuth 凭据。
- 不提供公网部署方案；第一阶段仍要求 IP-literal loopback 监听。
- 不增加浏览器可访问的管理 HTTP API，不允许通过远程请求登录、导出或删除凭据。
- 不支持 WebSocket；先以 HTTP/SSE 为兼容基线。
- 不从私有 Codex 后端动态同步模型列表。
- 不创建通用 OAuth provider 框架；先实现具体的 Codex 登录域。
- 不保证 ChatGPT 套餐、模型权限或内部 endpoint 长期不变。

## 4. 核心不变量

1. Pi 只持有 apid 本地 API key，永远不接触 OpenAI access token 或 refresh token。
2. `codex_oauth` 只能访问代码固定的 HTTPS origin 和固定 operation，不能由请求或普通配置改变目标。
3. 客户端的 `Authorization`、`X-Api-Key` 和 `Api-Key` 在本地鉴权后全部剥离，由 apid 重新注入 OpenAI Authorization。
4. OpenAI 凭据不进入 TOML、SQLite、TRACE、访问日志、错误响应或 `/stats/*`。
5. 托管请求的敏感策略由解析后的 target 决定，不能继续用“整个 path 是否为 subscription route”代替。
6. 同一进程内，同一个账号在任意时刻最多有一个 refresh 请求在执行。
7. refresh token 发生轮换时不能丢弃新值；持久化采用同目录临时文件和原子 rename。
8. 上游 401 最多触发一次刷新和一次重放，且只允许发生在下游响应尚未提交时。
9. SSE 已经向客户端写出任何响应后，不做透明重试。
10. `codex_subscription` 继续透传客户端凭据，不使用 `codexauth.Manager`，也不改变原有 401 行为。
11. 同一个凭据文件只允许一个运行中的 apid 实例负责刷新；多实例必须使用独立凭据文件。
12. `codex_oauth` upstream 存在时，必须配置非空 `client_api_key` 且 `APID_LISTEN` 必须是 IP-literal loopback。

## 5. 总体架构

### 5.1 控制面

```text
用户
  │ apid-ctl auth codex login
  ▼
apid-ctl
  │ PKCE + state + 127.0.0.1 callback
  ▼
OpenAI OAuth authorize/token endpoint
  │ access token + refresh token + expires_in
  ▼
codexauth.Store
  │ 0600 + atomic rename
  ▼
~/.config/apid/codex.json
```

`apid-ctl` 直接操作本地凭据文件，不要求 apid 正在运行，也不通过管理 HTTP API 联系 apid。

### 5.2 数据面

```text
Pi
  │ POST http://127.0.0.1:19092/v1/responses
  │ Authorization: Bearer <local apid key>
  ▼
server
  │ 本地鉴权 → 读取 body → 按 model Resolve
  │ 剥离客户端认证头
  ▼
Codex 请求兼容层
  │ 归一化标准 Responses payload
  ▼
codexauth.Manager
  │ 内存快路径 / singleflight refresh / 原子落盘
  ▼
upstream Codex client
  │ 固定 URL + apid 注入的 OpenAI Bearer
  ▼
https://chatgpt.com/backend-api/codex/responses
  │ Responses SSE
  ▼
server → Pi
```

### 5.3 包职责

| 包或入口 | 新职责 |
|---|---|
| `cmd/apid-ctl` | 子命令分发，以及 Codex login/status/logout 的终端交互 |
| `codexauth` | OAuth、PKCE、本地 callback、凭据 Store、Token Manager |
| `config` | `codex_oauth` 校验和凭据文件路径运维配置 |
| `server` | 目标级敏感策略、401 一次重试、非流式聚合、`/v1/models` |
| `upstream` | 固定 Codex endpoint、请求归一化、受控请求头和禁止重定向 |
| `convert` | 不改；托管路径仍是 Responses → Responses |
| `protocol` | 原则上不改；只有归一化确实需要缺失字段类型时才补充 |

不增加 `provider/`、`executor/`、`repository/` 等抽象层。OAuth 和刷新先以具体 `codexauth.Manager` 实现；未来出现第二种真实托管认证后，再由使用方提取最小接口。

## 6. 凭据与控制面设计

### 6.1 默认位置

Linux 默认凭据文件：

```text
~/.config/apid/codex.json
```

当前机器对应：

```text
/home/ruofeng/.config/apid/codex.json
```

代码使用 `os.UserConfigDir()` 取得平台配置目录，在其他平台自然落到对应用户配置目录。可以通过环境变量覆盖：

```bash
export APID_CODEX_AUTH_FILE=/secure/path/codex.json
```

`apid-ctl` 和 `apid` 必须解析到同一个文件。启动日志可以记录路径，但不能记录文件内容、账号 ID 或 token 摘要。

### 6.2 文件格式

建议采用带版本号的私有 JSON：

```json
{
  "version": 1,
  "provider": "codex",
  "token_type": "Bearer",
  "access_token": "<secret>",
  "refresh_token": "<secret>",
  "expires_at": "2026-08-04T10:32:10Z",
  "account_id": "<private>"
}
```

- 父目录权限 `0700`，文件权限 `0600`。
- 不持久化完整 JWT claims、email 或不参与请求的个人资料。
- `expires_at` 优先由 token endpoint 的 `expires_in` 计算，JWT `exp` 只作兼容性回退。
- `account_id` 只用于构造官方后端要求的请求头，不参与授权或模型路由。
- 写入流程为：同目录创建 `0600` 临时文件、写入、`fsync`、rename、同步父目录。
- 解析时拒绝未知主版本、空 refresh token、异常大的文件和宽松 JSON 尾随内容。

### 6.3 `apid-ctl` 命令

现有无子命令用法继续兼容为 sessions 查询：

```bash
apid-ctl --tool codex -n 50
apid-ctl sessions --tool codex -n 50
```

新增命令：

```bash
apid-ctl auth codex login
apid-ctl auth codex login --no-browser
apid-ctl auth codex status
apid-ctl auth codex status --json
apid-ctl auth codex logout
```

登录流程：

1. 生成高熵 `state`、PKCE verifier 和 S256 challenge。
2. 只在 `127.0.0.1:1455` 启动临时 callback server，不监听 `0.0.0.0`。
3. 默认打开浏览器；`--no-browser` 打印 URL，并允许用户粘贴最终 callback URL。
4. callback 必须验证 state、code、错误参数和固定 path。
5. 使用带总超时的专用 HTTP client 交换 token。
6. 验证并保存凭据后立即关闭 callback server。

`status` 只输出登录状态、过期时间和文件路径，不输出 token。`logout` 删除凭据文件前在交互终端确认；脚本模式要求显式 `--yes`。

第一阶段不实现运行中热加载。执行 login/logout 后必须重启 apid，避免引入高权限管理端点、文件监听和跨进程状态同步。

## 7. 配置设计

### 7.1 新认证模式

新增常量：

```go
const AuthModeCodexOAuth AuthMode = "codex_oauth"
```

`codex_oauth` upstream 的加载约束：

- `protocol` 必须为 `openai_responses`。
- `base_url` 必须精确为 `https://chatgpt.com/backend-api/codex`。
- `path` 必须精确为 `/responses`。
- `api_key`、`supports_responses` 和 `responses_path` 必须为空或关闭。
- upstream 和路由规则不得改写 model；客户端模型必须原样发往 Codex。
- 全局 `client_api_key` 必须非空。
- `APID_LISTEN` 必须是 IP-literal loopback。
- 加载到至少一个 `codex_oauth` upstream 时，凭据文件必须存在、格式合法且包含 refresh token；access token 过期不阻止启动。

### 7.2 路由混用

托管模式使用普通的 apid 本地鉴权，因此 inference route 可以按精确模型与普通 upstream 混用：

```text
精确 gpt-5.6-sol   → codex_oauth
精确 gpt-5.6-terra → codex_oauth
glob deepseek-*    → DeepSeek
catch-all *        → OpenAI API
```

这要求把当前 path 级 `subscriptionRoutes` 拆分为：

- `credentialPassthroughRoutes`：只服务旧 `codex_subscription`，继续整条 route 隔离。
- target 级 `isCodexManaged`：在 `Resolve` 后决定 TRACE、header、错误和重试策略。

`responses_compact` 的请求不保证包含可用于分派的 model，因此仍要求一条唯一的 `match = "*"` 规则，不能与其他 upstream 混用。

### 7.3 示例配置

```toml
client_api_key = "replace-with-a-long-random-local-key"

[[upstream]]
name = "codex-managed"
protocol = "openai_responses"
base_url = "https://chatgpt.com/backend-api/codex"
path = "/responses"
auth_mode = "codex_oauth"

[[route]]
path = "/v1/responses"
input_protocol = "openai_responses"

  [[route.model]]
  match = "gpt-5.6-sol"
  upstream = "codex-managed"
  model = ""

  [[route.model]]
  match = "gpt-5.6-terra"
  upstream = "codex-managed"
  model = ""

[[route]]
path = "/v1/responses/compact"
input_protocol = "openai_responses"
operation = "responses_compact"

  [[route.model]]
  match = "*"
  upstream = "codex-managed"
  model = ""
```

运维配置：

```bash
APID_LISTEN=127.0.0.1:19092
APID_CODEX_AUTH_FILE=/home/ruofeng/.config/apid/codex.json
```

## 8. 请求处理设计

### 8.1 服务启动

当配置中存在 `codex_oauth`：

1. 校验 loopback、`client_api_key`、固定 upstream 和路由约束。
2. 打开凭据文件并进行结构验证。
3. 构造一个进程级 `codexauth.Manager`，所有托管 upstream 共享。
4. 不在启动阶段访问 OAuth endpoint；过期 token 在第一次请求时按需刷新。
5. 缺少凭据时启动失败，错误明确提示执行 `apid-ctl auth codex login`。

不把 OAuth 服务临时不可用变成整个网关的启动依赖；普通 upstream 可以正常启动和工作。

### 8.2 本地鉴权与路由

Pi 请求先按普通 route 使用 `client_api_key` 鉴权。鉴权完成后：

1. 删除客户端 `Authorization`、`Proxy-Authorization`、`X-Api-Key` 和 `Api-Key`。
2. 按现有请求体大小上限读取 body，sniff `model` 和 `stream`。
3. 执行精确 > glob > catch-all 的 `Resolve`。
4. 只有 target 确认为 `codex_oauth` 后，才进入托管敏感路径。
5. target 是普通 upstream 时，保持现有转发或转换行为。

客户端无法通过伪造 header 改变上游凭据，也无法让 apid 把 OpenAI token 发往配置中的普通 URL。

### 8.3 Codex 请求归一化

原生 Codex 客户端已经生成私有后端兼容 payload，旧 `codex_subscription` 因此可以语义透传。Pi 发送的是标准 Responses 请求，`codex_oauth` 必须增加一层窄兼容处理。

首版兼容基线：

| 字段或行为 | 处理 |
|---|---|
| `model` | 保持客户端值，不做隐式改写 |
| `stream` | 上游固定为 `true`；客户端非流式时由 apid 聚合 SSE |
| `store` | 固定为 `false` |
| `instructions` | 缺失时补兼容默认值，已有值保持 |
| 文本、图片输入 | 保持标准 Responses 结构 |
| function tools/tool choice | 保持并做当前 Codex 所需的窄归一化 |
| reasoning | 保持客户端 effort/summary 语义，删除已确认不兼容的扩展字段 |
| `previous_response_id` | 删除；apid 不依赖私有后端保存会话状态 |
| `generate`、`prompt_cache_retention`、`safety_identifier`、`stream_options` | 按当前兼容基线删除 |
| 未知字段 | 默认保留；只有真实兼容性证据和回归测试才能加入删除列表 |

归一化属于“同协议私有后端兼容”，不进入 `convert` 包。实现集中在一个 Codex 专用文件，避免把供应商私有规则散落在通用协议结构中。

### 8.4 上游请求头

托管请求只从白名单构造头部：

- `Authorization: Bearer <manager access token>`
- `Chatgpt-Account-Id: <stored account id>`
- Codex 当前兼容基线要求的 `Originator`、User-Agent 和会话/cache headers
- 受控的 `Content-Type`、`Accept` 和内容编码头

以下内容不得由客户端覆盖：

- `Authorization`
- `Chatgpt-Account-Id`
- `Originator`
- `Host`
- `Content-Length`
- hop-by-hop、代理和 CDN 认证头

HTTP client 复用现有订阅安全 transport：不读取代理环境变量、不跟随重定向、固定 TLS origin，并保留适合长时间 SSE 的连接与响应头超时。

### 8.5 响应处理

- 客户端 `stream = true`：上游 2xx SSE 按 Responses 事件语义流式返回，同时复用现有 usage、TTFT 和活跃 token 采集。
- 客户端 `stream = false`：解析上游 SSE，累加为最终 Responses response JSON 后一次性返回。
- 上游响应提交前可以处理 401 刷新；提交后任何断流只记录错误并结束，不重放。
- 3xx 映射为固定 502，不暴露 `Location`。
- 错误 body 受大小限制；日志只记录状态、请求 ID 和错误类别。
- 托管路径默认禁止完整请求/响应 TRACE；统计只保留脱敏模型和用量。

## 9. Token 刷新设计

### 9.1 总体策略

采用“按需提前刷新 + 401 兜底”，不运行固定周期的后台刷新 goroutine：

```text
请求到达
  │
  ├─ token 距过期 > 5m ───────────────→ 直接返回内存 token
  │
  └─ token 已进入刷新窗口/已过期
       │
       └─ singleflight refresh
            │
            ├─ 成功 → 原子保存 → 更新内存 → 继续请求
            └─ 失败 → 按错误类型降级或要求重新登录
```

建议初始常量：

| 参数 | 初始值 |
|---|---:|
| 提前刷新窗口 | 5 分钟 |
| OAuth token 请求总超时 | 15 秒 |
| 临时失败初始退避 | 30 秒 |
| 最大退避 | 5 分钟 |
| 上游 401 强制重试 | 1 次 |

首版不暴露这些值为配置项；只有实际运行数据证明需要调整时再增加运维参数。

### 9.2 内存状态

`codexauth.Manager` 持有不可变凭据快照和少量状态：

```go
type Manager struct {
	store *Store
	http  *http.Client
	// mutex/atomic protected credential snapshot
	// singleflight group, retry deadline and last refresh category
}
```

建议状态：

| 状态 | 含义 |
|---|---|
| `ready` | access token 在正常有效期内 |
| `refresh_due` | 已进入提前刷新窗口 |
| `refreshing` | 一个共享刷新正在执行 |
| `persist_pending` | 已取得轮换后的凭据，但落盘失败，不能退回旧 refresh token |
| `reauth_required` | refresh token 被撤销或返回 `invalid_grant` |

正常请求只读取内存快照，不每次读取凭据文件，也不执行 `stat`。这就是执行 login/logout 后要求重启 apid 的原因。

### 9.3 并发刷新

多个请求同时进入刷新窗口时，使用 `singleflight` 合并为一次 token endpoint 调用。刷新函数内部必须在拿到执行权后再次检查 token，避免前一个请求已经刷新后重复调用。

等待者使用自己的请求 context 等待结果；某个 Pi 请求取消时，只取消它自己的等待，不取消其他请求共享的刷新。实际 OAuth 请求使用 manager 生命周期派生、带 15 秒超时的 context，apid 关闭时统一取消。

### 9.4 刷新成功与轮换

刷新响应处理顺序：

1. 验证 access token、token type、有效期和可选的新 refresh token。
2. token endpoint 返回新 refresh token 时替换；没有返回时保留原值。
3. 从新 access token 提取 account ID，并与登录账号保持一致。
4. 把完整新凭据原子写入 Store。
5. 发布新的内存快照，唤醒全部等待请求。

如果 token endpoint 已轮换 refresh token，但文件写入失败，不能丢弃新凭据或再次使用旧 refresh token。Manager 应把新凭据保留为 `persist_pending`，继续使用仍有效的新 access token，并在后续请求中优先重试落盘；日志记录高优先级错误。若此时进程退出，用户需要重新登录，这是无法成功持久化时的明确故障边界。

### 9.5 刷新失败

| 情况 | 行为 |
|---|---|
| 临时网络/5xx，旧 access token 尚未过期 | 记录 warning，在退避期内继续使用旧 token |
| 临时网络/5xx，access token 已过期 | 返回 503，退避到期后允许再次刷新 |
| token endpoint 返回 `invalid_grant` | 标记 `reauth_required`，不持续撞击 endpoint |
| 凭据文件缺少 refresh token | 启动失败并提示 login |
| account ID 与原账号冲突 | 拒绝刷新结果并要求重新登录 |
| 本地持久化失败 | 进入 `persist_pending`，不回退旧 refresh token |

退避只针对 token endpoint 的临时失败，不影响仍有充足有效期的 access token 快路径。

### 9.6 401 兜底

上游请求记录自己使用的 token 版本。收到 401 且响应尚未提交时：

1. 仅当当前缓存仍等于该请求使用的 token 时将其失效；旧请求不能使其他请求刚刷新的 token 失效。
2. 通过同一个 singleflight 通道执行强制刷新。
3. 使用新 token 和原始已缓冲请求体重放一次。
4. 第二次仍为 401 时停止，不循环刷新，返回 `codex_reauthentication_required`。

403 不触发刷新，因为它更可能表示账号、套餐、模型或策略无权限。429、5xx 和网络错误同样不触发 OAuth 401 逻辑。

客户端本地 API key 已经验证成功，因此最终的上游认证失败不应返回普通 401，避免 Pi 将其误判为本地 key 错误。建议响应：

```http
HTTP/1.1 503 Service Unavailable
Content-Type: application/json
```

```json
{
  "error": {
    "type": "authentication_error",
    "code": "codex_reauthentication_required",
    "message": "Codex login expired; run apid-ctl auth codex login"
  }
}
```

## 10. `/v1/models` 设计

新增只读入口：

```http
GET /v1/models
Authorization: Bearer <local apid key>
```

模型目录以当前加载配置为唯一来源：

1. 扫描 inference route 的精确 `[[route.model]].match`。
2. 排除 `*`、空 match 和 glob，因为它们不是具体模型 ID。
3. 按 ID 去重并稳定排序。
4. 不向 Codex 私有后端发目录请求。
5. 使用与 inference route 相同的 `client_api_key` 鉴权。

示例响应：

```json
{
  "object": "list",
  "data": [
    {"id": "gpt-5.6-sol", "object": "model", "created": 0, "owned_by": "apid"},
    {"id": "gpt-5.6-terra", "object": "model", "created": 0, "owned_by": "apid"}
  ]
}
```

新增模型时修改精确 route 规则并重启。`/v1/models` 不能替代 Pi 的模型元数据配置，因为 Pi 还需要 reasoning、上下文窗口、最大输出和输入模态等字段。

## 11. Pi 接入与最终使用方式

### 11.1 首次登录

```bash
go install ./cmd/apid-ctl
apid-ctl auth codex login
apid-ctl auth codex status
```

### 11.2 启动 apid

```bash
export APID_CODEX_AUTH_FILE="$HOME/.config/apid/codex.json"
export APID_LISTEN=127.0.0.1:19092
apid --config /path/to/config.toml
```

验证本地目录：

```bash
curl http://127.0.0.1:19092/v1/models \
  -H "Authorization: Bearer $APID_API_KEY"
```

### 11.3 配置 Pi

Pi 当前从 `~/.pi/agent/models.json` 加载自定义 provider。配置示例：

```json
{
  "providers": {
    "apid": {
      "name": "Local apid",
      "baseUrl": "http://127.0.0.1:19092/v1",
      "apiKey": "$APID_API_KEY",
      "api": "openai-responses",
      "authHeader": true,
      "models": [
        {
          "id": "gpt-5.6-sol",
          "name": "GPT-5.6 Sol via apid",
          "reasoning": true,
          "input": ["text", "image"],
          "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
          "contextWindow": 272000,
          "maxTokens": 32000
        },
        {
          "id": "gpt-5.6-terra",
          "name": "GPT-5.6 Terra via apid",
          "reasoning": true,
          "input": ["text", "image"],
          "cost": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
          "contextWindow": 272000,
          "maxTokens": 32000
        }
      ]
    }
  }
}
```

`cost = 0` 只表示 Pi 本地不估算按 token API 费用，不表示订阅没有成本或额度约束。模型 ID、上下文窗口和最大输出必须按兼容性验收结果维护，不能从示例推断为永久契约。

启动：

```bash
export APID_API_KEY="replace-with-the-same-local-key"
pi --provider apid --model gpt-5.6-sol --thinking high
```

也可以使用带 provider 的模型名：

```bash
pi --model apid/gpt-5.6-sol:high
```

Pi 只把 `APID_API_KEY` 作为本地 Bearer 发给 apid。它不执行 OpenAI OAuth，也不读取 apid 的 Codex 凭据文件。

### 11.4 日常操作

正常情况下只需启动 apid 和 Pi。access token 到期由 apid 自动刷新；只有 refresh token 被撤销、账号登出、OAuth 契约变化或持久化故障时才重新执行：

```bash
apid-ctl auth codex login
# 然后重启 apid
```

## 12. 错误语义

| 场景 | HTTP/进程行为 | 对外 code |
|---|---|---|
| 本地 `client_api_key` 缺失或错误 | 401 | `invalid_api_key` |
| model 未匹配 route | 400 | `model_not_configured` |
| 启动时凭据缺失或损坏 | apid 启动失败 | 提示 `apid-ctl auth codex login` |
| Token endpoint 临时失败且 token 已过期 | 503 | `codex_auth_unavailable` |
| refresh token 撤销/第二次 401 | 503 | `codex_reauthentication_required` |
| Codex 返回 403 | 403 | `codex_access_denied` |
| Codex 返回 429 | 429 | 保留受限的上游错误语义 |
| 上游 3xx | 502 | `upstream_redirect_blocked` |
| 上游网络失败 | 502 | `upstream_unavailable` |
| SSE 输出后断流 | 已提交的流结束 | 日志/统计记录 `stream_interrupted` |

所有托管错误使用 OpenAI 风格 JSON envelope；不得把 token endpoint 原始 body、私有响应头或完整上游错误写给客户端。

## 13. 安全与可观测性

### 13.1 安全边界

- loopback 和强制本地 key 同时启用，不把 bearer subscription 包装成无认证本地 API。
- 固定 origin/path、禁止 redirect、禁用代理环境变量，防止 SSRF 和凭据二次跳转。
- OAuth callback 仅监听 `127.0.0.1`，校验 state 和 PKCE，不接受远程 callback。
- 凭据文件不复用 `~/.codex/auth.json`，避免 native Codex 与 apid 并发轮换同一个 refresh token。
- `apid-ctl status --json` 同样不包含 access token、refresh token 或 account ID。
- 运行中不提供导出凭据的 API。

### 13.2 日志与指标

允许记录：

- `codex_token_refresh` 的结果类别和耗时。
- 是否进入提前刷新、401 强刷、退避或重新登录状态。
- 上游状态码、受控 request ID、模型、usage 和 TTFT。
- 凭据文件路径和非敏感错误类别。

禁止记录：

- access/refresh token、Authorization、完整 JWT 或 token 指纹。
- account ID、email 和 callback authorization code。
- token endpoint 原始请求/响应。
- 托管请求的完整 body、query 或私有错误 body。

建议增加低基数指标或统计字段：

```text
codex_auth_refresh_total{result=success|temporary_error|invalid_grant|persist_error}
codex_auth_refresh_duration_ms
codex_auth_forced_refresh_total
codex_auth_state{state=ready|reauth_required|persist_pending}
```

如果暂不引入 Prometheus，这些先以结构化日志和现有请求错误分类实现，避免为单一功能增加新的监控体系。

## 14. 测试计划

### 14.1 `codexauth` 单元测试

- PKCE verifier/challenge、state 长度和 callback 校验。
- OAuth 错误、超时、取消和异常 JSON。
- 默认路径、override、目录/文件权限、原子替换和损坏文件。
- access token 到期判断和 5 分钟刷新窗口。
- 100 个并发调用只执行一次 refresh。
- refresh token 返回新值、缺省值和空值的轮换处理。
- token 成功但持久化失败时进入 `persist_pending`，不复用旧 refresh token。
- 临时错误使用未过期旧 token，过期后返回错误。
- `invalid_grant` 标记 reauth，退避期内不重复请求。
- 等待者取消不影响其他等待者。

并发和时间测试使用注入 clock、channel 和显式同步，不依赖真实 `time.Sleep`。

### 14.2 配置测试

- `codex_oauth` 的协议、固定 URL/path、空 `api_key` 和 model 不改写约束。
- 缺少 `client_api_key`、非 loopback、凭据文件缺失时加载失败。
- inference route 可以混用 exact managed rule 与普通 upstream。
- 旧 `codex_subscription` 继续要求唯一 catch-all 且不能混用。
- compact 继续要求唯一 managed/subscription target。
- `/v1/models` 只收集精确模型，去重并稳定排序。

### 14.3 Upstream 测试

- 客户端认证头不会到达 Codex。
- Authorization 和 account header 只来自 Manager。
- 固定 target、query 策略、redirect 禁止和 proxy 禁用。
- 标准 Pi Responses body 的字段归一化 golden test。
- 文本、图片、工具调用、reasoning 和未知字段保留。
- 已确认不兼容字段被删除。
- stream 强制和非流式聚合输入保持配对完整。

### 14.4 Server 集成测试

- 普通 route 与 managed exact model 在同一路径正确分派。
- 托管 target 禁止 TRACE，普通 target 不受影响。
- 新鲜 token 快路径不调用 token endpoint。
- 首个 401 刷新并重放一次，第二个 401 停止。
- 并发旧 token 401 不会使刚刷新的 token 再次失效。
- SSE 开始后断流不重试。
- 403/429/5xx 不触发强制刷新。
- `/v1/models` 鉴权、结构、排序和模型来源。
- 原有 `codex_subscription` header、错误和 raw payload 回归测试全部保留。

### 14.5 CLI 与人工验收

- `apid-ctl` 老参数兼容 `sessions`。
- login/status/status --json/logout 的输出不泄漏凭据。
- 浏览器与 `--no-browser` 两条登录路径。
- apid 重启后能使用轮换后的凭据继续请求。
- Pi 完成文本回复、工具调用、reasoning、图片输入、长 SSE 和一次非流式请求。
- 记录 Pi、Codex OAuth 契约和私有 backend 验收时的精确版本。

当前本机 Pi 验收基线为 `0.83.0`；发布实现时应重新记录实际版本，而不是永久沿用本文数字。

## 15. 预计改动

| 区域 | 生产代码 | 测试代码 | 说明 |
|---|---:|---:|---|
| `codexauth` | 550～750 行 | 500～700 行 | OAuth、Store、Manager、JWT 最小解析 |
| `cmd/apid-ctl` | 150～250 行 | 150～250 行 | 子命令分发和 auth 命令 |
| `config` / `main` wiring | 120～200 行 | 150～250 行 | 模式、校验、Manager 注入 |
| `upstream` / Codex 归一化 | 150～300 行 | 200～350 行 | 私有后端兼容与 header |
| `server` / retry / models | 180～300 行 | 250～400 行 | 目标级策略、401、非流式、目录 |
| README / 示例配置 | 100～200 行 | — | 用户流程与风险说明 |

预计涉及 15～22 个文件，生产代码约 1150～1800 行、测试约 1250～1950 行。最终规模取决于 Pi 非流式 SSE 聚合能否复用现有 Responses 事件累加器，以及当前协议结构是否足以表达全部兼容字段。

## 16. 分阶段交付

### Phase A：凭据控制面

- `codexauth.Store`、OAuth login 和 `apid-ctl auth codex login/status/logout`。
- 文件权限、原子落盘、PKCE/state 和 fake token endpoint 测试。

完成标准：可以独立登录、重启后读取、查看状态和安全注销，但尚不接入请求。

### Phase B：运行时认证

- `AuthModeCodexOAuth`、配置校验和 Manager wiring。
- 按需刷新、singleflight、轮换持久化、退避和 401 一次重放。
- 保留旧 `codex_subscription` 行为。

完成标准：受控测试 upstream 可以验证新鲜、过期、轮换、401 和并发路径。

### Phase C：Pi 兼容

- Codex 请求归一化、SSE/非流式响应处理和 `/v1/models`。
- Pi models.json 示例和端到端验收。

完成标准：Pi 能完成文本、工具调用、reasoning、长流和上下文继续请求。

### Phase D：加固

- 敏感日志审计、故障注入、回归矩阵、README 和发布说明。
- 记录精确 Pi/Codex/backend 兼容性基线。

## 17. 验收标准

1. `apid-ctl auth codex login` 能生成权限正确、可重启读取的独立凭据文件。
2. `status`、日志、TRACE、SQLite 和错误响应中搜索不到任何 token 或 account ID。
3. token 距过期大于 5 分钟时，请求不访问 token endpoint。
4. 100 个并发请求进入刷新窗口时，只发生一次 refresh。
5. refresh token 轮换后重启 apid 仍能继续刷新。
6. token endpoint 临时失败时，未过期 token 继续工作；过期 token 返回受控 503。
7. 上游 401 只重放一次，SSE 提交后不重放，403/429 不刷新。
8. Pi 仅配置本地 key 即可通过 `openai-responses` 完成工具调用和 reasoning。
9. `/v1/models` 只暴露配置中的精确模型，不访问私有远端目录。
10. 普通 upstream、Responses → Chat 和旧 `codex_subscription` 全部回归通过。
11. `go test ./...`、`go vet ./...` 和前端现有构建不回归。

## 18. 风险与后续演进

### 18.1 私有后端兼容风险

`backend-api/codex` 的 payload 和 header 契约可能随 Codex 版本变化。归一化规则必须集中、带 golden test，并在升级参考实现或客户端后执行人工验收。不能把观察到的模型 ID、上下文窗口或字段删除列表描述为 OpenAI 公共 API 契约。

### 18.2 多进程与多账号

首版明确采用“一个凭据文件、一个活动 apid 进程、一个账号”。如果未来需要多实例或账号池，必须先设计跨进程锁、账号选择、refresh token 所有权、额度隔离和凭据撤销，不能简单把 Manager 放进 map。

### 18.3 控制面热加载

首版 login/logout 后重启 apid。未来若确有无中断切换需求，优先考虑本地 Unix socket 或显式 SIGHUP reload；不应直接开放 bearer 凭据管理 HTTP endpoint。

### 18.4 Pi 自带 OAuth

当前 Pi 已支持 provider OAuth 和 `openai-codex` 相关认证能力。直接使用 Pi OAuth 的链路更短；使用 apid 托管的价值在于统一多客户端入口、模型路由、统计和兼容策略。两者不应共享同一个 refresh token 文件。

## 19. 参考

- [现有 Codex 凭据透传设计](./CODEX_SUBSCRIPTION_PROXY_DESIGN.md)
- [Pi custom provider 文档](https://pi.dev/docs/latest/custom-provider)
- [Pi models 文档](https://pi.dev/docs/latest/models)
- [Pi usage 文档](https://pi.dev/docs/latest/usage)
- [CLIProxyAPI Codex OAuth 参考实现](https://github.com/router-for-me/CLIProxyAPI/tree/a88197f845c979132c8978ea223c6af05cc81536/internal/auth/codex)
- [CLIProxyAPI Codex auth 入口](https://github.com/router-for-me/CLIProxyAPI/blob/a88197f845c979132c8978ea223c6af05cc81536/sdk/auth/codex.go)
