# apid

本地 LLM API 协议转换服务，无 GUI，目前仅支持 Chat Completions 转 Response ，后续计划支持更多协议的转换，Coding Agent 配置托管、Token 使用量统计分析等，类似 cc-switch 的无 GUI 版本纯后端进程版本。

```
本地应用 ⇄ Responses ⇄ apid ⇄ Chat Completions ⇄ 远程上游
```

已支持文本、工具调用（function calling）和 reasoning。暂不支持图片/文件输入、
内置工具（web_search 等）、annotations、多模态输出。

## 运行

```bash
export APID_UPSTREAM_BASE_URL="https://api.deepseek.com"   # 上游基础地址
export APID_UPSTREAM_API_KEY="sk-..."                        # 上游 Key，留空则透传客户端 Authorization
export APID_UPSTREAM_MODEL="deepseek-v4-flash"                    # 覆盖转发的模型名，留空则透传客户端 model
export APID_LISTEN=":19092"                                    # 监听地址，默认 :8080

go run .
```

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
internal/config    环境变量配置
internal/types     Responses / Chat 两套数据结构
internal/convert   请求、响应、流式三类转换
internal/upstream  转发上游的 HTTP 客户端
internal/server    HTTP 服务与路由
```

字段映射的细节见 `CLAUDE.md` 和各 `convert/*.go`。

## 测试

```bash
go test ./...
```
