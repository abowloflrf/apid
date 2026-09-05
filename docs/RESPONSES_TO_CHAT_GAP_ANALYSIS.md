# apid：OpenAI Responses → Chat Completions 协议转换差距分析与修复优先级

> 文档状态：代码审查分析稿  
> 分析对象：apid、cc-switch、CLIProxyAPI  
> 目标读者：apid 项目维护者与协议转换实现者  
> 分析基线：apid `3598cff`（2026-09-02）、cc-switch `e47b5f`（2026-09-04）、CLIProxyAPI `2a6b87`（2026-09-03）

## 执行摘要

apid 当前已经具备一套结构清晰、基础行为可靠的 Responses → Chat Completions 转换器：文本消息、普通 function tool、并行工具调用、reasoning、非流式响应以及基本流式事件生命周期均已有实现。它适合连接遵循常见 OpenAI Chat 形态、只使用文本和 function tool 的后端。

但当前实现仍属于“基础 function-tool 子集”，不能覆盖 Codex 实际产生的完整请求。对本地一份真实 Codex trace 的结构统计显示，请求包含：

| 项目 | 数量 |
|---|---:|
| `function_call` | 70 |
| `function_call_output` | 70 |
| `custom_tool_call` | 21 |
| `custom_tool_call_output` | 21 |
| `tool_search` 工具定义 | 1 |
| `web_search` 工具定义 | 1 |
| `namespace` 工具定义 | 1 |
| `reasoning` item | 47 |

这 47 个 reasoning item 的 `summary` 和 `encrypted_content` 均为空。这意味着 custom tool、动态工具和 reasoning 回放问题已经可以由真实请求触发，并非纯粹的协议完整性问题。

总体判断：

- cc-switch 覆盖最完整，包含较成熟的供应商兼容、历史恢复、custom/tool-search、残缺工具流修复和 SSE 解析。
- CLIProxyAPI 居中，custom tool、namespace、`additional_tools` 和部分多模态覆盖强于 apid。
- apid 的优势是代码小、边界清楚、流式结束检查严格，适合通过显式 upstream capability 和统一工具上下文逐步完善。

| 能力 | apid | CLIProxyAPI | cc-switch |
|---|---|---|---|
| 文本与 function tool | 已支持 | 已支持 | 已支持 |
| custom tool | 未支持 | 已支持 | 已支持 |
| tool search / 动态工具 | 未支持 | 支持 `additional_tools` | 支持完整上下文恢复 |
| `previous_response_id` 历史恢复 | 仅有 reasoning 缓存 | 转换层未完整恢复 | 恢复完整调用项 |
| 供应商 reasoning 适配 | 固定 `reasoning_effort` | 基础映射 | 按供应商和模型适配 |
| 多模态输入 | 文本以外会丢失 | 部分支持 | 覆盖图片、文件、音频 |
| SSE 健壮性 | 基础可靠，非完整 SSE parser | 较完整 | 完整事件块解析和容错 |

## 优先级与评估边界

- **P0**：已能由真实 Codex 请求触发，会直接中断工具闭环、导致上游硬错误，或形成多租户正确性与隔离风险。
- **P1**：常见模型、常见跨轮交互或异常上游响应会触发，造成数据丢失、错误完成或供应商兼容失败。
- **P2**：协议健壮性、字段完整度和可观测性问题；通常不阻断最基础的文本请求，但会增加长尾故障。

本文讨论的是“协议桥接”而不是本地工具执行平台。对于 Chat 后端自身不具备的 Web Search、Code Interpreter 等内置能力，转换器无法仅靠字段改写实现等价语义。此类输入必须选择明确拒绝、接入本地执行器或配置能力降级，不能静默删除后继续请求。

## 问题清单总览

