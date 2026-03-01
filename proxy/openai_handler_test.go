package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/missdeer/aiproxy/config"
)

// ── extractOpenAIPromptPreview tests ───────────────────────────────────

func TestExtractOpenAIPromptPreview(t *testing.T) {
	tests := []struct {
		name     string
		messages []OpenAIMessage
		want     string
	}{
		{
			name:     "empty messages",
			messages: nil,
			want:     "(empty)",
		},
		{
			name: "string content",
			messages: []OpenAIMessage{
				{Role: "user", Content: "Hello there"},
			},
			want: "Hello there",
		},
		{
			name: "takes last message",
			messages: []OpenAIMessage{
				{Role: "user", Content: "First"},
				{Role: "assistant", Content: "Reply"},
				{Role: "user", Content: "Second"},
			},
			want: "Second",
		},
		{
			name: "array content with text block",
			messages: []OpenAIMessage{
				{Role: "user", Content: []any{
					map[string]any{"type": "text", "text": "What is AI?"},
				}},
			},
			want: "What is AI?",
		},
		{
			name: "non-text content",
			messages: []OpenAIMessage{
				{Role: "user", Content: 42},
			},
			want: "(unknown format)",
		},
		{
			name: "long string is truncated",
			messages: []OpenAIMessage{
				{Role: "user", Content: strings.Repeat("b", 200)},
			},
			want: strings.Repeat("b", 100) + "...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractOpenAIPromptPreview(tt.messages)
			if got != tt.want {
				t.Fatalf("extractOpenAIPromptPreview() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── mapAnthropicStopReason tests ───────────────────────────────────────

func TestMapAnthropicStopReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"end_turn", "stop"},
		{"max_tokens", "length"},
		{"stop_sequence", "stop"},
		{"tool_use", "tool_calls"},
		{"unknown_reason", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapAnthropicStopReason(tt.input)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ── convertAnthropicToOpenAIResponse tests ─────────────────────────────

func TestConvertAnthropicToOpenAIResponse(t *testing.T) {
	t.Run("basic response conversion", func(t *testing.T) {
		anthropicResp := map[string]any{
			"id":    "msg-abc",
			"type":  "message",
			"role":  "assistant",
			"model": "claude-3-opus",
			"content": []any{
				map[string]any{"type": "text", "text": "Hello from Claude"},
			},
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  100,
				"output_tokens": 50,
			},
		}
		body, _ := json.Marshal(anthropicResp)

		result, err := convertAnthropicToOpenAIResponse(body)
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var openAIResp OpenAIChatResponse
		json.Unmarshal(result, &openAIResp)

		if !strings.HasPrefix(openAIResp.ID, "chatcmpl-") {
			t.Fatalf("ID = %q, want prefix 'chatcmpl-'", openAIResp.ID)
		}
		if openAIResp.Object != "chat.completion" {
			t.Fatalf("Object = %q, want %q", openAIResp.Object, "chat.completion")
		}
		if openAIResp.Model != "claude-3-opus" {
			t.Fatalf("Model = %q, want %q", openAIResp.Model, "claude-3-opus")
		}
		if len(openAIResp.Choices) != 1 {
			t.Fatalf("choices = %d, want 1", len(openAIResp.Choices))
		}
		if openAIResp.Choices[0].Message.Content != "Hello from Claude" {
			t.Fatalf("content = %v, want %q", openAIResp.Choices[0].Message.Content, "Hello from Claude")
		}
		if openAIResp.Choices[0].FinishReason == nil || *openAIResp.Choices[0].FinishReason != "stop" {
			t.Fatalf("finish_reason = %v, want %q", openAIResp.Choices[0].FinishReason, "stop")
		}
		if openAIResp.Usage.PromptTokens != 100 {
			t.Fatalf("prompt_tokens = %d, want 100", openAIResp.Usage.PromptTokens)
		}
		if openAIResp.Usage.CompletionTokens != 50 {
			t.Fatalf("completion_tokens = %d, want 50", openAIResp.Usage.CompletionTokens)
		}
		if openAIResp.Usage.TotalTokens != 150 {
			t.Fatalf("total_tokens = %d, want 150", openAIResp.Usage.TotalTokens)
		}
	})

	t.Run("multiple text blocks concatenated", func(t *testing.T) {
		anthropicResp := map[string]any{
			"id":    "msg-multi",
			"model": "claude-3",
			"content": []any{
				map[string]any{"type": "text", "text": "Part 1. "},
				map[string]any{"type": "text", "text": "Part 2."},
			},
			"stop_reason": "end_turn",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 5},
		}
		body, _ := json.Marshal(anthropicResp)

		result, err := convertAnthropicToOpenAIResponse(body)
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var openAIResp OpenAIChatResponse
		json.Unmarshal(result, &openAIResp)
		if openAIResp.Choices[0].Message.Content != "Part 1. Part 2." {
			t.Fatalf("content = %v, want %q", openAIResp.Choices[0].Message.Content, "Part 1. Part 2.")
		}
	})

	t.Run("max_tokens mapped to length", func(t *testing.T) {
		anthropicResp := map[string]any{
			"id":          "msg-max",
			"model":       "claude-3",
			"content":     []any{map[string]any{"type": "text", "text": "..."}},
			"stop_reason": "max_tokens",
			"usage":       map[string]any{"input_tokens": 10, "output_tokens": 100},
		}
		body, _ := json.Marshal(anthropicResp)

		result, _ := convertAnthropicToOpenAIResponse(body)
		var openAIResp OpenAIChatResponse
		json.Unmarshal(result, &openAIResp)

		if openAIResp.Choices[0].FinishReason == nil || *openAIResp.Choices[0].FinishReason != "length" {
			t.Fatalf("finish_reason = %v, want %q", openAIResp.Choices[0].FinishReason, "length")
		}
	})

	t.Run("invalid JSON returns error", func(t *testing.T) {
		_, err := convertAnthropicToOpenAIResponse([]byte("not json"))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}

func TestOpenAIHandler_NoSupportedModel_NoFallback_ReturnsModelNotFound(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 1,
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Token:           "sk-test",
				APIType:         config.APITypeOpenAI,
				AvailableModels: []string{"gpt-4o"},
			},
		},
	}

	h := NewOpenAIHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-5",
		"messages":[{"role":"user","content":"hello"}]
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

func TestOpenAIHandler_ModelFallback_Success(t *testing.T) {
	modelCh := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(raw, &req)
		if m, ok := req["model"].(string); ok {
			select {
			case modelCh <- m:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	cfg := &config.Config{
		UpstreamRequestTimeout: 5,
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         upstream.URL,
				Token:           "sk-test",
				APIType:         config.APITypeOpenAI,
				AvailableModels: []string{"gpt-4o"},
			},
		},
		ModelFallback: map[string]string{
			"gpt-5": "gpt-4o",
		},
	}

	h := NewOpenAIHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	select {
	case got := <-modelCh:
		if got != "gpt-4o" {
			t.Fatalf("forwarded model = %q, want %q", got, "gpt-4o")
		}
	default:
		t.Fatal("upstream did not receive request")
	}
}

func TestOpenAIHandler_ModelFallback_CycleTerminates(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 1,
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Token:           "sk-test",
				APIType:         config.APITypeOpenAI,
				AvailableModels: []string{"gpt-4o"},
			},
		},
		ModelFallback: map[string]string{
			"gpt-5":   "gpt-4.1",
			"gpt-4.1": "gpt-5",
		},
	}

	h := NewOpenAIHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`))
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("ServeHTTP did not return; possible fallback loop")
	}
}
