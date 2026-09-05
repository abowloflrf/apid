package convert

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/abowloflrf/apid/protocol"
)

const (
	toolResultMediaMovedMarker = "[apid: tool result media moved to the following user message]"
	maxToolMediaDepth          = 32
)

type toolOutputMediaPlan struct {
	content string
	output  json.RawMessage
	media   []protocol.ChatContentPart
}

func responsesContentToChat(raw json.RawMessage) (string, []protocol.ChatContentPart) {
	if len(raw) == 0 {
		return "", nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text, nil
	}

	var input []protocol.InputContentPart
	if json.Unmarshal(raw, &input) != nil {
		return "", nil
	}
	return responsesPartsToChat(input)
}

func responsesPartsToChat(input []protocol.InputContentPart) (string, []protocol.ChatContentPart) {
	var text strings.Builder
	parts := make([]protocol.ChatContentPart, 0, len(input))
	hasImage := false
	for _, part := range input {
		switch part.Type {
		case "", "input_text", "output_text", "text":
			if part.Text == "" {
				continue
			}
			text.WriteString(part.Text)
			parts = append(parts, protocol.ChatContentPart{Type: "text", Text: part.Text})
		case "input_image", "image_url":
			image, ok := responsesImageToChat(part.ImageURL, part.FileID, part.Detail)
			if !ok {
				continue
			}
			parts = append(parts, protocol.ChatContentPart{Type: "image_url", ImageURL: image})
			hasImage = true
		}
	}
	if !hasImage {
		return text.String(), nil
	}
	return text.String(), parts
}

func responsesImageToChat(raw json.RawMessage, fileID, detail string) (*protocol.ChatImageURL, bool) {
	image := protocol.ChatImageURL{FileID: fileID, Detail: normalizeChatImageDetail(detail)}
	if len(raw) > 0 {
		if json.Unmarshal(raw, &image.URL) != nil {
			var object struct {
				URL    string `json:"url"`
				FileID string `json:"file_id"`
				Detail string `json:"detail"`
			}
			if json.Unmarshal(raw, &object) == nil {
				image.URL = object.URL
				if image.FileID == "" {
					image.FileID = object.FileID
				}
				if image.Detail == "" {
					image.Detail = normalizeChatImageDetail(object.Detail)
				}
			}
		}
	}
	if strings.TrimSpace(image.URL) == "" && strings.TrimSpace(image.FileID) == "" {
		return nil, false
	}
	return &image, true
}

func normalizeChatImageDetail(detail string) string {
	switch strings.ToLower(strings.TrimSpace(detail)) {
	case "auto", "low", "high":
		return strings.ToLower(strings.TrimSpace(detail))
	case "original":
		return "high"
	default:
		return ""
	}
}

func planToolOutputMedia(raw json.RawMessage) *toolOutputMediaPlan {
	if len(raw) == 0 {
		return nil
	}
	var value any
	if decodeToolMediaJSON(raw, &value) != nil {
		return nil
	}

	if text, ok := value.(string); ok {
		if image, ok := chatImageFromDataURL(text); ok {
			output, _ := json.Marshal(toolResultMediaMovedMarker)
			return &toolOutputMediaPlan{
				content: toolResultMediaMovedMarker,
				output:  output,
				media:   []protocol.ChatContentPart{image},
			}
		}
		var nested any
		if decodeToolMediaJSON([]byte(text), &nested) != nil {
			return nil
		}
		cleaned, media, count := stripToolImages(nested, 0)
		if count == 0 {
			return nil
		}
		cleanedJSON, err := json.Marshal(cleaned)
		if err != nil {
			return nil
		}
		cleanedText := string(cleanedJSON)
		output, _ := json.Marshal(cleanedText)
		return &toolOutputMediaPlan{content: cleanedText, output: output, media: media}
	}

	cleaned, media, count := stripToolImages(value, 0)
	if count == 0 {
		return nil
	}
	output, err := json.Marshal(cleaned)
	if err != nil {
		return nil
	}
	return &toolOutputMediaPlan{content: string(output), output: output, media: media}
}

func decodeToolMediaJSON(data []byte, value *any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("unexpected trailing JSON data")
	}
	return nil
}

