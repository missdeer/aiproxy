package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/missdeer/aiproxy/config"
)

// ============================================================================
// Helper functions for common operations
// ============================================================================

// copyParam copies a single parameter from src to dst if it exists
func copyParam(src, dst map[string]any, srcKey, dstKey string) {
	if v, ok := src[srcKey]; ok {
		dst[dstKey] = v
	}
}

// copyParams copies multiple parameters from src to dst using same keys
func copyParams(src, dst map[string]any, keys ...string) {
	for _, key := range keys {
		if v, ok := src[key]; ok {
			dst[key] = v
		}
	}
}

// buildGeminiGenerationConfig builds Gemini generationConfig from common parameters
func buildGeminiGenerationConfig(req map[string]any, tempKey, topPKey, maxTokensKey, stopKey string) map[string]any {
	genConfig := make(map[string]any)
	if temp, ok := req[tempKey]; ok {
		genConfig["temperature"] = temp
	}
	if topP, ok := req[topPKey]; ok {
		genConfig["topP"] = topP
	}
	if maxTokens, ok := req[maxTokensKey]; ok {
		genConfig["maxOutputTokens"] = maxTokens
	}
	if stopKey != "" {
		if stop, ok := req[stopKey]; ok {
			genConfig["stopSequences"] = stop
		}
	}
	return genConfig
}

// extractTextFromGeminiParts extracts and concatenates text from Gemini parts array
func extractTextFromGeminiParts(parts []any) string {
	var text strings.Builder
	for _, part := range parts {
		if partMap, ok := part.(map[string]any); ok {
			if t, ok := partMap["text"].(string); ok {
				text.WriteString(t)
			}
		}
	}
	return text.String()
}

// mapRoleToGemini converts OpenAI/Anthropic role to Gemini role
func mapRoleToGemini(role string) string {
	if role == "assistant" {
		return "model"
	}
	return "user"
}

// mapRoleFromGemini converts Gemini role to OpenAI/Anthropic role
func mapRoleFromGemini(role string) string {
	if role == "model" {
		return "assistant"
	}
	return role
}

// appendSSEEvent appends an SSE event to chunks buffer
func appendSSEEvent(chunks []byte, eventType string, data any) []byte {
	eventBytes, _ := json.Marshal(data)
	chunks = append(chunks, []byte("event: "+eventType+"\ndata: ")...)
	chunks = append(chunks, eventBytes...)
	chunks = append(chunks, []byte("\n\n")...)
	return chunks
}

// ============================================================================
// Anthropic -> Other formats
// ============================================================================

// convertAnthropicToOpenAIRequest converts Anthropic messages request to OpenAI chat format
func convertAnthropicToOpenAIRequest(req map[string]any) map[string]any {
	openAIReq := make(map[string]any)

	// Copy model and max_tokens
	copyParam(req, openAIReq, "model", "model")
	copyParam(req, openAIReq, "max_tokens", "max_tokens")

	// Copy optional parameters
	copyParams(req, openAIReq, "temperature", "top_p", "stream")
	copyParam(req, openAIReq, "stop_sequences", "stop")

	var messages []map[string]any

	// Convert system to system message
	if system, ok := req["system"].(string); ok && system != "" {
		messages = append(messages, map[string]any{
			"role":    "system",
			"content": system,
		})
	}

	// Convert messages
	if anthropicMessages, ok := req["messages"].([]any); ok {
		for _, msg := range anthropicMessages {
			if msgMap, ok := msg.(map[string]any); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// Convert content format
				var openAIContent any
				switch c := content.(type) {
				case string:
					openAIContent = c
				case []any:
					var parts []map[string]any
					for _, block := range c {
						if blockMap, ok := block.(map[string]any); ok {
							blockType, _ := blockMap["type"].(string)
							switch blockType {
							case "text":
								parts = append(parts, map[string]any{
									"type": "text",
									"text": blockMap["text"],
								})
							case "image":
								if source, ok := blockMap["source"].(map[string]any); ok {
									mediaType, _ := source["media_type"].(string)
									data, _ := source["data"].(string)
									parts = append(parts, map[string]any{
										"type": "image_url",
										"image_url": map[string]any{
											"url": fmt.Sprintf("data:%s;base64,%s", mediaType, data),
										},
									})
								}
							}
						}
					}
					if len(parts) == 1 && parts[0]["type"] == "text" {
						openAIContent = parts[0]["text"]
					} else {
						openAIContent = parts
					}
				default:
					openAIContent = content
				}

				messages = append(messages, map[string]any{
					"role":    role,
					"content": openAIContent,
				})
			}
		}
	}

	openAIReq["messages"] = messages
	return openAIReq
}

