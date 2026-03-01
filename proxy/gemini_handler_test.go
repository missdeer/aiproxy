package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/missdeer/aiproxy/config"
)

// ── parseGeminiPath tests ──────────────────────────────────────────────

func TestParseGeminiPath(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		wantModel  string
		wantStream bool
	}{
		{
			name:       "v1beta generateContent",
			path:       "/v1beta/models/gemini-pro:generateContent",
			wantModel:  "gemini-pro",
			wantStream: false,
		},
		{
			name:       "v1beta streamGenerateContent",
			path:       "/v1beta/models/gemini-pro:streamGenerateContent",
			wantModel:  "gemini-pro",
			wantStream: true,
		},
		{
			name:       "v1 generateContent",
			path:       "/v1/models/gemini-2.5-flash:generateContent",
			wantModel:  "gemini-2.5-flash",
			wantStream: false,
		},
		{
			name:       "v1 streamGenerateContent",
			path:       "/v1/models/gemini-2.5-flash:streamGenerateContent",
			wantModel:  "gemini-2.5-flash",
			wantStream: true,
		},
		{
			name:       "invalid path - no action",
			path:       "/v1beta/models/gemini-pro",
			wantModel:  "",
			wantStream: false,
		},
		{
			name:       "empty path",
			path:       "",
			wantModel:  "",
			wantStream: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			model, isStream := parseGeminiPath(tt.path)
			if model != tt.wantModel {
				t.Fatalf("model = %q, want %q", model, tt.wantModel)
			}
			if isStream != tt.wantStream {
				t.Fatalf("isStream = %v, want %v", isStream, tt.wantStream)
			}
		})
	}
}

// ── extractGeminiPromptPreview tests ───────────────────────────────────

func TestExtractGeminiPromptPreview(t *testing.T) {
	tests := []struct {
		name     string
		contents []GeminiContent
		want     string
	}{
		{
			name:     "empty contents",
			contents: nil,
			want:     "(empty)",
		},
		{
			name: "single user message",
			contents: []GeminiContent{
				{Role: "user", Parts: []GeminiPart{{Text: "Hello world"}}},
			},
			want: "Hello world",
		},
		{
			name: "takes last user message",
			contents: []GeminiContent{
				{Role: "user", Parts: []GeminiPart{{Text: "First"}}},
				{Role: "model", Parts: []GeminiPart{{Text: "Response"}}},
				{Role: "user", Parts: []GeminiPart{{Text: "Second"}}},
			},
			want: "Second",
		},
		{
			name: "skips model-only tail",
			contents: []GeminiContent{
				{Role: "user", Parts: []GeminiPart{{Text: "Before model"}}},
				{Role: "model", Parts: []GeminiPart{{Text: "Model response"}}},
			},
			want: "Before model",
		},
		{
			name: "empty role treated as user",
			contents: []GeminiContent{
				{Role: "", Parts: []GeminiPart{{Text: "No role set"}}},
			},
			want: "No role set",
		},
		{
			name: "no text parts",
			contents: []GeminiContent{
				{Role: "user", Parts: []GeminiPart{{InlineData: &GeminiInlineData{MimeType: "image/png", Data: "abc"}}}},
			},
			want: "(empty)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractGeminiPromptPreview(tt.contents)
			if got != tt.want {
				t.Fatalf("extractGeminiPromptPreview() = %q, want %q", got, tt.want)
			}
		})
	}
}

// ── mapAnthropicToGeminiFinishReason tests ─────────────────────────────

func TestMapAnthropicToGeminiFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"end_turn", "STOP"},
		{"max_tokens", "MAX_TOKENS"},
		{"stop_sequence", "STOP"},
		{"unknown_reason", "STOP"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapAnthropicToGeminiFinishReason(tt.input)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ── mapOpenAIToGeminiFinishReason tests ────────────────────────────────

func TestMapOpenAIToGeminiFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "STOP"},
		{"length", "MAX_TOKENS"},
		{"tool_calls", "STOP"},
		{"unknown", "STOP"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := mapOpenAIToGeminiFinishReason(tt.input)
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

// ── convertGeminiToAnthropicRequest tests ──────────────────────────────

func TestConvertGeminiToAnthropicRequest(t *testing.T) {
	t.Run("basic conversion with single text part", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role": "user",
					"parts": []any{
						map[string]any{"text": "Hello"},
					},
				},
			},
		}

		result := convertGeminiToAnthropicRequest(req, 4096)

		messages := result["messages"].([]map[string]any)
		if len(messages) != 1 {
			t.Fatalf("messages length = %d, want 1", len(messages))
		}
		if messages[0]["role"] != "user" {
			t.Fatalf("role = %q, want %q", messages[0]["role"], "user")
		}
		// Single text part should be simplified to string
		if messages[0]["content"] != "Hello" {
			t.Fatalf("content = %v, want %q", messages[0]["content"], "Hello")
		}
		if result["max_tokens"] != 4096 {
			t.Fatalf("max_tokens = %v, want 4096", result["max_tokens"])
		}
	})

	t.Run("model role mapped to assistant", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role": "model",
					"parts": []any{
						map[string]any{"text": "I can help"},
					},
				},
			},
		}

		result := convertGeminiToAnthropicRequest(req, 4096)
		messages := result["messages"].([]map[string]any)
		if messages[0]["role"] != "assistant" {
			t.Fatalf("role = %q, want %q", messages[0]["role"], "assistant")
		}
	})

	t.Run("system instruction extracted", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Hi"}},
				},
			},
			"systemInstruction": map[string]any{
				"parts": []any{
					map[string]any{"text": "You are a helpful assistant."},
				},
			},
		}

		result := convertGeminiToAnthropicRequest(req, 4096)
		if result["system"] != "You are a helpful assistant." {
			t.Fatalf("system = %q, want %q", result["system"], "You are a helpful assistant.")
		}
	})

	t.Run("generation config mapped", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Hi"}},
				},
			},
			"generationConfig": map[string]any{
				"temperature":     0.7,
				"topP":            0.9,
				"maxOutputTokens": 2048,
				"stopSequences":   []string{"END"},
			},
		}

		result := convertGeminiToAnthropicRequest(req, 4096)
		if result["temperature"] != 0.7 {
			t.Fatalf("temperature = %v, want 0.7", result["temperature"])
		}
		if result["top_p"] != 0.9 {
			t.Fatalf("top_p = %v, want 0.9", result["top_p"])
		}
		// maxOutputTokens is passed directly as int from input map, not JSON-round-tripped
		if v, ok := result["max_tokens"].(int); ok {
			if v != 2048 {
				t.Fatalf("max_tokens = %v, want 2048", v)
			}
		} else if v, ok := result["max_tokens"].(float64); ok {
			if v != 2048 {
				t.Fatalf("max_tokens = %v, want 2048", v)
			}
		} else {
			t.Fatalf("max_tokens unexpected type %T = %v", result["max_tokens"], result["max_tokens"])
		}
	})

	t.Run("default max_tokens when no generationConfig", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Hi"}},
				},
			},
		}

		result := convertGeminiToAnthropicRequest(req, 8192)
		if result["max_tokens"] != 8192 {
			t.Fatalf("max_tokens = %v, want 8192", result["max_tokens"])
		}
	})
}

// ── convertGeminiToOpenAIRequest tests ─────────────────────────────────

func TestConvertGeminiToOpenAIRequest(t *testing.T) {
	t.Run("basic conversion", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role": "user",
					"parts": []any{
						map[string]any{"text": "Hello"},
					},
				},
			},
		}

		result := convertGeminiToOpenAIRequest(req)
		messages := result["messages"].([]map[string]any)
		if len(messages) != 1 {
			t.Fatalf("messages length = %d, want 1", len(messages))
		}
		if messages[0]["role"] != "user" {
			t.Fatalf("role = %q, want %q", messages[0]["role"], "user")
		}
		if messages[0]["content"] != "Hello" {
			t.Fatalf("content = %v, want %q", messages[0]["content"], "Hello")
		}
	})

	t.Run("system instruction becomes system message", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Hi"}},
				},
			},
			"systemInstruction": map[string]any{
				"parts": []any{
					map[string]any{"text": "Be helpful"},
				},
			},
		}

		result := convertGeminiToOpenAIRequest(req)
		messages := result["messages"].([]map[string]any)
		// First message should be the system message
		if messages[0]["role"] != "system" {
			t.Fatalf("first role = %q, want %q", messages[0]["role"], "system")
		}
		if messages[0]["content"] != "Be helpful" {
			t.Fatalf("system content = %v, want %q", messages[0]["content"], "Be helpful")
		}
	})

	t.Run("model role mapped to assistant", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "model",
					"parts": []any{map[string]any{"text": "Reply"}},
				},
			},
		}

		result := convertGeminiToOpenAIRequest(req)
		messages := result["messages"].([]map[string]any)
		if messages[0]["role"] != "assistant" {
			t.Fatalf("role = %q, want %q", messages[0]["role"], "assistant")
		}
	})

	t.Run("generation config mapped", func(t *testing.T) {
		req := map[string]any{
			"contents": []any{
				map[string]any{
					"role":  "user",
					"parts": []any{map[string]any{"text": "Hi"}},
				},
			},
			"generationConfig": map[string]any{
				"temperature":     0.5,
				"topP":            0.8,
				"maxOutputTokens": 1024,
				"stopSequences":   []string{"STOP"},
			},
		}

		result := convertGeminiToOpenAIRequest(req)
		if result["temperature"] != 0.5 {
			t.Fatalf("temperature = %v, want 0.5", result["temperature"])
		}
		if result["top_p"] != 0.8 {
			t.Fatalf("top_p = %v, want 0.8", result["top_p"])
		}
		if v, ok := result["max_tokens"].(int); ok {
			if v != 1024 {
				t.Fatalf("max_tokens = %v, want 1024", v)
			}
		} else if v, ok := result["max_tokens"].(float64); ok {
			if v != 1024 {
				t.Fatalf("max_tokens = %v, want 1024", v)
			}
		} else {
			t.Fatalf("max_tokens unexpected type %T = %v", result["max_tokens"], result["max_tokens"])
		}
	})
}

