# CALUDE.md/AGENTS.md

## 项目目标

`apid` 是一个常驻的 LLM API **网关**。配置分两张表：**`[[upstream]]`** 定义后端
（协议 / 地址 / 鉴权 / 模型），定义一次按 `name` 复用；**`[[route]]`** 是对外入口
（暴露 `path` + 客户端 `input_protocol`），按请求里的 `model` 匹配（精确 > glob > 兜底）
引用某个 upstream。一个 upstream 可被多个入口（不同 path / 不同客户端协议）复用，
上游信息只写一份。是否做协议转换由「入口 `input_protocol` 与所选 upstream `protocol`
是否相等」决定：

- **协议转换**：两端协议不同时做转换。当前支持对外 **Responses API** → 上游
  **Chat Completions**：把请求转成 Chat 格式转发，再把响应转回 Responses 格式。
  用途：让只会 Chat Completions 的远程服务以 Responses API 形式被本地应用调用。
- **纯转发**：两端协议相同时不做任何协议转换，原样把请求字节转发给上游，仅
  sniff/解析以采集 model/stream/token/TTFT 等指标（upstream 配了 `model` 才解析改写后再转发）。

数据流：
- 转换：`本地应用 ⇄ Responses ⇄ apid ⇄ Chat Completions ⇄ 远程上游`
- 纯转发：`本地应用 ⇄ Chat ⇄ apid（仅统计）⇄ Chat ⇄ 远程上游`

枚举协议：`openai_responses`、`openai_chat_completions`。当前转换仅实现
`responses → chat`；反向 `chat → responses` 在配置加载时报错拒绝。

## 常用命令

```bash
go build ./...                                   # 编译
go vet ./...                                     # 静态检查
go test ./...                                    # 全部测试
go test ./internal/convert -run TestStreamTools  # 跑单个测试(按名匹配)
go test ./... -v                                 # 详细输出
go run .                                          # 启动服务
```

运行时配置分两部分：**转发配置走 TOML 文件**，**运维参数走环境变量**。

**转发配置（TOML）**：路径由命令行 flag `--config` 指定（默认 `config.toml`），是必需
配置，缺失或非法即启动失败。见 `config.example.toml`。两张表：

`[[upstream]]`（后端，定义一次按 `name` 复用）字段：
- `name`：引用键，必须唯一、非空。
- `protocol`：该后端实际说的协议（枚举两值）。
- `base_url` + `path`：上游实际地址（显式配 path，不按协议推导）。
- `api_key`：留空则透传客户端 `Authorization` 头。
- `model`：该后端的默认 model 改写，非空则覆盖转发的 `model`（纯转发也生效，会解析
  改写 body）；留空透传。可被引用它的 `[[route.model]]` 规则按需覆盖（见下）。

`[[route]]`（入口）字段：
- `path`：对外暴露路径，也是路由键，必须唯一、以 `/` 开头。
- `input_protocol`：客户端在这个入口说的协议（枚举两值）。
- `[[route.model]]`：至少一条按 model 的分派规则，`match`（精确名 / glob `claude-*` /
  空或 `*` 兜底，兜底至多一条）+ `upstream`（引用 `Upstream.name`）+ 可选 `model`
  （本规则的 model 改写，**三态指针**：键省略 = 继承 upstream `model`；`""` = 强制透传
  客户端 model；非空 = 改写成该值）。生效优先级：规则 `model` > upstream `model` > 透传。
  这样同一 upstream 被多条规则复用时，每条规则可各自决定透传 / 改写。匹配优先级
  精确 > glob > 兜底。是否转换 = `input_protocol` 与所选 upstream `protocol` 是否相等。

**运维参数（环境变量，见 `internal/config`）**。启动时先加载工作目录下的 `.env`
（可用 `APID_ENV_FILE` 指定其他路径），真实环境变量优先、不被 `.env` 覆盖：
- `APID_LISTEN`（默认 `:8080`）
- `APID_TRACE_DIR` / `APID_TRACE`：开启 TRACE 落盘。显式指定 `APID_TRACE_DIR` 优先；
  否则 `APID_TRACE` 为真（`1`/`true`/`yes`/`on`）时落到 `./logs`。默认关闭、零开销。
- `APID_DB`：项目通用 SQLite 数据库文件路径（空 = 不启用）。开启后 stats 会在
  每次请求结束时把指标异步写入 `requests` 表，可直接用 `sqlite3` CLI 查询。
  默认关闭、零开销。

## 架构

