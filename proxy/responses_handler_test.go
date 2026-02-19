package proxy

import (
	"reflect"
	"testing"
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
