# CLAUDE.md / AGENTS.md

## 项目目标

`apid` 是常驻的 LLM API **网关**。配置分两张表:**`[[upstream]]`** 定义后端
(协议/地址/鉴权/模型),按 `name` 复用;**`[[route]]`** 是对外入口(暴露 `path` +
客户端 `input_protocol`),按请求 `model` 匹配(精确 > glob > 兜底)引用某个 upstream。
一个 upstream 可被多个入口复用,信息只写一份。

是否做协议转换 = 「入口 `input_protocol` 与所选 upstream `protocol` 是否相等」:

- **协议转换**(两端协议不同):当前仅实现对外 **Responses** → 上游 **Chat Completions**
  (请求转 Chat 转发、响应转回 Responses)。反向 `chat → responses` 在配置加载时报错拒绝。
- **纯转发**(两端协议相同,chat→chat / responses→responses / anthropic→anthropic 均可):
  原样转发字节,仅 sniff/解析采集 model/stream/token/TTFT 指标(upstream 配了 `model`
  才改写 body)。
- **双协议透传**:`openai_chat_completions` upstream 配 `supports_responses = true`
  (可选 `responses_path`,留空由 `path` 推导:去掉尾部 `/chat/completions` 换成
  `/responses`)时,同一后端还原生支持 Responses;responses 入口命中它按
  `responses → responses` 纯转发到 responses 端点,不做转换。供应商两种协议都支持时
  不用写两份 upstream。

枚举协议:`openai_responses`、`openai_chat_completions`、`anthropic_messages`。

## 常用命令

```bash
pnpm --dir webui build                           # 前端构建（Vite+React，产物进 server/webui/dist）
pnpm --dir webui dev                             # 前端开发（Vite dev server，代理 /stats/* 到本地 :19092）
go build ./...                                   # 编译（依赖前端产物，未构建时 //go:embed web/ui/dist 编译失败，须先 pnpm --dir webui build）
go vet ./...                                     # 静态检查
go test ./...                                    # 全部测试
go test ./convert -run TestStreamTools  # 跑单个测试(按名匹配)
go run .                                          # 启动服务
```

## 开发约定

- 代码注释尽量精简,非必要不写;必要的注释、日志与报错信息统一用英文。
- Commit message 用英文,一句话简洁概括改动即可。

## 配置

**转发配置走 TOML**(`--config`,默认 `config.toml`,缺失/非法即启动失败),
**运维参数走环境变量**。完整字段见 `config.example.toml` 与 `config`
的 struct 注释,这里只记约定:

- `[[upstream]].model`:非空覆盖转发 model(纯转发也生效),留空透传。
- `[[upstream]].supports_responses`:布尔开关,仅对 `openai_chat_completions` 生效;
  打开后同一后端同时接受 Chat 与 Responses,responses 入口直接透传。`responses_path`
  显式指定 Responses 端点,留空则从 `path` 推导;单独出现或用于非 chat 协议会报错。
- `protocol = "anthropic_messages"` 当前只支持同协议纯转发;配置的 `api_key` 会作为
  `X-Api-Key` 发给上游。
- `[[route.model]]`:至少一条 `{match, upstream, model}` 规则。`match` 精确名 / glob
  `claude-*` / 空或 `*` 兜底(至多一条)。`model` 是**三态指针**:键省略 = 继承 upstream;
  `""` = 强制透传客户端 model;非空 = 改写。生效优先级:规则 `model` > upstream `model` > 透传。
- 启动先加载工作目录 `.env`(`APID_ENV_FILE` 可改路径),真实环境变量优先。
  关键变量:`APID_LISTEN`、`APID_TRACE_DIR`/`APID_TRACE`、`APID_DB`(均默认关闭、零开销)。

## 架构

分层、各司其职,逐包细节见各文件 doc comment:

- **`config`** — 两张表的加载/校验/`Resolve`(精确>glob>兜底)。**新增协议或配置字段改这里。**
- **`protocol`** — 多协议结构并存:`responses.go`、`chat.go`、`anthropic.go`;联合类型字段用 `json.RawMessage` 延迟解析。
- **`convert`** — Responses⇄Chat 三方向转换:`request.go`/`response.go`/`stream.go`(有状态累加器)。**扩展字段映射改这里 + 补 `convert_test.go`。**
- **`upstream`** — 每个 upstream 一个 `Client`(绑定 baseURL+path+apiKey),`Forward` 转发原始字节,`ChatCompletions` 是其薄封装。
- **`trace`** — 可选 TRACE 落盘,配对离线 DEBUG;配套 `trace-viewer.html`。禁用时零开销。
- **`store`** — 通用 SQLite 层(WAL),schema 集中维护,**加表改 `store.go` 一处**。
- **`stats`** — 在 store 之上异步落盘请求指标:有界 channel + 单 worker 批量 INSERT,热路径满即丢、永不阻塞,nil 安全。读侧 `query.go` 的 `QueryDailyUsage` 做按天×上游模型聚合,供 Grafana 端点用。
- **`server`** — path→model 两级分派。`handleRoute` 编排:读 body→sniff model→resolve→算生效 model→trace/stats,再按协议是否相等(含 `supports_responses` 双协议透传)分派到 `convertResponsesToChat` 或 `forwardRaw`(`forward.go`/`sseforward.go`)。两条路径都用 `passUpstreamError` 原样回传上游非 2xx。另有只读指标端点 `GET /stats/daily`(`stats.go`),返回 Grafana Infinity 数据源可直接消费的扁平 JSON。另有 `GET /stats/topology`(`topology.go`):把已加载的 route/upstream 配置输出成图(入口→规则→上游,含 forward/convert、生效 model、鉴权方式),脱敏(api_key 不出现在响应里)、不依赖 `APID_DB`,webui 的 routes 标签页消费它。
- **`server`** — 另有 `GET /stats/active`(`live.go`):内存注册表输出所有在途请求(模型/协议/流式/实时 token 计数),流式期间 output token 优先取协议中途 usage(Anthropic),否则按增量文本估算(`*_est` 标记),同样不依赖 `APID_DB`,webui 的 live 标签页 1s 轮询、差分算 tokens/s。
- **`webui`** - Vite + React + TS 前端:`src/components` 通用组件、`src/views` 页面视图、`src/useStatsQuery.ts` 统一查询状态(范围/筛选/分页),构建产物进 `server/webui/dist`,由 `server/stats.go` 的 `//go:embed webui/dist` 嵌入(APID_DB 未开时 `/stats/` 返回 503)。`dist/` 整个不入库,必须先 `pnpm --dir webui build` 再 `go build`,否则 `//go:embed` 因目录缺失而编译失败。
- **`main.go`** — 入口 + 优雅退出(Shutdown → srv.Close 排空 stats → store.Close)。

## 关键约定与不变量

- **字段映射**细节见 `convert/*.go` 注释。要点:`developer` 角色经 `mapRole` 归一为 `system`;
  `reasoning_content` 是 DeepSeek/vLLM 的事实标准字段(OpenAI 官方 Chat **无**此字段),
  本项目面向兼容上游而非官方端点。
- **流式不变量**:`streamState` 三类增量(文本/reasoning/tool_calls)各自惰性开项,
  务必保持 **added → delta → done 事件配对完整**,否则客户端解析失败。

## 字段覆盖范围

只做最关键字段。**已支持**:文本、工具调用、reasoning。**未覆盖**:图片/文件输入、
内置工具、annotations、多模态输出、`previous_response_id` 多轮状态。

## 参考资料

- `OPENAI_REPONSE_API.md` / `OPENAI_CHAT_API.md`:两套协议完整字段文档(很大,用 grep 按字段名定位,勿整篇读)。
- LiteLLM 同方向转换实现可参考:<https://github.com/BerriAI/litellm/tree/main/litellm/responses/litellm_completion_transformation>