| 编号 | 优先级 | 问题 | 主要后果 |
|---:|---|---|---|
| 1 | P0 | custom tool 未支持，未知 item 变成空消息 | 请求 400 或工具语义丢失 |
| 2 | P0 | tool search 与 `additional_tools` 无法建立动态工具上下文 | 后续工具不存在或调用链断裂 |
| 3 | P0 | reasoning 与 token 参数无条件映射 | 不同 Chat 后端硬错误 |
| 4 | P0/P1 | reasoning 缓存无会话、路由和上游隔离 | 跨请求污染；多租户风险 |
| 5 | P1 | `previous_response_id` 无完整历史恢复 | 增量请求缺少 assistant tool calls |
| 6 | P1 | 工具名称、schema、去重和 `tool_choice` 不够健壮 | 严格后端拒绝或调用错工具 |
| 7 | P1 | 残缺或无 index 的工具响应处理不足 | 事件身份错误或伪造成功 |
| 8 | P1 | reasoning 缺失时无供应商兼容策略 | 严格推理模型拒绝工具轮 |
| 9 | P1 | 图片、文件、音频和结构化工具输出被静默删除 | 上下文损坏或模型答非所问 |
| 10 | P1 | Chat 错误体未转换成 Responses 错误 | Codex 错误解析与诊断不稳定 |
| 11 | P2 | SSE 按物理行而非事件块解析 | 多行事件、`event:error` 等长尾故障 |
| 12 | P2 | 顶层字段与非流式响应校验不完整 | 性能字段失效或空响应被判成功 |

## 逐项问题分析

### 1. P0：custom tool 未支持，未知 item 变成空消息

#### 问题场景

Codex 使用 custom tool 表达自由格式输入，例如 `apply_patch` 一类以字符串而不是 JSON object 为输入的工具。请求的 `tools` 中包含 `type=custom`，`input` 中随后出现 `custom_tool_call` 和 `custom_tool_call_output`。

#### 最小触发方式

```json
{
  "model": "codex-model",
  "input": [
    {
      "type": "custom_tool_call",
      "call_id": "call_1",
      "name": "apply_patch",
      "input": "*** Begin Patch ..."
    },
    {
      "type": "custom_tool_call_output",
      "call_id": "call_1",
      "output": "Done"
    }
  ],
  "tools": [
    {"type": "custom", "name": "apply_patch"}
  ]
}
```

#### 当前行为

`protocol/responses.go` 的工具结构只正式表达 function；`convert/request.go` 的 `expandTools` 只展开 function 和 namespace。输入转换的 default 分支把未知 item 当作普通消息读取 `role/content`。custom item 没有这些字段，因此会形成 `role` 和 `content` 都为空的 Chat message。

#### 造成的问题

- 严格 Chat 后端因空 role 或非法消息序列返回 400。
- 宽松后端可能接受请求，但 custom 调用和结果完全丢失，模型无法继续正确推理。
- 响应方向没有 custom 类型恢复，Codex 收不到期望的 `custom_tool_call` 事件。
- 日志只显示“未知工具”时，问题容易被误诊为上游模型故障。

#### 修复方案

1. 在协议层显式建模 custom tool、`custom_tool_call` 和 `custom_tool_call_output`。
2. 建立请求级 `ToolContext`，记录展开后的 Chat 名称、原始 Responses 类型、namespace 和参数编码方式。
3. 将 custom 定义映射成合成的 Chat function tool，例如使用 object schema 包装单个 `input:string`；custom call 的字符串输入编码为 `{"input": ...}`。
4. Chat 响应转回 Responses 时依据 `ToolContext` 恢复 `custom_tool_call`，而不是一律输出 `function_call`。
5. 输入 item 使用穷举式分派。未知且具有语义的 item 返回明确的 `unsupported_input_item` 错误，不再生成空消息。

#### 验收标准

- 覆盖 custom 的非流式和流式往返测试。
- 字符串中的换行、引号和大体积 patch 不变形。
- 任何未知 item 都不会生成空 role/content。
- 本地真实 trace 中 21 组 custom 调用保持 call ID、name、input 和 output 配对。

### 2. P0：tool search 与 `additional_tools` 无法建立动态工具上下文

#### 问题场景与触发方式

Codex 先调用 `tool_search` 搜索可用工具，`tool_search_output` 再携带 `additional_tools`，之后模型调用其中某个动态工具。动态工具可能还处于 namespace 中。