按路由分派是入口、协议转换是核心，分层、各司其职：

- **`internal/config`** — 运维参数从环境变量读，转发配置从 TOML 文件读（路径由 `main.go`
  的 `--config` flag 传入）。两个结构：`Upstream`（name/protocol/base+path+key+model）与
  `Route`（path/input_protocol/`[]ModelRule{match, upstream, model}`，`model` 是 `*string`
  三态：nil 继承 upstream / `""` 透传 / 非空改写）。`Load(configPath) (Config, error)`：`loadFile` 用 `BurntSushi/toml`
  解析、`MetaData.Undecoded()` 拒未知键、`validateConfig` 校验（upstream name 唯一、协议
  枚举合法、地址完整；route path 唯一且以 `/` 开头、引用的 upstream 存在、精确 match 不重复、
  兜底至多一条、拒绝 `chat→responses`）。`Route.Resolve(model)` 按「精确 > glob > 兜底」
  选中命中的 `ModelRule`，`UpstreamFor` 是其取 `upstream` 字段的薄封装；glob 用不把 `/`
  当分隔符的 `globMatch`。新增协议 / 字段都改这一处。
- **`internal/types`** — 两套数据结构并存：`responses.go`(对外) 与 `chat.go`(对上游)。
  联合类型字段(`input` / `tool_choice` / `content` / `output`)统一用 `json.RawMessage`
  延迟解析，因为同一字段在协议里既可能是字符串也可能是数组/对象。
- **`internal/convert`** — 三个转换方向，互相独立：
  - `request.go` `ResponsesToChat`：Responses 请求 → Chat 请求（含 tools、reasoning、
    多轮工具对话的 input 项映射）。
  - `response.go` `ChatToResponses`：非流式 Chat 响应 → Responses 响应。
  - `stream.go` `StreamChatToResponses`：流式 SSE 转换，是**有状态累加器**(`streamState`)，
    见下。
- **`internal/upstream`** — 每个 `[[upstream]]` 一个 `Client`，绑定其 `baseURL + path + apiKey`，
  按 name 被多个 route/model 规则共享，不做任何转换。`Forward(ctx, body, authOverride)`
  转发任意原始字节（纯转发用）；
  `ChatCompletions` 是它的薄封装：marshal 转换后的 `ChatRequest` 再 `Forward`（转换路由用）。
  鉴权优先用配置 `apiKey`，否则透传 `authOverride`。`Endpoint()` 返回完整转发 URL，
  供 stats 记录「实际转发 URL」而不发起请求。
- **`internal/trace`** — 可选的 TRACE 落盘。`Tracer.Begin` 为一次请求分配共享的
  时间戳+序号前缀，`Entry.Dump(kind, body)` 把不同形态（按输入协议命名的原始请求 /
  `chat` 转换后 / `upstream` 纯转发改写后）各存成一个 JSON 文件，便于配对离线 DEBUG。
  禁用时所有方法可安全空调用、零开销。仓库根的 `trace-viewer.html` 是配套的离线查看页面。
- **`internal/store`** — 项目通用 SQLite 存储层。`Open(path)` 打开数据库文件并跑
  schema 迁移（`PRAGMA journal_mode=WAL` + `synchronous=NORMAL` + 集中维护的建表
  SQL），`Store.DB()` 暴露原生 `*sql.DB` 给业务包使用，`Close()` 关闭连接。`path`
  为空时返回 `(nil, nil)` 表示不启用；schema 集中维护意味着加新表（如配置、审计）
  都改 `store.go` 一处。
- **`internal/stats`** — 在 `store` 之上提供请求指标收集：`Record` 结构 + `Recorder`
  异步落盘。`Recorder` 内部走有界 channel（默认 1024）+ 单 worker goroutine，
  攒到 64 条或 500ms 就批量 INSERT 到 `requests` 表，热路径只一次 `select { case ch <- r: default: drop }`，
  永不阻塞。所有方法对 nil 接收者安全；`Close()` 排空 channel 后退出 worker。
  表 schema：`time / duration_ms / ttft_ms / client_protocol / client_path / client_model /
  upstream_protocol / upstream_url / upstream_model / stream / client_status /
  upstream_status / input_tokens / output_tokens / total_tokens / cached_tokens / error`。
  `client_protocol`/`upstream_protocol` 是 `Record` 字段（按入口 `input_protocol` + 所选
  upstream `protocol` 填，不再写死）。
  `ttft_ms`（首 token 耗时）仅流式且收到内容增量时有值，非流式 / 失败时为 NULL。