// convertAnthropicToGeminiRequest converts Anthropic messages request to Gemini format
func convertAnthropicToGeminiRequest(req map[string]any) map[string]any {
	geminiReq := make(map[string]any)

	var contents []map[string]any

	// Convert messages
	if anthropicMessages, ok := req["messages"].([]any); ok {
		for _, msg := range anthropicMessages {
			if msgMap, ok := msg.(map[string]any); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				// Map role
				geminiRole := mapRoleToGemini(role)

				var parts []map[string]any
				switch c := content.(type) {
				case string:
					parts = append(parts, map[string]any{"text": c})
				case []any:
					for _, block := range c {
						if blockMap, ok := block.(map[string]any); ok {
							blockType, _ := blockMap["type"].(string)
							switch blockType {
							case "text":
								parts = append(parts, map[string]any{"text": blockMap["text"]})
							case "image":
								if source, ok := blockMap["source"].(map[string]any); ok {
									parts = append(parts, map[string]any{
										"inlineData": map[string]any{
											"mimeType": source["media_type"],
											"data":     source["data"],
										},
									})
								}
							}
						}
					}
				}

				contents = append(contents, map[string]any{
					"role":  geminiRole,
					"parts": parts,
				})
			}
		}
	}

	geminiReq["contents"] = contents

	// Convert system instruction
	if system, ok := req["system"].(string); ok && system != "" {
		geminiReq["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": system}},
		}
	}

	// Convert generation config
	genConfig := buildGeminiGenerationConfig(req, "temperature", "top_p", "max_tokens", "stop_sequences")
	if len(genConfig) > 0 {
		geminiReq["generationConfig"] = genConfig
	}

	return geminiReq
}

// convertAnthropicToResponsesRequest converts Anthropic messages request to OpenAI Responses format
func convertAnthropicToResponsesRequest(req map[string]any) map[string]any {
	responsesReq := make(map[string]any)

	// Copy model
	if model, ok := req["model"]; ok {
		responsesReq["model"] = model
	}

	// Convert max_tokens to max_output_tokens
	copyParam(req, responsesReq, "max_tokens", "max_output_tokens")

	// Copy optional parameters
	copyParams(req, responsesReq, "temperature", "top_p", "stream")

	// Convert system to instructions
	if system, ok := req["system"].(string); ok && system != "" {
		responsesReq["instructions"] = system
	}

	// Convert messages to input
	var input []map[string]any
	if anthropicMessages, ok := req["messages"].([]any); ok {
		for _, msg := range anthropicMessages {
			if msgMap, ok := msg.(map[string]any); ok {
				role, _ := msgMap["role"].(string)
				content := msgMap["content"]

				var contentStr string
				switch c := content.(type) {
				case string:
					contentStr = c
				case []any:
					for _, block := range c {
						if blockMap, ok := block.(map[string]any); ok {
							if text, ok := blockMap["text"].(string); ok {
								contentStr += text
							}
						}
					}
				}

				input = append(input, map[string]any{
					"role":    role,
					"content": contentStr,
				})
			}
		}
	}
	responsesReq["input"] = input

	return responsesReq
}

// ============================================================================
// OpenAI -> Other formats
// ============================================================================

