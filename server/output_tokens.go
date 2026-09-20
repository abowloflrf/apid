package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/abowloflrf/apid/config"
)

// defaultOutputTokens preserves explicit limits and unrelated JSON fields.
func defaultOutputTokens(body []byte, proto config.Protocol, limit int, chatField string) ([]byte, error) {
	var request map[string]json.RawMessage
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	if request == nil {
		return nil, fmt.Errorf("request body must be a JSON object")
	}
	field := "max_tokens"
	if proto == config.ProtoResponses {
		field = "max_output_tokens"
	}
	fields := []string{field}
	if proto == config.ProtoChat {
		field = chatField
		fields = []string{"max_tokens", "max_completion_tokens"}
	}
	for _, key := range fields {
		if raw, ok := request[key]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return body, nil
		}
	}
	// Remove a null alias so Chat backends never receive both token parameters.
	if proto == config.ProtoChat {
		delete(request, "max_tokens")
		delete(request, "max_completion_tokens")
	}
	request[field] = json.RawMessage(strconv.Itoa(limit))
	return json.Marshal(request)
}
