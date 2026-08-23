package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/abowloflrf/apid/config"
)

func TestModelsListsSortedExactChatModels(t *testing.T) {
	cfg := config.Config{
		Routes: []config.Route{
			{
				Path:          "/v1/chat/completions",
				InputProtocol: config.ProtoChat,
				Models: []config.ModelRule{
					{Match: "zeta", Upstream: "chat"},
					{Match: "chat-*", Upstream: "chat"},
					{Match: "", Upstream: "chat"},
					{Match: "alpha", Upstream: "chat"},
				},
			},
			{
				Path:          "/other/chat/completions",
				InputProtocol: config.ProtoChat,
				Models: []config.ModelRule{
					{Match: "alpha", Upstream: "chat"},
					{Match: "middle", Upstream: "chat"},
				},
			},
			{
				Path:          "/v1/responses",
				InputProtocol: config.ProtoResponses,
				Models: []config.ModelRule{
					{Match: "responses-only", Upstream: "responses"},
				},
			},
			{
				Path:          "/v1/messages",
				InputProtocol: config.ProtoAnthropic,
				Models: []config.ModelRule{
					{Match: "anthropic-only", Upstream: "anthropic"},
				},
			},
		},
	}
	srv := New(cfg, nil, nil)
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("GET /v1/models Content-Type = %q, want application/json", got)
	}

	var got modelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.Object != "list" {
		t.Errorf("object = %q, want list", got.Object)
	}
	wantIDs := []string{"alpha", "middle", "zeta"}
	if len(got.Data) != len(wantIDs) {
		t.Fatalf("models = %+v, want IDs %v", got.Data, wantIDs)
	}
	for i, wantID := range wantIDs {
		model := got.Data[i]
		if model.ID != wantID || model.Object != "model" || model.Created != 0 || model.OwnedBy != "apid" {
			t.Errorf("model[%d] = %+v, want id=%q object=model created=0 owned_by=apid", i, model, wantID)
		}
	}
}

func TestModelsReturnsEmptyList(t *testing.T) {
	srv := New(config.Config{}, nil, nil)
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))

	if got := rec.Body.String(); got != "{\"object\":\"list\",\"data\":[]}\n" {
		t.Errorf("GET /v1/models body = %q", got)
	}
}

func TestModelsUsesClientAPIKey(t *testing.T) {
	srv := New(config.Config{ClientAPIKey: "client-key"}, nil, nil)
	t.Cleanup(srv.Close)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("GET /v1/models without key status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer client-key")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /v1/models with key status = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}