请求中同时出现以下对象即可触发：

- `type=tool_search` 的工具定义；
- `tool_search_call`；
- `tool_search_output`；
- 输出中的 `additional_tools`；
- 后续对动态工具名的调用。

#### 当前行为及影响

apid 不认识 tool-search call/output，也不会扫描 `additional_tools`。工具定义被忽略，调用项可能退化成空消息，后续动态工具没有对应 Chat tools 定义。

结果是上游模型看不到动态工具、模型生成的工具名无法恢复、调用链在搜索阶段或实际调用阶段中断，namespace 展开后的名称碰撞也无法发现。

#### 修复方案

1. 在转换正文前，两遍扫描 `tools` 和 `input`，先建立完整 `ToolContext`。
2. 收集顶层 tools、namespace children 和 `tool_search_output.additional_tools`。
3. 记录原始类型、原始名称、namespace、展开名称、是否来自动态搜索以及参数编码器。
4. 若 apid 拥有 tool-search 执行能力，可将搜索动作映射成合成 function；若没有，返回明确的 `capability_not_supported`，不能假装搜索已执行。
5. 动态工具的 Chat 响应通过同一个上下文恢复到原始 Responses 类型。

#### 验收标准

增加“搜索 → 返回 `additional_tools` → 调用动态工具 → 返回 output”的完整回合测试；动态 namespace 工具可稳定恢复；不支持搜索执行时返回可识别的 Responses 错误。

### 3. P0：reasoning 与 max token 参数无条件映射

#### 问题场景与触发方式

同一个 apid route 指向不同 Chat 兼容供应商：某些支持 `reasoning_effort`，某些要求 `enable_thinking`，某些完全拒绝未知字段；新式推理模型可能要求 `max_completion_tokens`，传统端点只接受 `max_tokens`。

发送包含 `reasoning.effort` 或 `max_output_tokens` 的 Responses 请求，并将 upstream 切换到字段约束严格的 Chat 后端即可触发。

#### 当前行为及影响

`convert/request.go` 无条件写 `reasoning_effort`，并固定把 `max_output_tokens` 写入 `max_tokens`。转换器没有 upstream capability 输入。

后端可能直接返回 unknown field、unsupported parameter 或 max_tokens not supported。即使请求成功，reasoning 档位也可能被错误映射，导致成本、时延和输出行为偏离。

#### 修复方案

在 upstream 配置增加声明式 `ChatCapabilities`，至少表达：

- max-token 字段名；
- reasoning 参数形态；
- 允许的 effort 值；
- 是否需要 thinking 开关；
- 是否接受 `reasoning_content`；
- 是否要求工具轮的 reasoning 占位。

converter 只消费能力配置，不持续硬编码供应商名。默认策略应保守：未声明支持时不发送非标准 reasoning 字段。

#### 验收标准

用能力矩阵表驱动测试传统 Chat、o-series 风格和 `enable_thinking` 风格；确保同一 Responses 输入产生各自合法的 Chat 请求；非法配置在启动阶段报错。

### 4. P0/P1：reasoning 缓存无会话、路由和上游隔离

#### 问题场景与触发方式

网关同时服务多个用户、多个 route 或多个 upstream。会话 A 写入某段文本对应的 reasoning；会话 B 随后发送相同文本但没有 reasoning，或者两个上游使用相同 call ID。

#### 当前行为及影响

`reasoning/cache.go` 的全局缓存主要按 call ID 或内容 hash 查找；server 已提取 session ID，但没有参与缓存键。

可能导致：

- 错误 reasoning 被附加到另一请求；
- 不同模型的 reasoning 格式相互污染；
- 多租户环境出现跨用户上下文污染和潜在信息隔离风险。

单用户本地部署风险较低，可降为 P1；共享网关应按 P0 处理。

#### 修复方案

将缓存改成作用域明确的 `HistoryStore`。主键至少包含 tenant/session、route、upstream 和 response ID/call ID；内容 hash 只能作为同一会话内的最后回退，不能作为全局键。设置 TTL、总容量和单会话容量，并明确多实例时使用共享存储还是关闭跨实例恢复。