- **`internal/server`** — 两级分派（path → model）。`New` 为每个 upstream 建一个
  `upstream.Client` 包成 `target`（含 upstream 配置）存进 `map[name]*target`，route 存进
  `map[path]*route`；`Handler` 把每条 route 注册到其暴露 path。`handleRoute` 是共用编排
  骨架：读 body → sniff model → `resolve`（`route.Resolve` 选中 `ModelRule`，再查 `target`，
  无命中回 400）→ `effectiveModel(rule, upstream)` 算出生效转发 model（规则 `model` >
  upstream `model` > 透传）→ 填 trace/`stats.Record`（defer 一次性上报），再按「`input_protocol`
  与所选 `target.cfg.Protocol` 是否相等」分派（两条路径都收 `effModel` 参数）：
  - 转换 `convertResponsesToChat`（`server.go`）：解析 Responses → 转 Chat（按 `effModel`
    覆盖）→ 转发 → 按 `stream` 走 `streamResponse`/`jsonResponse`（复用 `convert` 包）。
  - 纯转发 `forwardRaw`（`forward.go`）：`effModel` 非空则改写 model，`Forward` 原始字节；非流式
    `forwardJSON` 整体回传并按 upstream 协议 `extractUsage`；流式 `forwardStream` 调
    `forwardSSE`（`sseforward.go`）逐行原样透传 + tee 解析 usage/TTFT。
  两条路径都用 `passUpstreamError` 把上游非 2xx 错误体原样回传。
- **`main.go`** — 入口 + 优雅退出。`config.Load`（失败即退出）→ `store.Open` → `server.New`
  → 启动 HTTP，收到退出信号后 `httpServer.Shutdown` → `srv.Close`（排空 stats channel）
  → `st.Close`（关 SQLite）。所有 route/upstream 共享单 recorder/单 store，退出时序不变。

### 关键字段映射约定

请求方向：`instructions`→首条 system 消息；`max_output_tokens`→`max_tokens`；
`reasoning.effort`→`reasoning_effort`；Responses 扁平 tools→Chat 嵌套 `{function:{...}}`；
input 项 `function_call`→`assistant.tool_calls`，`function_call_output`→`role:"tool"` 消息，
其中 `call_id` ⇄ `id`/`tool_call_id`。消息角色经 `mapRole` 归一：`developer`→`system`
（`developer` 是 o1+ 模型对 `system` 的重命名、语义等价，而兼容上游通常只认
`system`/`assistant`/`user`/`tool`，原样透传会被拒），其余角色透传。

响应方向：Chat `message.content`→`output[]` 的 `message`/`output_text`；
`tool_calls[]`→`function_call` 项(`id`→`call_id`)；`reasoning_content`→`reasoning` 项(`summary`)。

注意：`reasoning_content` 是 DeepSeek / vLLM 等的事实标准字段，OpenAI 官方 Chat 响应里
**没有**这个字段——转换面向的是兼容上游，不是 OpenAI 官方端点。

### 流式转换的核心设计

`streamState` 边读上游 SSE 边发 Responses 事件。三类增量(文本 / `reasoning_content` /
`tool_calls`)各自**惰性开项**：首次出现某类增量时才分配 `output_index`、发
`response.output_item.added`，并对每个 item 配对 `.added`/`.done`，最终用 `response.completed`
带上完整 `output` 数组和 `usage`。工具调用参数按上游 `tool_calls[].index` 累积拼接。
新增/修改流式行为时务必保持 added→delta→done 的事件配对完整，否则客户端会解析失败。

## 字段覆盖范围

只做最关键字段。**已支持**：文本、工具调用(function calling)、reasoning。
**未覆盖**：图片/文件输入、内置工具(web_search 等)、annotations、多模态输出、
`previous_response_id` 多轮状态。扩展时优先改对应的 `convert/*.go` 并补
`internal/convert/convert_test.go` 用例。

## 参考资料

仓库根目录的 `OPENAI_REPONSE_API.md` 和 `OPENAI_CHAT_API.md` 是两套协议的完整字段文档
（很大，用 grep 按字段名定位，不要整篇读）。新增字段映射时以这两份为准。

设计协议转换时可参考 LiteLLM 的 Responses ⇄ Chat Completions 转换实现：
<https://github.com/BerriAI/litellm/tree/main/litellm/responses/litellm_completion_transformation>
（与本项目同方向：把 Responses API 请求转成 Chat Completions、再把响应转回，可借鉴其字段映射与边界处理。）
