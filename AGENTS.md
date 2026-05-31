# CALUDE.md/AGENTS.md

## 项目目标

`apid` 是一个常驻的 LLM API **协议转换**服务。它对外暴露 OpenAI **Responses API**
(`POST /v1/responses`)，内部把请求转换成 **Chat Completions** 格式转发给远程上游，
再把上游响应转换回 Responses 格式。用途：让只会 Chat Completions 的远程服务，以
Responses API 的形式被本地应用调用。

数据流：`本地应用 ⇄ Responses 协议 ⇄ apid ⇄ Chat Completions 协议 ⇄ 远程上游`

## 常用命令

```bash
go build ./...                                   # 编译
go vet ./...                                     # 静态检查
go test ./...                                    # 全部测试
go test ./internal/convert -run TestStreamTools  # 跑单个测试(按名匹配)
go test ./... -v                                 # 详细输出
go run .                                          # 启动服务
```

运行时配置（环境变量，见 `internal/config`）。启动时先加载工作目录下的 `.env`
（可用 `APID_ENV_FILE` 指定其他路径），真实环境变量优先、不被 `.env` 覆盖：
- `APID_LISTEN`（默认 `:8080`）
- `APID_UPSTREAM_BASE_URL`（默认 `https://api.openai.com/v1`，会拼接 `/chat/completions`）
- `APID_UPSTREAM_API_KEY`（留空则透传客户端请求里的 `Authorization` 头）
- `APID_UPSTREAM_MODEL`（非空则覆盖转发给上游的 `model`，留空则透传客户端的 `model`）
- `APID_TRACE_DIR` / `APID_TRACE`：开启 TRACE 落盘。显式指定 `APID_TRACE_DIR` 优先；
  否则 `APID_TRACE` 为真（`1`/`true`/`yes`/`on`）时落到 `./logs`。默认关闭、零开销。

## 架构

转换是核心，分三层、各司其职：

- **`internal/types`** — 两套数据结构并存：`responses.go`(对外) 与 `chat.go`(对上游)。
  联合类型字段(`input` / `tool_choice` / `content` / `output`)统一用 `json.RawMessage`
  延迟解析，因为同一字段在协议里既可能是字符串也可能是数组/对象。
- **`internal/convert`** — 三个转换方向，互相独立：
  - `request.go` `ResponsesToChat`：Responses 请求 → Chat 请求（含 tools、reasoning、
    多轮工具对话的 input 项映射）。
  - `response.go` `ChatToResponses`：非流式 Chat 响应 → Responses 响应。
  - `stream.go` `StreamChatToResponses`：流式 SSE 转换，是**有状态累加器**(`streamState`)，
    见下。
- **`internal/upstream`** — `Client.ChatCompletions` 仅负责转发 HTTP 请求，不做任何转换。
- **`internal/trace`** — 可选的 TRACE 落盘。`Tracer.Begin` 为一次请求分配共享的
  时间戳+序号前缀，`Entry.Dump(kind, body)` 把不同形态（`responses` 原始 / `chat` 转换后）
  各存成一个 JSON 文件，便于配对离线 DEBUG。禁用时所有方法可安全空调用、零开销。
  仓库根的 `trace-viewer.html` 是配套的离线查看页面。
- **`internal/server`** — `handleResponses` 编排全流程：解析 → 转换请求 → 转发 →
  按 `stream` 走流式或非流式响应转换。途中按 `kind` 落 trace。上游非 2xx 时把错误体原样回传。
- **`main.go`** — 入口 + 优雅退出。

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