#### 验收标准

并发构造两个内容相同的会话，验证绝不串用 reasoning；同一会话可命中；route/upstream 切换后不命中；TTL 和容量淘汰有测试及指标。

### 5. P1：`previous_response_id` 无完整历史恢复

#### 问题场景与触发方式

Codex 采用增量会话，只提交 `previous_response_id` 和新的 `function_call_output`，而不重发产生该调用的 assistant item：

1. 第一轮返回 `response_id=resp_1`，其中包含 `function_call call_1`。
2. 第二轮请求只包含 `previous_response_id=resp_1` 和 `function_call_output call_1`。

#### 当前行为及影响

`ResponsesRequest` 未建模 `previous_response_id`；现有缓存只尝试恢复 reasoning，不保存完整 assistant tool call。转换后的 Chat 历史可能直接以 `role=tool` 开头，缺少对应的 `assistant.tool_calls`。

严格 Chat 后端会报 “tool message must follow tool_calls”；宽松后端会丢失工具名称和参数。重启或负载均衡到另一实例后行为也可能不一致。

#### 修复方案

按 response ID 保存可重放的 Responses output items，包括 function/custom/tool-search 的 call ID、name、namespace、arguments/input、reasoning 和原始顺序。转换新请求前沿 `previous_response_id` 链补齐所需 assistant 调用。

同时设置最大链深、环检测、TTL 和缺失历史策略；历史缺失时返回明确错误，不生成不合法 Chat 序列。

#### 验收标准

完整历史请求与 `previous_response_id` 增量请求产生语义等价的 Chat messages；覆盖重启后缓存缺失、链过深、ID 不存在和并行工具调用。

### 6. P1：工具名称、schema、去重和 `tool_choice` 不够健壮

#### 问题场景与触发方式

- namespace 和 tool 名拼接后超过 Chat 后端常见的 64 字节限制；
- 两个不同原始工具展开成相同名称；
- parameters 的 `type` 为 null 或非 object；
- structured `tool_choice` 同时包含 namespace 和 name。

构造长 namespace、Unicode 工具名、重复工具或不规范 JSON Schema，并强制选择 namespace 中的工具即可触发。

#### 当前行为及影响

apid 直接使用 `namespace__name`，没有长度控制和碰撞检测。`ensureObjectSchema` 只在 type 缺失时补 object；structured `tool_choice` 只读取 name。

严格后端可能拒绝请求；后端自行截断时可能调用错误工具；响应无法从扁平名恢复原始 namespace；重复定义会导致行为依赖供应商。

#### 修复方案

集中生成 Chat 工具名：限制 UTF-8 字节长度，超长时保留可读前缀并附稳定 hash；建立双向映射；展开后去重并对碰撞报错；将 parameters 根节点稳定归一为 object，同时保留 properties/required；structured `tool_choice` 通过 `ToolContext` 解析 namespace 和原始类型。

#### 验收标准

覆盖 ASCII/Unicode 边界、超长名称、hash 稳定性、重复与碰撞、type 缺失/null/错误类型、namespace tool choice 和往返恢复。

### 7. P1：残缺或无 index 的工具响应处理不足

#### 问题场景与触发方式

兼容性较弱的 Chat upstream 在流式 delta 中晚到 ID/name、遗漏 index、把 arguments 分多段发送，或在非流式响应中返回空 arguments、空 name、空 call ID。

典型触发请求是两个并行 tool call 都没有 index；第一块只有 arguments，后一块才出现 name/id；或者最终 choice 中 tool calls 存在但全部缺少 name。

#### 当前行为及影响

缺失 index 会落到同一默认索引；`output_item.added` 可能在 ID/name 尚为空时发出。非流式路径基本信任上游字段，choices 为空时也可能产生空 completed response。

结果可能是两个调用被合并、added 与 done 身份不一致、后续 output 无法按 call ID 配对，或者工具全部损坏时仍伪造“成功完成”。

#### 修复方案