// convertOpenAIChatToAnthropicRequest converts OpenAI chat request to Anthropic format
func convertOpenAIChatToAnthropicRequest(req map[string]any, defaultMaxTokens ...int) map[string]any {
	anthropicReq := make(map[string]any)

	// Copy model
	if model, ok := req["model"]; ok {
		anthropicReq["model"] = model
	}

	// Convert max_tokens
	if maxTokens, ok := req["max_tokens"]; ok {
		anthropicReq["max_tokens"] = maxTokens
	} else {
		maxTokensDefault := 4096
		if len(defaultMaxTokens) > 0 && defaultMaxTokens[0] > 0 {
			maxTokensDefault = defaultMaxTokens[0]
		}
		anthropicReq["max_tokens"] = maxTokensDefault
	}

	// Copy optional parameters
	copyParams(req, anthropicReq, "temperature", "top_p", "stream")
	copyParam(req, anthropicReq, "stop", "stop_sequences")

	// Convert messages
	messages, ok := req["messages"].([]any)
	if !ok {
		return anthropicReq
	}

	var anthropicMessages []map[string]any
	var systemContent string

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}

		role, _ := msgMap["role"].(string)
		content := msgMap["content"]

		// Extract system message
		if role == "system" {
			if s, ok := content.(string); ok {
				if systemContent != "" {
					systemContent += "\n"
				}
				systemContent += s
			}
			continue
		}

		// Convert content format
		var anthropicContent any
		switch c := content.(type) {
		case string:
			anthropicContent = c
		case []any:
			var parts []map[string]any
			for _, part := range c {
				if partMap, ok := part.(map[string]any); ok {
					partType, _ := partMap["type"].(string)
					if partType == "text" {
						parts = append(parts, map[string]any{
							"type": "text",
							"text": partMap["text"],
						})
					} else if partType == "image_url" {
						if imageURL, ok := partMap["image_url"].(map[string]any); ok {
							if url, ok := imageURL["url"].(string); ok {
								if strings.HasPrefix(url, "data:") {
									urlParts := strings.SplitN(url, ",", 2)
									if len(urlParts) == 2 {
										mediaType := strings.TrimPrefix(strings.Split(urlParts[0], ";")[0], "data:")
										parts = append(parts, map[string]any{
											"type": "image",
											"source": map[string]any{
												"type":       "base64",
												"media_type": mediaType,
												"data":       urlParts[1],
											},
										})
									}
								}
							}
						}
					}
				}
			}
			if len(parts) > 0 {
				anthropicContent = parts
			} else {
				anthropicContent = c
			}
		default:
			anthropicContent = content
		}

		anthropicMessages = append(anthropicMessages, map[string]any{
			"role":    role,
			"content": anthropicContent,
		})
	}

	anthropicReq["messages"] = anthropicMessages
	if systemContent != "" {
		anthropicReq["system"] = systemContent
	}

	return anthropicReq
}

// convertOpenAIChatToGeminiRequest converts OpenAI chat request to Gemini format
func convertOpenAIChatToGeminiRequest(req map[string]any) map[string]any {
	// First convert to Anthropic, then to Gemini
	anthropicReq := convertOpenAIChatToAnthropicRequest(req)
	return convertAnthropicToGeminiRequest(anthropicReq)
}

// convertOpenAIChatToResponsesRequest converts OpenAI chat request to Responses format
func convertOpenAIChatToResponsesRequest(req map[string]any) map[string]any {
	responsesReq := make(map[string]any)

	copyParam(req, responsesReq, "model", "model")
	copyParam(req, responsesReq, "max_tokens", "max_output_tokens")
	copyParams(req, responsesReq, "temperature", "top_p", "stream")

	// Convert messages to input
	var input []map[string]any
	if messages, ok := req["messages"].([]any); ok {
		for _, msg := range messages {
			if msgMap, ok := msg.(map[string]any); ok {
				role, _ := msgMap["role"].(string)
				if role == "system" {
					if content, ok := msgMap["content"].(string); ok {
						responsesReq["instructions"] = content
					}
					continue
				}
				input = append(input, map[string]any{
					"role":    role,
					"content": msgMap["content"],
				})
			}
		}
	}
	responsesReq["input"] = input

	return responsesReq
}

// ============================================================================
// Gemini -> Other formats
// ============================================================================

// convertGeminiToAnthropicRequest is defined in gemini_handler.go

// convertGeminiToOpenAIRequest is defined in gemini_handler.go

