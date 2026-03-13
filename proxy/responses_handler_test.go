package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/missdeer/aiproxy/balancer"
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
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeResponses,
				AvailableModels: config.AvailableModelList{"gpt-4o"},
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	h := NewResponsesHandler(cfg, bal)
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

func TestResponsesHandler_ForwardRequest_NativeResponsesCompactPreservesPathAndQuery(t *testing.T) {
	cfg := &config.Config{UpstreamRequestTimeout: 1}
	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewResponsesHandler(cfg, bal)

	var gotURL string
	var gotBody map[string]any

	handler.client.Store(&http.Client{
		Transport: &mockRoundTripper{
			handler: func(req *http.Request) (*http.Response, error) {
				gotURL = req.URL.String()

				bodyBytes, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatalf("ReadAll(req.Body) error = %v", err)
				}
				if err := json.Unmarshal(bodyBytes, &gotBody); err != nil {
					t.Fatalf("json.Unmarshal(body) error = %v", err)
				}

				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`{"id":"resp_123"}`)),
					Header:     http.Header{"Content-Type": {"application/json"}},
				}, nil
			},
		},
	})

	upstream := config.Upstream{
		Name:    "test-upstream",
		BaseURL: "https://api.example.com/",
		Tokens:  config.TokenList{"sk-test"},
		APIType: config.APITypeResponses,
	}

	originalReq := httptest.NewRequest(
		http.MethodPost,
		"/v1/responses/compact?include=usage&reasoning=summary",
		nil,
	)
	originalReq.Header.Set("Content-Type", "application/json")

	originalBody := []byte(`{"model":"original-model","input":"hello"}`)

	status, respBody, respHeaders, streamResp, err := handler.forwardRequest(
		upstream,
		"mapped-model",
		originalBody,
		false,
		originalReq,
	)
	if err != nil {
		t.Fatalf("forwardRequest() error = %v", err)
	}
	if status != http.StatusOK {
		t.Fatalf("status = %d, want %d", status, http.StatusOK)
	}
	if streamResp != nil {
		t.Fatal("streamResp != nil, want nil for non-streaming request")
	}
	if gotURL != "https://api.example.com/v1/responses/compact?include=usage&reasoning=summary" {
		t.Fatalf("upstream URL = %q", gotURL)
	}
	if gotBody["model"] != "mapped-model" {
		t.Fatalf("forwarded model = %v, want mapped-model", gotBody["model"])
	}
	if string(respBody) != `{"id":"resp_123"}` {
		t.Fatalf("response body = %q", string(respBody))
	}
	if respHeaders.Get("Content-Type") != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", respHeaders.Get("Content-Type"))
	}
}

func TestResponsesRouteReachability(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 1,
		Upstreams: []config.Upstream{
			{
				Name:            "test-upstream",
				BaseURL:         "https://api.example.com",
				Tokens:          config.TokenList{"sk-test"},
				APIType:         config.APITypeResponses,
				AvailableModels: config.AvailableModelList{"gpt-4o"},
			},
		},
	}

	bal := balancer.NewWeightedRoundRobin(cfg.Upstreams)
	handler := NewResponsesHandler(cfg, bal)

	// Build a ServeMux identical to main.go route registration
	mux := http.NewServeMux()
	mux.Handle("POST /v1/responses", handler)
	mux.Handle("POST /v1/responses/compact", handler)

	tests := []struct {
		name       string
		path       string
		wantStatus int // exact match for 404; "not 404" for reachable routes
		exact404   bool
	}{
		{
			name:       "POST /v1/responses reaches handler",
			path:       "/v1/responses",
			wantStatus: http.StatusBadRequest, // handler returns model_not_found for unknown model
			exact404:   false,
		},
		{
			name:       "POST /v1/responses/compact reaches handler",
			path:       "/v1/responses/compact",
			wantStatus: http.StatusBadRequest,
			exact404:   false,
		},
		{
			name:       "POST /v1/responses/v1/messages returns 404",
			path:       "/v1/responses/v1/messages",
			wantStatus: http.StatusNotFound,
			exact404:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"model":"nonexistent-model","input":"hello"}`
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			mux.ServeHTTP(rec, req)

			if tc.exact404 {
				if rec.Code != http.StatusNotFound {
					t.Fatalf("status = %d, want %d (route should NOT be reachable)", rec.Code, http.StatusNotFound)
				}
			} else {
				if rec.Code == http.StatusNotFound {
					t.Fatalf("status = 404, route should be reachable (expected handler to process request)")
				}
				if rec.Code != tc.wantStatus {
					t.Fatalf("status = %d, want %d", rec.Code, tc.wantStatus)
				}
			}
		})
	}
}
