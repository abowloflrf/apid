package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

const (
	maxSessionIDLength    = 1024
	maxTurnMetadataLength = 64 * 1024
	maxIdentityHintLength = 256
)

type bodySessionMetadata struct {
	canonicalSession string
	flatSession      string
	promptCacheKey   string
	optionsSession   string
	hasCodexMetadata bool
}

// extractAgentSessionID normalizes the session identifiers emitted by Claude
// Code, Codex, pi, and OpenCode into one value. Ambiguous cache/request IDs are
// only used when another field confirms they represent the same session.
func extractAgentSessionID(header http.Header, requestBody []byte) string {
	claudeSession := sessionHeader(header, "x-claude-code-session-id")
	openCodeSession := sessionHeader(header, "x-opencode-session")
	sessionID := sessionHeader(header, "session-id")
	sessionIDUnderscore := sessionHeader(header, "session_id")
	threadID := sessionHeader(header, "thread-id")
	xSessionID := sessionHeader(header, "x-session-id")
	sessionAffinity := sessionHeader(header, "x-session-affinity")
	clientRequestID := sessionHeader(header, "x-client-request-id")
	originator := strings.ToLower(boundedValue(header.Get("originator"), maxIdentityHintLength))
	userAgent := strings.ToLower(boundedValue(header.Get("user-agent"), maxIdentityHintLength))

	if claudeSession != "" {
		return claudeSession
	}
	if openCodeSession != "" {
		return openCodeSession
	}

	body := parseBodySessionMetadata(requestBody)
	headerTurnMetadata := boundedValue(header.Get("x-codex-turn-metadata"), maxTurnMetadataLength)
	headerCanonicalSession := parseCodexSession(headerTurnMetadata)
	hasCodexHeaders := threadID != "" || headerTurnMetadata != "" ||
		sessionHeader(header, "x-codex-parent-thread-id") != "" ||
		sessionHeader(header, "x-codex-window-id") != ""
	if body.hasCodexMetadata || hasCodexHeaders {
		return firstSessionID(body.canonicalSession, body.flatSession, headerCanonicalSession, sessionID)
	}

	isOpenCode := originator == "opencode" || strings.HasPrefix(userAgent, "opencode/") ||
		(xSessionID != "" && sessionAffinity != "")
	if isOpenCode {
		return firstSessionID(xSessionID, sessionAffinity, sessionID, sessionIDUnderscore)
	}

	isPi := originator == "pi" || strings.HasPrefix(userAgent, "pi/") ||
		strings.HasPrefix(userAgent, "pi (") || strings.HasPrefix(userAgent, "pi-coding-agent")
	if isPi {
		return firstSessionID(sessionID, sessionIDUnderscore, xSessionID, sessionAffinity,
			body.optionsSession, body.promptCacheKey)
	}

	switch {
	case sessionIDUnderscore != "" && clientRequestID != "":
		return sessionIDUnderscore
	case sessionID != "" && clientRequestID != "":
		return sessionID
	case xSessionID != "":
		return xSessionID
	case sessionID != "":
		return sessionID
	case sessionIDUnderscore != "":
		return sessionIDUnderscore
	case sessionAffinity != "":
		return sessionAffinity
	case body.optionsSession != "":
		return body.optionsSession
	default:
		return corroboratedCacheKey(clientRequestID, body.promptCacheKey)
	}
}

func parseBodySessionMetadata(requestBody []byte) bodySessionMetadata {
	var envelope struct {
		ClientMetadata  json.RawMessage `json:"client_metadata"`
		PromptCacheKey  json.RawMessage `json:"prompt_cache_key"`
		ProviderOptions json.RawMessage `json:"options"`
	}
	if json.Unmarshal(requestBody, &envelope) != nil {
		return bodySessionMetadata{}
	}
	meta := bodySessionMetadata{
		promptCacheKey: jsonString(envelope.PromptCacheKey),
	}

	var options struct {
		SessionID json.RawMessage `json:"sessionId"`
	}
	if json.Unmarshal(envelope.ProviderOptions, &options) == nil {
		meta.optionsSession = jsonString(options.SessionID)
	}

	var clientMetadata struct {
		SessionID         json.RawMessage `json:"session_id"`
		ThreadID          json.RawMessage `json:"thread_id"`
		CodexTurnMetadata json.RawMessage `json:"x-codex-turn-metadata"`
	}
	if json.Unmarshal(envelope.ClientMetadata, &clientMetadata) != nil {
		return meta
	}
	meta.flatSession = jsonString(clientMetadata.SessionID)
	threadID := jsonString(clientMetadata.ThreadID)
	canonicalJSON := jsonStringWithLimit(clientMetadata.CodexTurnMetadata, maxTurnMetadataLength)
	meta.canonicalSession = parseCodexSession(canonicalJSON)
	meta.hasCodexMetadata = threadID != "" || canonicalJSON != ""
	return meta
}

func parseCodexSession(raw string) string {
	if raw == "" {
		return ""
	}
	var metadata struct {
		SessionID json.RawMessage `json:"session_id"`
	}
	if json.Unmarshal([]byte(raw), &metadata) != nil {
		return ""
	}
	return jsonString(metadata.SessionID)
}

func sessionHeader(header http.Header, name string) string {
	return boundedValue(header.Get(name), maxSessionIDLength)
}

func jsonString(raw json.RawMessage) string {
	return jsonStringWithLimit(raw, maxSessionIDLength)
}

func jsonStringWithLimit(raw json.RawMessage, limit int) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return boundedValue(value, limit)
}

func boundedValue(value string, limit int) string {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > limit {
		return ""
	}
	return value
}

func corroboratedCacheKey(requestID, cacheKey string) string {
	if requestID != "" && requestID == cacheKey {
		return cacheKey
	}
	return ""
}

func firstSessionID(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