// convertGeminiToResponsesRequest converts Gemini request to Responses format
func convertGeminiToResponsesRequest(req map[string]any) map[string]any {
	responsesReq := make(map[string]any)

	// Convert system instruction to instructions
	if sysInstr, ok := req["systemInstruction"].(map[string]any); ok {
		if parts, ok := sysInstr["parts"].([]any); ok {
			if systemText := extractTextFromGeminiParts(parts); systemText != "" {
				responsesReq["instructions"] = systemText
			}
		}
	}

	// Convert contents to input
	var input []map[string]any
	if contents, ok := req["contents"].([]any); ok {
		for _, content := range contents {
			if contentMap, ok := content.(map[string]any); ok {
				role := "user"
				if r, ok := contentMap["role"].(string); ok {
					role = mapRoleFromGemini(r)
				}

				var textContent string
				if parts, ok := contentMap["parts"].([]any); ok {
					textContent = extractTextFromGeminiParts(parts)
				}

				input = append(input, map[string]any{
					"role":    role,
					"content": textContent,
				})
			}
		}
	}
	responsesReq["input"] = input

	// Convert generation config
	if genConfig, ok := req["generationConfig"].(map[string]any); ok {
		if temp, ok := genConfig["temperature"]; ok {
			responsesReq["temperature"] = temp
		}
		if topP, ok := genConfig["topP"]; ok {
			responsesReq["top_p"] = topP
		}
		if maxTokens, ok := genConfig["maxOutputTokens"]; ok {
			responsesReq["max_output_tokens"] = maxTokens
		}
	}

	return responsesReq
}

// ============================================================================
// Responses -> Other formats
// ============================================================================

// convertResponsesToAnthropicRequest is defined in responses_handler.go

// convertResponsesToChatRequest is defined in responses_handler.go

// convertResponsesToGeminiRequest converts Responses API request to Gemini format
func convertResponsesToGeminiRequest(req map[string]any) map[string]any {
	geminiReq := make(map[string]any)

	// Convert instructions to systemInstruction
	if instructions, ok := req["instructions"].(string); ok && instructions != "" {
		geminiReq["systemInstruction"] = map[string]any{
			"parts": []map[string]any{{"text": instructions}},
		}
	}

	// Convert input to contents
	var contents []map[string]any
	switch v := req["input"].(type) {
	case string:
		contents = append(contents, map[string]any{
			"role":  "user",
			"parts": []map[string]any{{"text": v}},
		})
	case []any:
		for _, item := range v {
			if itemMap, ok := item.(map[string]any); ok {
				role, _ := itemMap["role"].(string)
				geminiRole := mapRoleToGemini(role)

				var parts []map[string]any
				if content, ok := itemMap["content"].(string); ok {
					parts = append(parts, map[string]any{"text": content})
				}

				contents = append(contents, map[string]any{
					"role":  geminiRole,
					"parts": parts,
				})
			}
		}
	}
	geminiReq["contents"] = contents

	// Convert generation config
	genConfig := buildGeminiGenerationConfig(req, "temperature", "top_p", "max_output_tokens", "")
	if len(genConfig) > 0 {
		geminiReq["generationConfig"] = genConfig
	}

	return geminiReq
}

// ============================================================================
// Response conversions
// ============================================================================

// convertResponseToAnthropic converts OpenAI or Gemini response to Anthropic format
func convertResponseToAnthropic(body []byte, apiType config.APIType) ([]byte, error) {
	switch apiType {
	case config.APITypeOpenAI:
		return convertOpenAIResponseToAnthropic(body)
	case config.APITypeGemini:
		return convertGeminiResponseToAnthropic(body)
	case config.APITypeResponses:
		openAIBody, err := convertResponsesToOpenAIChat(body)
		if err != nil {
			return nil, err
		}
		return convertOpenAIResponseToAnthropic(openAIBody)
	default:
		return body, nil
	}
}

