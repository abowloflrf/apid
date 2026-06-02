# apid: Local LLM API proxy & protocol converter

本地 LLM API 网关，无 GUI。按暴露路径配置多条转发路由：两端协议不同则做协议转换
（目前支持 Responses → Chat Completions），两端协议相同则纯转发（不改请求、仅统计
token 等指标）。后续计划支持更多协议转换、Coding Agent 配置托管、Token 使用量分析等，
类似 cc-switch 的无 GUI 纯后端进程版本。

```
本地应用 ⇄ Responses ⇄ apid ⇄ Chat Completions ⇄ 远程上游   (协议转换)
本地应用 ⇄ Chat      ⇄ apid ⇄ Chat               ⇄ 远程上游   (纯转发 + 统计)
```

转换路由已支持文本、工具调用（function calling）和 reasoning。暂不支持图片/文件输入、
内置工具（web_search 等）、annotations、多模态输出。

## 运行

转发路由配置在 TOML 文件里（默认 `config.toml`，可用命令行 flag `--config` 指定路径）。
先复制示例并按需修改：

```bash
cp config.example.toml config.toml   # 编辑里面的上游地址 / 协议 / Key / 模型

export APID_LISTEN=":19092"        # 监听地址，默认 :8080（运维参数仍走环境变量）
go run . --config config.toml      # --config 省略时默认 config.toml
```

配置分两张表：`[[upstream]]` 定义后端（协议 / 地址 / Key / 模型），定义一次按 `name` 复用；
`[[route]]` 是对外入口（`path` + `input_protocol`），按请求里的 `model` 匹配（精确 > glob >
兜底）引用某个 upstream。入口协议与所选 upstream 协议相等时纯转发，否则协议转换。
详见 `config.example.toml`。

## 调用

```bash
curl http://localhost:19092/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-4o-mini","instructions":"你是一个简洁的助手","input":"用一句话介绍你自己"}'
```

加 `"stream": true` 走 SSE 流式。`input` 可以是字符串，也可以是消息数组：

```json
{"model":"gpt-4o-mini","input":[{"role":"user","content":[{"type":"input_text","text":"你好"}]}]}
```

工具调用：

```bash
curl http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o-mini",
    "input": "北京今天天气怎么样？",
    "tools": [{
      "type": "function",
      "name": "get_weather",
      "description": "查询某城市天气",
      "parameters": {"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}
    }]
  }'
```

## 结构

```
main.go            入口、优雅退出
internal/config    运维环境变量 + TOML 路由配置
internal/types     Responses / Chat 两套数据结构
internal/convert   请求、响应、流式三类转换
internal/upstream  转发上游的 HTTP 客户端（每路由一个）
internal/server    多路由分派：协议转换 / 纯转发 + 指标采集
internal/store     通用 SQLite 存储
internal/stats     请求指标异步落盘
```

字段映射的细节见 `CLAUDE.md` 和各 `convert/*.go`。

## 测试

```bash
go test ./...
```