- 流式状态先缓冲工具身份，达到最小合法条件后再发送 `output_item.added`。
- 缺失 ID 时生成稳定 call ID。
- 空 arguments 归一为 `{}`。
- 缺失 name 的调用丢弃并记录诊断。
- 无 index 时结合 ID、name 和到达顺序分组。
- 如果 `finish_reason=tool_calls` 但没有任何有效工具项，产生 `response.failed` 或网关 502。

#### 验收标准

覆盖 ID/name 晚到、缺 index、并行交错、空 arguments、缺 name、重复 index 和提前断流；断言事件严格满足 added → delta → done，且不存在空身份事件。

### 8. P1：reasoning 缺失时无供应商兼容策略

#### 问题场景与触发方式

Codex 历史中的 reasoning item 只有空 summary，`encrypted_content` 也为空；缓存首次使用、重启、淘汰或跨实例后无法恢复原始 reasoning。某些推理型 Chat 后端要求 assistant tool-call 消息存在非空 `reasoning_content`。

清空缓存后转发一段包含历史工具调用、但 reasoning summary 为空的 Codex 请求到严格的 DeepSeek/Kimi 类兼容端点即可触发。

#### 当前行为及影响

apid 在缓存和 pending summary 都没有值时发送空 reasoning，也不会从 content 中的 `<think>...</think>` 恢复 reasoning。

严格后端可能拒绝工具调用历史；出现重启前能用、重启后失败；内嵌 thinking 被当成普通答案文本，Responses 客户端的 reasoning/content 语义混合。

#### 修复方案

把行为放入 upstream capability：要求 reasoning 的后端在缺失时填入稳定占位文本；支持内嵌 thinking 的后端可配置解析 `<think>` 块。占位仅用于满足历史格式，不应伪装成真实推理，也不应默认对所有后端开启。

#### 验收标准

缓存命中、缓存未命中、空 summary、内嵌 think 和普通文本五类测试；占位策略仅在声明能力的 upstream 生效。

### 9. P1：多模态输入和结构化工具输出被静默删除

#### 问题场景与触发方式

Responses message 包含 `input_image`、`input_file` 或 `input_audio`；工具输出由文本和图片组成，而目标 Chat 后端支持相应多模态 content parts。向 Codex 输入截图/文件，或者让工具返回带图片的结构化 output 即可触发。

#### 当前行为及影响

`extractText` 只拼接文本，其他 content part 不报错但不会进入 Chat 请求。模型不知道用户提供了图片或文件，问题可能从“分析截图”退化为无上下文猜测。因为请求仍可能返回 200，数据丢失不易发现。

#### 修复方案

扩展 `ChatMessage.Content` 为 string 或 content-part 联合类型；优先实现 `input_image` 及工具输出中的文本/图片，再按供应商能力增加 file/audio。upstream 声明不支持时，返回 `unsupported_content_type`，或者通过显式配置允许降级；默认不能静默删除。

#### 验收标准

图片 URL、data URL、混合文本图片、结构化 tool output 和不支持能力的错误路径都有测试；转换前后 content part 顺序不变。

### 10. P1：Chat 错误未归一为 Responses 错误

#### 问题场景与触发方式

Chat upstream 返回 OpenAI Chat 风格 JSON error、供应商自定义错误、纯文本/HTML 502，或在 SSE 中发送 `event:error`。非法参数、限流和反向代理错误均可触发。

#### 当前行为及影响

非 2xx 响应原样回传；流式转换只重点识别 `data` JSON 内的部分错误。HTTP 状态可以保留，但 body 不一定符合 Responses 客户端预期。

Codex 可能无法提取 `error.message/type/code`，重试策略失准；用户只看到解析错误而不是根因；流已经发出 `response.created` 后出现上游错误时，事件生命周期不完整。

#### 修复方案

增加统一 `ErrorNormalizer`：保留原 HTTP status、request ID 和安全的上游错误信息，输出 Responses error schema；纯文本/HTML 包装为 `upstream_error`；流中错误生成 error 或 `response.failed`，并关闭已开启的 item。原始错误只写 trace，避免把敏感响应头和内部地址暴露给客户端。

#### 验收标准