func convertOpenAIResponseToAnthropic(body []byte) ([]byte, error) {
	var openAIResp OpenAIChatResponse
	if err := json.Unmarshal(body, &openAIResp); err != nil {
		return nil, err
	}

	var content []map[string]any
	if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].Message != nil {
		if text, ok := openAIResp.Choices[0].Message.Content.(string); ok {
			content = append(content, map[string]any{
				"type": "text",
				"text": text,
			})
		}
	}

	var stopReason string
	if len(openAIResp.Choices) > 0 && openAIResp.Choices[0].FinishReason != nil {
		stopReason = mapOpenAIStopReasonToAnthropic(*openAIResp.Choices[0].FinishReason)
	}

	anthropicResp := map[string]any{
		"id":          "msg_" + openAIResp.ID,
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       openAIResp.Model,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  openAIResp.Usage.PromptTokens,
			"output_tokens": openAIResp.Usage.CompletionTokens,
		},
	}

	return json.Marshal(anthropicResp)
}

func convertGeminiResponseToAnthropic(body []byte) ([]byte, error) {
	var geminiResp GeminiResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, err
	}

	var content []map[string]any
	if len(geminiResp.Candidates) > 0 {
		for _, part := range geminiResp.Candidates[0].Content.Parts {
			if part.Text != "" {
				content = append(content, map[string]any{
					"type": "text",
					"text": part.Text,
				})
			}
		}
	}

	var stopReason string
	if len(geminiResp.Candidates) > 0 {
		stopReason = mapGeminiStopReasonToAnthropic(geminiResp.Candidates[0].FinishReason)
	}

	var inputTokens, outputTokens int
	if geminiResp.UsageMetadata != nil {
		inputTokens = geminiResp.UsageMetadata.PromptTokenCount
		outputTokens = geminiResp.UsageMetadata.CandidatesTokenCount
	}

	anthropicResp := map[string]any{
		"id":          fmt.Sprintf("msg_%d", time.Now().UnixNano()),
		"type":        "message",
		"role":        "assistant",
		"content":     content,
		"model":       geminiResp.ModelVersion,
		"stop_reason": stopReason,
		"usage": map[string]any{
			"input_tokens":  inputTokens,
			"output_tokens": outputTokens,
		},
	}

	return json.Marshal(anthropicResp)
}

// convertResponseToOpenAI converts Anthropic or Gemini response to OpenAI format
func convertResponseToOpenAI(body []byte, apiType config.APIType) ([]byte, error) {
	switch apiType {
	case config.APITypeAnthropic:
		return convertAnthropicToOpenAIResponse(body)
	case config.APITypeGemini:
		return convertGeminiToOpenAIResponse(body)
	default:
		return body, nil
	}
}

func convertGeminiToOpenAIResponse(body []byte) ([]byte, error) {
	var geminiResp GeminiResponse
	if err := json.Unmarshal(body, &geminiResp); err != nil {
		return nil, err
	}

	var textContent string
	if len(geminiResp.Candidates) > 0 {
		for _, part := range geminiResp.Candidates[0].Content.Parts {
			textContent += part.Text
		}
	}

	var finishReason *string
	if len(geminiResp.Candidates) > 0 && geminiResp.Candidates[0].FinishReason != "" {
		reason := mapGeminiToOpenAIFinishReason(geminiResp.Candidates[0].FinishReason)
		finishReason = &reason
	}

	var promptTokens, completionTokens int
	if geminiResp.UsageMetadata != nil {
		promptTokens = geminiResp.UsageMetadata.PromptTokenCount
		completionTokens = geminiResp.UsageMetadata.CandidatesTokenCount
	}

	openAIResp := OpenAIChatResponse{
		ID:      fmt.Sprintf("chatcmpl-%d", time.Now().UnixNano()),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		Model:   geminiResp.ModelVersion,
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:    "assistant",
					Content: textContent,
				},
				FinishReason: finishReason,
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     promptTokens,
			CompletionTokens: completionTokens,
			TotalTokens:      promptTokens + completionTokens,
		},
	}

	return json.Marshal(openAIResp)
}

