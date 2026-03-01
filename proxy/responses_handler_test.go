package proxy

import (
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestConvertInputToChatMessages_PreservesMultimodalContentParts(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{
					"type":      "input_image",
					"image_url": "https://example.com/cat.png",
				},
			},
		},
	}

	got := convertInputToChatMessages(input)
	if len(got) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got))
	}
	if got[0]["role"] != "user" {
		t.Fatalf("role = %v, want user", got[0]["role"])
	}
	if !reflect.DeepEqual(got[0]["content"], input[0].(map[string]any)["content"]) {
		t.Fatalf("content was modified or dropped: got=%#v", got[0]["content"])
	}
}

func TestConvertInputToChatMessages_PreservesArbitraryContentShape(t *testing.T) {
	input := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": map[string]any{
				"custom": true,
				"value":  42,
			},
		},
	}

	got := convertInputToChatMessages(input)
	if len(got) != 1 {
		t.Fatalf("messages count = %d, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0]["content"], input[0].(map[string]any)["content"]) {
		t.Fatalf("content was modified: got=%#v", got[0]["content"])
	}
}

func TestResponsesHandler_NoSupportedModel_NoFallback_ReturnsModelNotFound(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 1,
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Token:           "sk-test",
				APIType:         config.APITypeResponses,
				AvailableModels: []string{"gpt-4o"},
			},
		},
	}

	h := NewResponsesHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5",
		"input":"hello"
	}`))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "model_not_found") {
		t.Fatalf("body = %q, want model_not_found", rec.Body.String())
	}
}