覆盖 400、401、429、500、纯文本、非 JSON、SSE `event:error` 和中途断流；Codex 始终能读取稳定的 message/type/code；不得把上游密钥或内部 URL 写入错误。

### 11. P2：SSE 按物理行而非完整事件块解析

#### 问题场景与触发方式

合法 SSE 事件包含多行 data、event 字段、注释、CRLF，或者 UTF-8 字符跨底层读取边界；上游还可能发送 malformed JSON 后继续输出。

构造由空行终止、含两行 data 的事件，发送 `event:error`，在多字节字符中分割底层数据，或插入一个非法 JSON chunk 即可触发。

#### 当前行为及影响

apid 逐行处理 `data:`，没有先聚合成 SSE 事件块；非法 JSON chunk 会被跳过。其优点是使用 `bufio.Reader`，不受 Scanner 常见的 64 KiB token 限制，并会拒绝没有正常 finish reason 的提前 EOF。

当前实现可能把多行 data 拆成两个无效 JSON，忽略 `event:error`，或让非法 chunk 造成内容缺口但缺少明确诊断。

#### 修复方案

实现小型 SSE block parser：按空行提交事件，合并多行 data，识别 event/id/retry 和注释，统一处理 CRLF 和流尾残留；JSON 解析错误应根据是否已有可恢复状态选择 `response.failed` 或终止，而不是无条件跳过。

#### 验收标准

基于分片矩阵测试同一事件在任意字节边界切分；覆盖多行 data、CRLF、注释、超长行、`event:error`、`[DONE]`、非法 JSON 和无结束标记 EOF。

### 12. P2：顶层字段和非流式响应校验不完整

#### 问题场景与触发方式

Codex 请求携带 `prompt_cache_key`、`previous_response_id`、`include`、`store`、`metadata`、`client_metadata`、`truncation` 或 `max_tool_calls`；上游返回 HTTP 200 但 choices 为空或 message 缺失。

#### 当前行为及影响

`ResponsesRequest` 没有建模多数顶层字段，字段在 JSON 反序列化时静默消失。非流式 response converter 读取第一项 choice；没有 choice 时使用零值 message，可能生成空 completed response。

由此会造成 prompt cache 命中率下降、客户端状态字段无法参与历史恢复、控制字段看似生效但实际被忽略，以及上游协议故障被包装成成功。

#### 修复方案

按用途分类字段：

- `previous_response_id` 进入 `HistoryStore`；
- `prompt_cache_key` 在 capability 允许时透传；
- `include/store/metadata` 等明确记录“消费、透传或忽略”策略；
- 响应侧验证 choices 非空、message 或有效 tool calls 存在；
- 不合法的 200 响应转换为 `upstream_protocol_error`；
- 补充 usage 中缓存 token 等兼容字段，但不虚构不存在的数据。

#### 验收标准

每个已建模字段都有单测说明其去向；未知顶层字段是否允许由明确策略决定；choices 为空、message 缺失、只有 reasoning、只有 tool calls 等响应均有确定结果。

## 建议的实现分期

### 第一阶段：恢复当前 Codex 工具闭环

1. 先完成问题 1：custom tool 全链路，并让未知 item fail closed。
2. 随后完成问题 2：统一 `ToolContext`、`additional_tools` 和 tool-search 策略。
3. 同步完成问题 6、7 中与工具身份直接相关的名称映射、call ID 和流式缓冲。

第一阶段的完成标准不是“请求不报错”，而是 custom/function/namespace/dynamic tool 在非流式和流式中都能往返恢复，且真实 Codex trace 不产生空消息、空工具名或错配 call ID。

### 第二阶段：兼容不同 Chat 后端与跨轮会话

1. 完成问题 3：引入 upstream `ChatCapabilities`。
2. 将问题 4、5 合并设计为有作用域的 `HistoryStore`，避免先扩张旧的全局 reasoning cache。
3. 基于 capability 完成问题 8 的 reasoning 占位和 think 标签处理。

### 第三阶段：数据保真和故障可诊断性