// convertResponsesToOpenAIChat converts Responses API response to OpenAI chat format
func convertResponsesToOpenAIChat(body []byte) ([]byte, error) {
	var responsesResp struct {
		ID        string `json:"id"`
		Object    string `json:"object"`
		CreatedAt int64  `json:"created_at"`
		Model     string `json:"model"`
		Output    []struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
			TotalTokens  int `json:"total_tokens"`
		} `json:"usage"`
	}

	if err := json.Unmarshal(body, &responsesResp); err != nil {
		return nil, err
	}

	// Extract text content from output
	var textContent string
	for _, output := range responsesResp.Output {
		for _, content := range output.Content {
			if content.Type == "output_text" || content.Type == "text" {
				textContent += content.Text
			}
		}
	}

	finishReason := "stop"
	openAIResp := OpenAIChatResponse{
		ID:      "chatcmpl-" + responsesResp.ID,
		Object:  "chat.completion",
		Created: responsesResp.CreatedAt,
		Model:   responsesResp.Model,
		Choices: []OpenAIChoice{
			{
				Index: 0,
				Message: &OpenAIMessage{
					Role:    "assistant",
					Content: textContent,
				},
				FinishReason: &finishReason,
			},
		},
		Usage: OpenAIUsage{
			PromptTokens:     responsesResp.Usage.InputTokens,
			CompletionTokens: responsesResp.Usage.OutputTokens,
			TotalTokens:      responsesResp.Usage.TotalTokens,
		},
	}

	return json.Marshal(openAIResp)
}

// ============================================================================
// Stream conversions
// ============================================================================