func stripToolImages(value any, depth int) (any, []protocol.ChatContentPart, int) {
	if depth > maxToolMediaDepth {
		return value, nil, 0
	}
	switch value := value.(type) {
	case string:
		if image, ok := chatImageFromDataURL(value); ok {
			return toolResultMediaMovedMarker, []protocol.ChatContentPart{image}, 1
		}
		var nested any
		if decodeToolMediaJSON([]byte(value), &nested) != nil {
			return value, nil, 0
		}
		cleaned, media, count := stripToolImages(nested, depth+1)
		if count == 0 {
			return value, nil, 0
		}
		b, err := json.Marshal(cleaned)
		if err != nil {
			return value, nil, 0
		}
		return string(b), media, count

	case []any:
		var media []protocol.ChatContentPart
		count := 0
		for i, item := range value {
			cleaned, found, n := stripToolImages(item, depth+1)
			value[i] = cleaned
			media = append(media, found...)
			count += n
		}
		return value, media, count

	case map[string]any:
		if image, ok := chatImageFromToolValue(value); ok {
			return map[string]any{"type": "text", "text": toolResultMediaMovedMarker},
				[]protocol.ChatContentPart{image}, 1
		}
		content, ok := value["content"]
		if !ok {
			return value, nil, 0
		}
		cleaned, media, count := stripToolImages(content, depth+1)
		if count > 0 {
			value["content"] = cleaned
		}
		return value, media, count
	}
	return value, nil, 0
}

func chatImageFromToolValue(value map[string]any) (protocol.ChatContentPart, bool) {
	typeName, _ := value["type"].(string)
	switch typeName {
	case "input_image", "image_url":
		return chatImageFromURLValue(value)
	case "image":
		if source, ok := value["source"].(map[string]any); ok {
			if url := stringValue(source, "url"); url != "" {
				return newChatImage(url, "", stringValue(value, "detail")), true
			}
			if data := stringValue(source, "data", "base64"); data != "" {
				mimeType := stringValue(source, "media_type", "mime_type", "mimeType")
				return newChatImage(imageDataURL(mimeType, data), "", stringValue(value, "detail")), true
			}
		}
		if data := stringValue(value, "data"); data != "" {
			mimeType := stringValue(value, "mimeType", "mime_type")
			if strings.HasPrefix(strings.ToLower(mimeType), "image/") {
				return newChatImage(imageDataURL(mimeType, data), "", stringValue(value, "detail")), true
			}
		}
	case "":
		image, ok := chatImageFromURLValue(value)
		if ok && isImageDataURL(image.ImageURL.URL) {
			return image, true
		}
	}
	return protocol.ChatContentPart{}, false
}

func chatImageFromURLValue(value map[string]any) (protocol.ChatContentPart, bool) {
	var url, fileID, detail string
	switch imageURL := value["image_url"].(type) {
	case string:
		url = imageURL
	case map[string]any:
		url = stringValue(imageURL, "url")
		fileID = stringValue(imageURL, "file_id")
		detail = stringValue(imageURL, "detail")
	}
	if fileID == "" {
		fileID = stringValue(value, "file_id")
	}
	if detail == "" {
		detail = stringValue(value, "detail")
	}
	if strings.TrimSpace(url) == "" && strings.TrimSpace(fileID) == "" {
		return protocol.ChatContentPart{}, false
	}
	return newChatImage(url, fileID, detail), true
}

func chatImageFromDataURL(value string) (protocol.ChatContentPart, bool) {
	value = strings.TrimSpace(value)
	if !isImageDataURL(value) {
		return protocol.ChatContentPart{}, false
	}
	return newChatImage(value, "", ""), true
}

func newChatImage(url, fileID, detail string) protocol.ChatContentPart {
	return protocol.ChatContentPart{
		Type: "image_url",
		ImageURL: &protocol.ChatImageURL{
			URL:    url,
			FileID: fileID,
			Detail: normalizeChatImageDetail(detail),
		},
	}
}

func imageDataURL(mimeType, data string) string {
	if isImageDataURL(data) {
		return data
	}
	if !strings.HasPrefix(strings.ToLower(mimeType), "image/") {
		mimeType = "image/png"
	}
	return fmt.Sprintf("data:%s;base64,%s", mimeType, data)
}

func isImageDataURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	comma := strings.IndexByte(value, ',')
	return comma > 0 && strings.HasPrefix(value[:comma], "data:image/") &&
		strings.HasSuffix(value[:comma], ";base64")
}

func stringValue(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if text, ok := value[key].(string); ok && strings.TrimSpace(text) != "" {
			return text
		}
	}
	return ""
}
