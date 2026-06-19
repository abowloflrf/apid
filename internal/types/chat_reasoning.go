package types

import (
	"encoding/json"
	"strings"
)

// UnmarshalJSON 在标准解析之外做两件事：
//  1. 把 content 归一为字符串——上游可能返回字符串、内容块数组或干脆省略
//     (配 refusal)，直接按 string 解析会让整条响应反序列化失败。
//  2. 按多家上游约定回退提取 reasoning 文本：
//     reasoning_content > reasoning(字符串/对象) > reasoning_details。
//     兼容 vLLM/DeepSeek(reasoning_content)、OpenRouter(reasoning 字符串)及结构化 reasoning。
func (m *ChatMessage) UnmarshalJSON(data []byte) error {
	type alias ChatMessage // 借别名避免递归调用本方法
	// 用 RawMessage 接住 content，避免数组形态炸掉整次解析；其余字段经嵌入的
	// alias 直接落到 m 上(content 字段被浅层的 RawMessage 遮蔽，不会写进 m)。
	aux := struct {
		*alias
		Content json.RawMessage `json:"content"`
	}{alias: (*alias)(m)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	m.Content = contentText(aux.Content)
	if m.ReasoningContent == "" {
		m.ReasoningContent = extractReasoningField(data)
	}
	return nil
}

// contentText 提取 Chat 响应里 message.content 的纯文本，
// 兼容上游把 content 返回成字符串或内容块数组([{type,text},...])两种形态。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []InputContentPart
	if json.Unmarshal(raw, &parts) != nil {
		return ""
	}
	var b strings.Builder
	for _, p := range parts {
		b.WriteString(p.Text)
	}
	return b.String()
}

// UnmarshalJSON 同 ChatMessage，处理流式 delta 里的多态 reasoning 字段。
func (d *ChatChunkDelta) UnmarshalJSON(data []byte) error {
	type alias ChatChunkDelta
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	*d = ChatChunkDelta(a)
	if d.ReasoningContent == "" {
		d.ReasoningContent = extractReasoningField(data)
	}
	return nil
}

// extractReasoningField 从一条 message/delta 的原始 JSON 提取 reasoning 文本。
// reasoning_content 已由标准解析处理，这里只兜底其余字段形态。
func extractReasoningField(data []byte) string {
	var probe struct {
		Reasoning        json.RawMessage `json:"reasoning"`
		ReasoningDetails json.RawMessage `json:"reasoning_details"`
	}
	if json.Unmarshal(data, &probe) != nil {
		return ""
	}
	if s := reasoningFromValue(probe.Reasoning); s != "" {
		return s
	}
	return reasoningFromDetails(probe.ReasoningDetails)
}

// reasoningFromValue 处理 reasoning 字段：纯字符串，或 {content,text,summary} 对象。
func reasoningFromValue(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return objectText(raw, "content", "text", "summary")
}

// reasoningFromDetails 处理 reasoning_details 字段：字符串 / 数组 / 单对象。
func reasoningFromDetails(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var arr []json.RawMessage
	if json.Unmarshal(raw, &arr) == nil {
		return joinParts(arr)
	}
	return reasoningDetailPart(raw)
}

// reasoningDetailPart 从一个 detail 块取文本：text/content/summary 字符串，或递归其 parts。
func reasoningDetailPart(raw json.RawMessage) string {
	if t := objectText(raw, "text", "content", "summary"); t != "" {
		return t
	}
	var obj struct {
		Parts []json.RawMessage `json:"parts"`
	}
	if json.Unmarshal(raw, &obj) == nil && len(obj.Parts) > 0 {
		return joinParts(obj.Parts)
	}
	return ""
}

// joinParts 把多个 detail 块的文本用空行连接，跳过空块。
func joinParts(parts []json.RawMessage) string {
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := reasoningDetailPart(p); t != "" {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n\n")
}

// objectText 从一个 JSON 对象里按 keys 顺序取第一个非空字符串字段；非对象返回空。
func objectText(raw json.RawMessage, keys ...string) string {
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := obj[k]; ok {
			var s string
			if json.Unmarshal(v, &s) == nil && s != "" {
				return s
			}
		}
	}
	return ""
}