// extractOpenAIStreamContent extracts content from OpenAI streaming event
func extractOpenAIStreamContent(event map[string]any) (content string, model string) {
	if m, ok := event["model"].(string); ok {
		model = m
	}
	choices, ok := event["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	delta, ok := choice["delta"].(map[string]any)
	if !ok {
		return
	}
	content, _ = delta["content"].(string)
	return
}

// extractGeminiStreamData extracts texts and metadata from Gemini streaming event.
// Handles both root-level candidates and response-wrapped payloads ({"response": {...}}).
func extractGeminiStreamData(event map[string]any) (texts []string, model string, inputTokens, outputTokens int) {
	// Unwrap response-wrapped payloads (Gemini CLI / Antigravity may wrap under "response")
	data := event
	if resp, ok := event["response"].(map[string]any); ok {
		data = resp
	}

	if mv, ok := data["modelVersion"].(string); ok {
		model = mv
	}
	if usage, ok := data["usageMetadata"].(map[string]any); ok {
		if prompt, ok := usage["promptTokenCount"].(float64); ok {
			inputTokens = int(prompt)
		}
		if candidates, ok := usage["candidatesTokenCount"].(float64); ok {
			outputTokens = int(candidates)
		}
	}
	candidates, ok := data["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return
	}
	candidate, ok := candidates[0].(map[string]any)
	if !ok {
		return
	}
	content, ok := candidate["content"].(map[string]any)
	if !ok {
		return
	}
	parts, ok := content["parts"].([]any)
	if !ok {
		return
	}
	for _, part := range parts {
		if partMap, ok := part.(map[string]any); ok {
			if text, ok := partMap["text"].(string); ok {
				texts = append(texts, text)
			}
		}
	}
	return
}

// convertStreamToAnthropic converts OpenAI or Gemini streaming response to Anthropic format
func (h *AnthropicHandler) convertStreamToAnthropic(reader io.Reader, apiType config.APIType, toNonStream bool) ([]byte, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var (
		messageID    string
		model        string
		inputTokens  int
		outputTokens int
		textContent  strings.Builder
		dataBuilder  strings.Builder
		chunks       []byte
	)

	messageID = fmt.Sprintf("msg_%d", time.Now().UnixNano())

	flushEvent := func() {
		if dataBuilder.Len() == 0 {
			return
		}
		data := strings.TrimSpace(dataBuilder.String())
		dataBuilder.Reset()

		if data == "" || data == "[DONE]" {
			return
		}

		var event map[string]any
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return
		}

		switch apiType {
		case config.APITypeOpenAI:
			content, m := extractOpenAIStreamContent(event)
			if m != "" {
				model = m
			}
			if content != "" {
				textContent.WriteString(content)
				if !toNonStream {
					evt := map[string]any{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]any{
							"type": "text_delta",
							"text": content,
						},
					}
					chunks = appendSSEEvent(chunks, "content_block_delta", evt)
				}
			}
		case config.APITypeGemini:
			texts, m, in, out := extractGeminiStreamData(event)
			if m != "" {
				model = m
			}
			if in > 0 {
				inputTokens = in
			}
			if out > 0 {
				outputTokens = out
			}
			for _, text := range texts {
				textContent.WriteString(text)
				if !toNonStream {
					evt := map[string]any{
						"type":  "content_block_delta",
						"index": 0,
						"delta": map[string]any{
							"type": "text_delta",
							"text": text,
						},
					}
					chunks = appendSSEEvent(chunks, "content_block_delta", evt)
				}
			}
		case config.APITypeResponses:
			eventType, _ := event["type"].(string)
			switch eventType {
			case "response.output_text.delta":
				if delta, ok := event["delta"].(string); ok && delta != "" {
					textContent.WriteString(delta)
					if !toNonStream {
						evt := map[string]any{
							"type":  "content_block_delta",
							"index": 0,
							"delta": map[string]any{
								"type": "text_delta",
								"text": delta,
							},
						}
						chunks = appendSSEEvent(chunks, "content_block_delta", evt)
					}
				}
			case "response.completed":
				if resp, ok := event["response"].(map[string]any); ok {
					if m, ok := resp["model"].(string); ok {
						model = m
					}
					if usage, ok := resp["usage"].(map[string]any); ok {
						if in, ok := usage["input_tokens"].(float64); ok {
							inputTokens = int(in)
						}
						if out, ok := usage["output_tokens"].(float64); ok {
							outputTokens = int(out)
						}
					}
				}
			}
		}
	}

	// Send initial events if streaming
	if !toNonStream {
		// message_start
		startEvt := map[string]any{
			"type": "message_start",
			"message": map[string]any{
				"id":    messageID,
				"type":  "message",
				"role":  "assistant",
				"model": model,
				"usage": map[string]any{
					"input_tokens": 0,
				},
			},
		}
		chunks = appendSSEEvent(chunks, "message_start", startEvt)

		// content_block_start
		blockStartEvt := map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}
		chunks = appendSSEEvent(chunks, "content_block_start", blockStartEvt)
	}

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			flushEvent()
			continue
		}

		if strings.HasPrefix(line, ":") {
			continue
		}

		if strings.HasPrefix(line, "event:") {
			// Skip event type line - we parse type from data
			continue
		}

		if strings.HasPrefix(line, "data:") {
			data := strings.TrimPrefix(line, "data:")
			data = strings.TrimPrefix(data, " ")
			if dataBuilder.Len() > 0 {
				dataBuilder.WriteByte('\n')
			}
			dataBuilder.WriteString(data)
		}
	}

	flushEvent()

	if toNonStream {
		// Return complete Anthropic response
		anthropicResp := map[string]any{
			"id":   messageID,
			"type": "message",
			"role": "assistant",
			"content": []map[string]any{
				{"type": "text", "text": textContent.String()},
			},
			"model":       model,
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":  inputTokens,
				"output_tokens": outputTokens,
			},
		}
		return json.Marshal(anthropicResp)
	}

	// Send final events
	// content_block_stop
	blockStopEvt := map[string]any{
		"type":  "content_block_stop",
		"index": 0,
	}
	chunks = appendSSEEvent(chunks, "content_block_stop", blockStopEvt)

	// message_delta
	deltaEvt := map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": "end_turn",
		},
		"usage": map[string]any{
			"output_tokens": outputTokens,
		},
	}
	chunks = appendSSEEvent(chunks, "message_delta", deltaEvt)

	// message_stop
	stopEvt := map[string]any{"type": "message_stop"}
	chunks = appendSSEEvent(chunks, "message_stop", stopEvt)

	return chunks, nil
}

// ============================================================================
// Helper functions for stop reason mapping
// ============================================================================

func mapOpenAIStopReasonToAnthropic(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func mapGeminiStopReasonToAnthropic(reason string) string {
	switch reason {
	case "STOP":
		return "end_turn"
	case "MAX_TOKENS":
		return "max_tokens"
	default:
		return "end_turn"
	}
}

func mapGeminiToOpenAIFinishReason(reason string) string {
	switch reason {
	case "STOP":
		return "stop"
	case "MAX_TOKENS":
		return "length"
	default:
		return "stop"
	}
}
