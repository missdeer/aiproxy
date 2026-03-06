package proxy

import (
	"fmt"
	"strings"
	"testing"
)

func TestAntigravityApplySystemInstruction_InstructionsConversion(t *testing.T) {
	payload := map[string]any{
		"request": map[string]any{
			"contents": []map[string]any{},
		},
	}
	bodyMap := map[string]any{
		"instructions": "Be helpful and concise",
	}

	antigravityApplySystemInstruction(payload, "gemini-2.5-flash", bodyMap)

	reqMap := payload["request"].(map[string]any)
	sysInst, ok := reqMap["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatal("systemInstruction not set after instructions conversion")
	}
	parts, ok := sysInst["parts"].([]map[string]any)
	if !ok || len(parts) != 1 {
		t.Fatalf("expected 1 part, got %d", len(parts))
	}
	if parts[0]["text"] != "Be helpful and concise" {
		t.Fatalf("part text = %q, want %q", parts[0]["text"], "Be helpful and concise")
	}
}

func TestAntigravityApplySystemInstruction_NoInjectionForNonQualifyingModels(t *testing.T) {
	models := []string{"gemini-2.5-flash", "gemini-2.5-pro", "gemini-3-flash", "gpt-oss-120b-medium"}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			payload := map[string]any{
				"request": map[string]any{
					"contents": []map[string]any{},
				},
			}
			bodyMap := map[string]any{}

			antigravityApplySystemInstruction(payload, model, bodyMap)

			reqMap := payload["request"].(map[string]any)
			if _, ok := reqMap["systemInstruction"]; ok {
				t.Fatalf("systemInstruction should NOT be set for model %q", model)
			}
		})
	}
}

func TestAntigravityApplySystemInstruction_InjectionForQualifyingModels(t *testing.T) {
	models := []string{
		"claude-sonnet-4-5",
		"claude-opus-4-5-thinking",
		"Claude-Sonnet-4-6", // case-insensitive for claude
		"gemini-3-pro-high",
	}

	for _, model := range models {
		t.Run(model, func(t *testing.T) {
			payload := map[string]any{
				"request": map[string]any{
					"contents": []map[string]any{},
				},
			}
			bodyMap := map[string]any{}

			antigravityApplySystemInstruction(payload, model, bodyMap)

			reqMap := payload["request"].(map[string]any)
			sysInst, ok := reqMap["systemInstruction"].(map[string]any)
			if !ok {
				t.Fatalf("systemInstruction should be set for model %q", model)
			}
			parts, ok := sysInst["parts"].([]map[string]any)
			if !ok || len(parts) != 2 {
				t.Fatalf("expected 2 default parts, got %d", len(parts))
			}
			// First part: the system instruction
			if parts[0]["text"] != antigravitySystemInstruction {
				t.Fatalf("first part should be the system instruction constant")
			}
			// Second part: the [ignore] wrapper
			expected := fmt.Sprintf("Please ignore following [ignore]%s[/ignore]", antigravitySystemInstruction)
			if parts[1]["text"] != expected {
				t.Fatalf("second part should be the [ignore] wrapper")
			}
		})
	}
}

func TestAntigravityApplySystemInstruction_PreservesExistingParts(t *testing.T) {
	payload := map[string]any{
		"request": map[string]any{
			"contents": []map[string]any{},
		},
	}
	bodyMap := map[string]any{
		"instructions": "User's custom instruction",
	}

	// Use a qualifying model so both instructions conversion AND default injection happen
	antigravityApplySystemInstruction(payload, "claude-sonnet-4-5", bodyMap)

	reqMap := payload["request"].(map[string]any)
	sysInst, ok := reqMap["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatal("systemInstruction not set")
	}
	parts, ok := sysInst["parts"].([]map[string]any)
	if !ok {
		t.Fatal("parts not found")
	}

	// Should have 3 parts: 2 defaults + 1 preserved
	if len(parts) != 3 {
		t.Fatalf("expected 3 parts (2 defaults + 1 existing), got %d", len(parts))
	}

	// First two are defaults
	if parts[0]["text"] != antigravitySystemInstruction {
		t.Fatal("first part should be the system instruction constant")
	}
	if !strings.Contains(parts[1]["text"].(string), "[ignore]") {
		t.Fatal("second part should be the [ignore] wrapper")
	}

	// Third is the preserved user instruction
	if parts[2]["text"] != "User's custom instruction" {
		t.Fatalf("third part = %q, want %q", parts[2]["text"], "User's custom instruction")
	}
}

func TestAntigravityApplySystemInstruction_NonQualifyingModelWithInstructions(t *testing.T) {
	// Non-qualifying model should still convert instructions, but NOT inject defaults
	payload := map[string]any{
		"request": map[string]any{
			"contents": []map[string]any{},
		},
	}
	bodyMap := map[string]any{
		"instructions": "Custom only",
	}

	antigravityApplySystemInstruction(payload, "gemini-2.5-flash", bodyMap)

	reqMap := payload["request"].(map[string]any)
	sysInst, ok := reqMap["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatal("systemInstruction should be set from instructions field")
	}
	parts := sysInst["parts"].([]map[string]any)
	if len(parts) != 1 {
		t.Fatalf("expected 1 part (no defaults), got %d", len(parts))
	}
	if parts[0]["text"] != "Custom only" {
		t.Fatalf("part text = %q, want %q", parts[0]["text"], "Custom only")
	}
}
