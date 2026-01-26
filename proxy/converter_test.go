package proxy

import (
	"encoding/json"
	"testing"

	"github.com/missdeer/aiproxy/config"
)

func TestConvertOpenAIChatToAnthropicRequest(t *testing.T) {
	tests := []struct {
		name             string
		input            map[string]any
		defaultMaxTokens []int
		wantMaxTokens    int
		wantSystem       string
		wantMsgCount     int
	}{
		{
			name: "basic conversion with max_tokens",
			input: map[string]any{
				"model":      "gpt-4",
				"max_tokens": 1000,
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 1000,
			wantMsgCount:  1,
		},
		{
			name: "default max_tokens when not specified",
			input: map[string]any{
				"model": "gpt-4",
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 4096,
			wantMsgCount:  1,
		},
		{
			name: "custom default max_tokens",
			input: map[string]any{
				"model": "gpt-4",
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			defaultMaxTokens: []int{8192},
			wantMaxTokens:    8192,
			wantMsgCount:     1,
		},
		{
			name: "system message extraction",
			input: map[string]any{
				"model": "gpt-4",
				"messages": []any{
					map[string]any{"role": "system", "content": "You are helpful"},
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 4096,
			wantSystem:    "You are helpful",
			wantMsgCount:  1, // system message is extracted, not in messages
		},
		{
			name: "temperature and top_p",
			input: map[string]any{
				"model":       "gpt-4",
				"temperature": 0.7,
				"top_p":       0.9,
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMaxTokens: 4096,
			wantMsgCount:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertOpenAIChatToAnthropicRequest(tt.input, tt.defaultMaxTokens...)

			// Check max_tokens
			if maxTokens, ok := result["max_tokens"].(int); ok {
				if maxTokens != tt.wantMaxTokens {
					t.Errorf("max_tokens = %d, want %d", maxTokens, tt.wantMaxTokens)
				}
			} else {
				t.Error("max_tokens not found or not an int")
			}

			// Check system
			if tt.wantSystem != "" {
				if system, ok := result["system"].(string); ok {
					if system != tt.wantSystem {
						t.Errorf("system = %q, want %q", system, tt.wantSystem)
					}
				} else {
					t.Error("system not found")
				}
			}

			// Check messages count
			if messages, ok := result["messages"].([]map[string]any); ok {
				if len(messages) != tt.wantMsgCount {
					t.Errorf("messages count = %d, want %d", len(messages), tt.wantMsgCount)
				}
			}
		})
	}
}

func TestConvertAnthropicToOpenAIRequest(t *testing.T) {
	tests := []struct {
		name          string
		input         map[string]any
		wantMsgCount  int
		wantHasSystem bool
	}{
		{
			name: "basic conversion",
			input: map[string]any{
				"model":      "claude-3",
				"max_tokens": 1000,
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMsgCount: 1,
		},
		{
			name: "with system message",
			input: map[string]any{
				"model":      "claude-3",
				"max_tokens": 1000,
				"system":     "You are helpful",
				"messages": []any{
					map[string]any{"role": "user", "content": "Hello"},
				},
			},
			wantMsgCount:  2, // system + user
			wantHasSystem: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := convertAnthropicToOpenAIRequest(tt.input)

			if messages, ok := result["messages"].([]map[string]any); ok {
				if len(messages) != tt.wantMsgCount {
					t.Errorf("messages count = %d, want %d", len(messages), tt.wantMsgCount)
				}

				if tt.wantHasSystem && len(messages) > 0 {
					if messages[0]["role"] != "system" {
						t.Error("expected first message to be system")
					}
				}
			}
		})
	}
}

func TestConvertAnthropicToGeminiRequest(t *testing.T) {
	input := map[string]any{
		"model":      "claude-3",
		"max_tokens": 1000,
		"system":     "You are helpful",
		"messages": []any{
			map[string]any{"role": "user", "content": "Hello"},
			map[string]any{"role": "assistant", "content": "Hi there"},
		},
	}

	result := convertAnthropicToGeminiRequest(input)

	// Check contents
	if contents, ok := result["contents"].([]map[string]any); ok {
		if len(contents) != 2 {
			t.Errorf("contents count = %d, want 2", len(contents))
		}
		if contents[0]["role"] != "user" {
			t.Errorf("first role = %v, want user", contents[0]["role"])
		}
		if contents[1]["role"] != "model" {
			t.Errorf("second role = %v, want model", contents[1]["role"])
		}
	} else {
		t.Error("contents not found")
	}

	// Check systemInstruction
	if sysInstr, ok := result["systemInstruction"].(map[string]any); ok {
		if parts, ok := sysInstr["parts"].([]map[string]any); ok {
			if parts[0]["text"] != "You are helpful" {
				t.Error("system instruction text mismatch")
			}
		}
	} else {
		t.Error("systemInstruction not found")
	}

	// Check generationConfig
	if genConfig, ok := result["generationConfig"].(map[string]any); ok {
		if genConfig["maxOutputTokens"] != 1000 {
			t.Errorf("maxOutputTokens = %v, want 1000", genConfig["maxOutputTokens"])
		}
	} else {
		t.Error("generationConfig not found")
	}
}

func TestMapOpenAIStopReasonToAnthropic(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"stop", "end_turn"},
		{"length", "max_tokens"},
		{"tool_calls", "tool_use"},
		{"unknown", "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapOpenAIStopReasonToAnthropic(tt.input); got != tt.want {
				t.Errorf("mapOpenAIStopReasonToAnthropic(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapGeminiStopReasonToAnthropic(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"STOP", "end_turn"},
		{"MAX_TOKENS", "max_tokens"},
		{"UNKNOWN", "end_turn"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapGeminiStopReasonToAnthropic(tt.input); got != tt.want {
				t.Errorf("mapGeminiStopReasonToAnthropic(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMapGeminiToOpenAIFinishReason(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"STOP", "stop"},
		{"MAX_TOKENS", "length"},
		{"UNKNOWN", "stop"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := mapGeminiToOpenAIFinishReason(tt.input); got != tt.want {
				t.Errorf("mapGeminiToOpenAIFinishReason(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestConvertOpenAIResponseToAnthropic(t *testing.T) {
	openAIResp := OpenAIChatResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1234567890,
		Model:   "gpt-4",
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:    "assistant",
					Content: "Hello, how can I help?",
				},
				FinishReason: strPtr("stop"),
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 20,
			TotalTokens:      30,
		},
	}

	body, _ := json.Marshal(openAIResp)
	result, err := convertOpenAIResponseToAnthropic(body)
	if err != nil {
		t.Fatalf("convertOpenAIResponseToAnthropic() error = %v", err)
	}

	var anthropicResp map[string]any
	if err := json.Unmarshal(result, &anthropicResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if anthropicResp["type"] != "message" {
		t.Errorf("type = %v, want message", anthropicResp["type"])
	}
	if anthropicResp["role"] != "assistant" {
		t.Errorf("role = %v, want assistant", anthropicResp["role"])
	}
	if anthropicResp["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason = %v, want end_turn", anthropicResp["stop_reason"])
	}
}

func TestConvertResponseToOpenAI(t *testing.T) {
	// Test Gemini response conversion
	geminiResp := GeminiResponse{
		Candidates: []GeminiCandidate{
			{
				Content: GeminiContent{
					Parts: []GeminiPart{{Text: "Hello from Gemini"}},
					Role:  "model",
				},
				FinishReason: "STOP",
			},
		},
		UsageMetadata: &GeminiUsage{
			PromptTokenCount:     10,
			CandidatesTokenCount: 20,
		},
		ModelVersion: "gemini-1.5-pro",
	}

	body, _ := json.Marshal(geminiResp)
	result, err := convertResponseToOpenAI(body, config.APITypeGemini)
	if err != nil {
		t.Fatalf("convertResponseToOpenAI() error = %v", err)
	}

	var openAIResp OpenAIChatResponse
	if err := json.Unmarshal(result, &openAIResp); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if openAIResp.Object != "chat.completion" {
		t.Errorf("object = %v, want chat.completion", openAIResp.Object)
	}
	if len(openAIResp.Choices) != 1 {
		t.Errorf("choices count = %d, want 1", len(openAIResp.Choices))
	}
	if openAIResp.Choices[0].Message.Content != "Hello from Gemini" {
		t.Errorf("content = %v, want 'Hello from Gemini'", openAIResp.Choices[0].Message.Content)
	}
}

func strPtr(s string) *string {
	return &s
}