1. 完成问题 9 的多模态内容策略。
2. 完成问题 10、11 的错误归一和完整 SSE parser。
3. 最后处理问题 12 的字段覆盖、响应 envelope 和 usage 完整度。

## 建议的代码结构

| 组件 | 职责 |
|---|---|
| `ChatCapabilities` | 描述 upstream 对 max token、reasoning、thinking、多模态、工具名限制及非标准字段的支持 |
| `ToolContext` | 请求级工具注册表；负责 function/custom/namespace/dynamic tool 的展开、去重、名称限制、参数编码和响应恢复 |
| `HistoryStore` | 按 tenant/session/route/upstream/response ID 保存可重放调用项，替代全局 reasoning-only 缓存 |
| `SSEParser` | 只负责 SSE framing；转换状态机继续负责 Responses added/delta/done 不变量 |
| `ErrorNormalizer` | 把 HTTP、Chat JSON、纯文本和 SSE 错误统一为 Responses 客户端可消费的错误 |

这一拆分可避免 `convert/request.go` 和 `convert/stream.go` 逐渐演变成供应商特例集合，也能保留 apid 当前“协议结构、转换、上游、服务器编排”职责清楚的优势。

## 回归测试建议

- 建立 table-driven golden tests：每个 Responses 请求对应期望 Chat 请求，再用 Chat 响应恢复期望 Responses。
- 工具类型至少覆盖 function、custom、namespace function、namespace custom、tool search 和 `additional_tools`。
- 流式测试按任意字节边界切分，不只按 JSON chunk 边界切分。
- 所有测试都验证事件生命周期、call ID 配对、输出顺序，以及失败时不得伪造 completed。
- 把已脱敏的真实 Codex trace 固化为结构回归 fixture，禁止提交用户消息、密钥或敏感工具输出。
- 为三类代表性 Chat capability 建契约测试：传统 Chat、推理型 Chat、严格工具调用 Chat。

## 现有实现中应保留的行为

- 并行 function call 与对应 tool output 的邻接整理逻辑。
- 文本、reasoning、tool call 分别维护 added → delta → done 的流式事件不变量。
- 识别 `reasoning_content`、reasoning 字符串/对象和 `reasoning_details` 的兼容读取。
- 将 `length` 和 `content_filter` 结束原因映射为 incomplete。
- 检测没有正常结束标记的提前 EOF。
- 递归展开 namespace 的能力。

## 证据位置与限制

apid 主要证据位置：

- `protocol/responses.go`
- `protocol/chat.go`
- `protocol/chat_reasoning.go`
- `convert/request.go`
- `convert/response.go`
- `convert/stream.go`
- `reasoning/cache.go`
- `server/server.go`
- `server/session_id.go`

cc-switch 主要对比位置：

- `src-tauri/src/proxy/providers/transform_codex_chat.rs`
- `src-tauri/src/proxy/providers/streaming_codex_chat.rs`
- `src-tauri/src/proxy/providers/codex_chat_history.rs`
- `src-tauri/src/proxy/providers/codex_chat_common.rs`
- 集成层 `forwarder.rs`、`handlers.rs`

CLIProxyAPI 主要对比位置：

- `internal/translator/openai/openai/responses/openai_openai-responses_request.go`
- `internal/translator/openai/openai/responses/openai_openai-responses_response.go`
- `internal/translator/openai/openai/responses/openai_openai-responses_tools.go`

本报告基于上述本地代码版本和一份本地 Codex trace 的结构统计，没有把某一家供应商的当前行为推断为 OpenAI 官方协议保证。实际实施每个 capability 前，仍应使用目标 upstream 的请求契约或探测测试确认。

验证情况：apid 的 `go test ./...` 与 CLIProxyAPI 对应 translator 测试通过；cc-switch 测试因当前环境无法向 rustup 临时目录写入而未执行，相关判断来自源码、测试用例和该模块的修复历史。

本报告提出的是修复候选和顺序，不代表所有能力都应默认开启。优先保证“不丢语义、不伪造成功、能力不支持时明确失败”，再按实际 upstream 和 Codex 使用方式选择兼容范围。