// ── convertAnthropicToGemini tests ─────────────────────────────────────

func TestConvertAnthropicToGemini(t *testing.T) {
	t.Run("basic response conversion", func(t *testing.T) {
		anthropicResp := map[string]any{
			"id":    "msg-abc",
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

		result, err := convertAnthropicToGemini(body)
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var geminiResp GeminiResponse
		if err := json.Unmarshal(result, &geminiResp); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		if len(geminiResp.Candidates) != 1 {
			t.Fatalf("candidates = %d, want 1", len(geminiResp.Candidates))
		}
		if geminiResp.Candidates[0].Content.Role != "model" {
			t.Fatalf("role = %q, want %q", geminiResp.Candidates[0].Content.Role, "model")
		}
		if len(geminiResp.Candidates[0].Content.Parts) != 1 {
			t.Fatalf("parts = %d, want 1", len(geminiResp.Candidates[0].Content.Parts))
		}
		if geminiResp.Candidates[0].Content.Parts[0].Text != "Hello from Claude" {
			t.Fatalf("text = %q, want %q", geminiResp.Candidates[0].Content.Parts[0].Text, "Hello from Claude")
		}
		if geminiResp.Candidates[0].FinishReason != "STOP" {
			t.Fatalf("finishReason = %q, want %q", geminiResp.Candidates[0].FinishReason, "STOP")
		}
		if geminiResp.UsageMetadata.PromptTokenCount != 100 {
			t.Fatalf("promptTokenCount = %d, want 100", geminiResp.UsageMetadata.PromptTokenCount)
		}
		if geminiResp.UsageMetadata.CandidatesTokenCount != 50 {
			t.Fatalf("candidatesTokenCount = %d, want 50", geminiResp.UsageMetadata.CandidatesTokenCount)
		}
		if geminiResp.UsageMetadata.TotalTokenCount != 150 {
			t.Fatalf("totalTokenCount = %d, want 150", geminiResp.UsageMetadata.TotalTokenCount)
		}
		if geminiResp.ModelVersion != "claude-3-opus" {
			t.Fatalf("modelVersion = %q, want %q", geminiResp.ModelVersion, "claude-3-opus")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, err := convertAnthropicToGemini([]byte("not json"))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}

// ── convertOpenAIToGemini tests ────────────────────────────────────────

func TestConvertOpenAIToGemini(t *testing.T) {
	t.Run("basic response conversion", func(t *testing.T) {
		stop := "stop"
		openAIResp := OpenAIChatResponse{
			ID:      "chatcmpl-abc",
			Model:   "gpt-4",
			Object:  "chat.completion",
			Created: 1234567890,
			Choices: []OpenAIChoice{
				{
					Index:        0,
					Message:      &OpenAIMessage{Role: "assistant", Content: "Hello from GPT"},
					FinishReason: &stop,
				},
			},
			Usage: OpenAIUsage{
				PromptTokens:     20,
				CompletionTokens: 10,
				TotalTokens:      30,
			},
		}
		body, _ := json.Marshal(openAIResp)

		result, err := convertOpenAIToGemini(body)
		if err != nil {
			t.Fatalf("error = %v", err)
		}

		var geminiResp GeminiResponse
		json.Unmarshal(result, &geminiResp)

		if geminiResp.Candidates[0].Content.Parts[0].Text != "Hello from GPT" {
			t.Fatalf("text = %q, want %q", geminiResp.Candidates[0].Content.Parts[0].Text, "Hello from GPT")
		}
		if geminiResp.Candidates[0].FinishReason != "STOP" {
			t.Fatalf("finishReason = %q, want %q", geminiResp.Candidates[0].FinishReason, "STOP")
		}
		if geminiResp.UsageMetadata.PromptTokenCount != 20 {
			t.Fatalf("promptTokenCount = %d, want 20", geminiResp.UsageMetadata.PromptTokenCount)
		}
		if geminiResp.ModelVersion != "gpt-4" {
			t.Fatalf("modelVersion = %q, want %q", geminiResp.ModelVersion, "gpt-4")
		}
	})

	t.Run("invalid JSON", func(t *testing.T) {
		_, err := convertOpenAIToGemini([]byte("not json"))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}

func TestGeminiHandler_NoSupportedModel_NoFallback_ReturnsModelNotFound(t *testing.T) {
	cfg := &config.Config{
		UpstreamRequestTimeout: 1,
		Upstreams: []config.Upstream{
			{
				Name:            "upstream-1",
				BaseURL:         "https://api.example.com",
				Token:           "sk-test",
				APIType:         config.APITypeGemini,
				AvailableModels: []string{"gemini-2.5-pro"},
			},
		},
	}

	h := NewGeminiHandler(cfg)
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.0-flash:generateContent", strings.NewReader(`{
		"contents":[{"role":"user","parts":[{"text":"hello"}]}]
	}`))
	rec := httptest.NewRecorder()

	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusBadRequest)
	}
	if !strings.Contains(rec.Body.String(), "MODEL_NOT_FOUND") {
		t.Fatalf("body = %q, want MODEL_NOT_FOUND", rec.Body.String())
	}
}
