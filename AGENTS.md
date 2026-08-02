# CLAUDE.md / AGENTS.md

## 项目目标

`apid` 是常驻的 LLM API **网关**。配置两张表:**`[[upstream]]`**(后端:协议/地址/鉴权/模型,按 `name` 复用)、**`[[route]]`**(对外入口:`path` + 客户端 `input_protocol`,按请求 `model` 精确 > glob > 兜底匹配 upstream)。一个 upstream 可被多个入口复用。

是否做协议转换 = 入口 `input_protocol` 与所选 upstream `protocol` 是否相等:

- **两端不同**:仅支持 Responses → Chat Completions 转换;反向 `chat → responses` 配置加载时报错。
- **两端相同**:原样转发字节,仅采集 model/stream/token/TTFT 指标(upstream 配 `model` 才改写 body)。
- **双协议透传**:chat upstream 配 `supports_responses = true` 时,同一后端也接受 responses 入口,直接透传(`responses_path` 留空由 `path` 推导),供应商两种协议都支持时不用写两份 upstream。

协议枚举:`openai_responses`、`openai_chat_completions`、`anthropic_messages`。

## 常用命令

```bash
pnpm --dir webui build    # 构建前端(产物进 server/webui/dist)
pnpm --dir webui dev      # 前端开发(vite dev server)
go build ./...            # 编译(未构建前端也能编译,仅 /stats/ 页面 503)
go vet ./...              # 静态检查
go test ./...             # 全部测试;单个:go test ./convert -run TestStreamTools
go run .                  # 启动
```

## 开发约定

- 代码注释精简、非必要不写;注释/日志/报错统一英文;commit message 英文一句话。

## 配置

转发配置走 TOML(`--config`,默认 `config.toml`,缺失/非法即启动失败),运维参数走环境变量。完整字段见 `config.example.toml` 与 config struct 注释,这里只记约定:

- `[[upstream]].model`:非空覆盖转发 model(纯转发也生效),留空透传。
- `[[upstream]].supports_responses`:仅对 chat 协议生效,打开后 responses 入口直接透传;`responses_path` 单独出现或用于非 chat 协议会报错。
- `protocol = "anthropic_messages"` 只支持同协议纯转发;`api_key` 作为 `X-Api-Key` 发给上游。
- `[[route.model]]`:至少一条 `{match, upstream, model}` 规则。`match` 精确名 / glob(`claude-*`) / 空或 `*` 兜底(至多一条)。`model` 三态:键省略 = 继承 upstream;`""` = 强制透传;非空 = 改写。优先级:规则 `model` > upstream `model` > 透传。
- 启动加载工作目录 `.env`(`APID_ENV_FILE` 可改路径),真实环境变量优先。关键变量 `APID_LISTEN`、`APID_TRACE_DIR`/`APID_TRACE`、`APID_DB`,均默认关闭、零开销。

## 架构

分层职责,细节见各包 doc comment:

- **`config`** — 加载/校验/`Resolve`(精确>glob>兜底)。**新增协议或配置字段改这里。**
- **`protocol`** — 多协议结构并存(`responses.go`/`chat.go`/`anthropic.go`),联合类型字段用 `json.RawMessage` 延迟解析。
- **`convert`** — Responses⇄Chat 转换(request/response/stream 三方向,有状态累加器)。**扩展字段映射改这里 + 补 `convert_test.go`。**
- **`upstream`** — 每 upstream 一个 `Client`(baseURL+path+apiKey),`Forward` 转发原始字节。
- **`trace`** — 可选 TRACE 落盘 + 离线 DEBUG,配套 `trace-viewer.html`;禁用时零开销。
- **`store`** — SQLite 层(WAL),schema 集中维护,**加表改 `store.go` 一处**。
- **`stats`** — store 之上异步落盘指标:有界 channel + 单 worker 批量 INSERT,满即丢、永不阻塞,nil 安全。
- **`server`** — path→model 两级分派:`handleRoute` 编排 sniff→resolve→trace/stats→按协议相等分派 `convertResponsesToChat` 或 `forwardRaw`;上游非 2xx 经 `passUpstreamError` 原样回传。另有 `/stats/*` 只读端点:`daily`(Grafana Infinity 扁平 JSON)、`topology`(配置拓扑图,脱敏、不依赖 APID_DB)、`active`(在途请求实时 token,估算值带 `*_est` 标记)。
- **`webui`** — Vite+React 前端,构建产物进 `server/webui/dist` 由 `//go:embed` 嵌入;`dist/` 不入库,仅保留 `placeholder.txt` 占位使未构建时也能编译(`webuiBuilt` 检测 `index.html`),未构建/未开 APID_DB 时 `/stats/` 返回 503。
- **`main.go`** — 入口 + 优雅退出(Shutdown → srv.Close 排空 stats → store.Close)。

## 关键约定与不变量

- **字段映射**细节见 `convert/*.go` 注释。要点:`developer` 经 `mapRole` 归一为 `system`;`reasoning_content` 是 DeepSeek/vLLM 事实标准字段(OpenAI 官方 Chat 无此字段),面向兼容上游。
- **流式不变量**:`streamState` 三类增量(文本/reasoning/tool_calls)各自惰性开项,保持 **added → delta → done 配对完整**,否则客户端解析失败。
- **字段覆盖范围**:已支持文本、工具调用、reasoning;未覆盖图片/文件输入、内置工具、annotations、多模态、`previous_response_id` 多轮状态。

## 参考资料

- `OPENAI_REPONSE_API.md` / `OPENAI_CHAT_API.md`:两套协议完整字段文档(很大,用 grep 按字段名定位,勿整篇读)。
- LiteLLM 同方向转换实现可参考:<https://github.com/BerriAI/litellm/tree/main/litellm/responses/litellm_completion_transformation>
