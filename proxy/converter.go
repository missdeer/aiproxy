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
// Anthropic -> Other formats
// ============================================================================

// convertAnthropicToOpenAIRequest converts Anthropic messages request to OpenAI chat format
func convertAnthropicToOpenAIRequest(req map[string]any) map[string]any {
	openAIReq := make(map[string]any)

	// Copy model
	if model, ok := req["model"]; ok {
		openAIReq["model"] = model
	}

	// Convert max_tokens
	if maxTokens, ok := req["max_tokens"]; ok {
		openAIReq["max_tokens"] = maxTokens
	}

	// Copy optional parameters
	if temp, ok := req["temperature"]; ok {
		openAIReq["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		openAIReq["top_p"] = topP
	}
	if stop, ok := req["stop_sequences"]; ok {
		openAIReq["stop"] = stop
	}
	if stream, ok := req["stream"]; ok {
		openAIReq["stream"] = stream
	}

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
				geminiRole := "user"
				if role == "assistant" {
					geminiRole = "model"
				}

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
	genConfig := make(map[string]any)
	if temp, ok := req["temperature"]; ok {
		genConfig["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		genConfig["topP"] = topP
	}
	if maxTokens, ok := req["max_tokens"]; ok {
		genConfig["maxOutputTokens"] = maxTokens
	}
	if stop, ok := req["stop_sequences"]; ok {
		genConfig["stopSequences"] = stop
	}
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
	if maxTokens, ok := req["max_tokens"]; ok {
		responsesReq["max_output_tokens"] = maxTokens
	}

	// Copy optional parameters
	if temp, ok := req["temperature"]; ok {
		responsesReq["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		responsesReq["top_p"] = topP
	}
	if stream, ok := req["stream"]; ok {
		responsesReq["stream"] = stream
	}

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
func convertOpenAIChatToAnthropicRequest(req map[string]any) map[string]any {
	anthropicReq := make(map[string]any)

	// Copy model
	if model, ok := req["model"]; ok {
		anthropicReq["model"] = model
	}

	// Convert max_tokens
	if maxTokens, ok := req["max_tokens"]; ok {
		anthropicReq["max_tokens"] = maxTokens
	} else {
		anthropicReq["max_tokens"] = 4096
	}

	// Copy optional parameters
	if temp, ok := req["temperature"]; ok {
		anthropicReq["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		anthropicReq["top_p"] = topP
	}
	if stop, ok := req["stop"]; ok {
		anthropicReq["stop_sequences"] = stop
	}
	if stream, ok := req["stream"]; ok {
		anthropicReq["stream"] = stream
	}

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

	if model, ok := req["model"]; ok {
		responsesReq["model"] = model
	}
	if maxTokens, ok := req["max_tokens"]; ok {
		responsesReq["max_output_tokens"] = maxTokens
	}
	if temp, ok := req["temperature"]; ok {
		responsesReq["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		responsesReq["top_p"] = topP
	}
	if stream, ok := req["stream"]; ok {
		responsesReq["stream"] = stream
	}

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
			var systemText string
			for _, part := range parts {
				if partMap, ok := part.(map[string]any); ok {
					if text, ok := partMap["text"].(string); ok {
						systemText += text
					}
				}
			}
			if systemText != "" {
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
					if r == "model" {
						role = "assistant"
					} else {
						role = r
					}
				}

				var textContent string
				if parts, ok := contentMap["parts"].([]any); ok {
					for _, part := range parts {
						if partMap, ok := part.(map[string]any); ok {
							if text, ok := partMap["text"].(string); ok {
								textContent += text
							}
						}
					}
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
				geminiRole := "user"
				if role == "assistant" {
					geminiRole = "model"
				}

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
	genConfig := make(map[string]any)
	if temp, ok := req["temperature"]; ok {
		genConfig["temperature"] = temp
	}
	if topP, ok := req["top_p"]; ok {
		genConfig["topP"] = topP
	}
	if maxTokens, ok := req["max_output_tokens"]; ok {
		genConfig["maxOutputTokens"] = maxTokens
	}
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

// ============================================================================
// Stream conversions
// ============================================================================

// convertStreamToAnthropic converts OpenAI or Gemini streaming response to Anthropic format
func (h *Handler) convertStreamToAnthropic(reader io.Reader, apiType config.APIType, toNonStream bool) ([]byte, error) {
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
			if m, ok := event["model"].(string); ok {
				model = m
			}
			if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if delta, ok := choice["delta"].(map[string]any); ok {
						if content, ok := delta["content"].(string); ok {
							textContent.WriteString(content)
							if !toNonStream {
								// Generate Anthropic SSE event
								evt := map[string]any{
									"type":  "content_block_delta",
									"index": 0,
									"delta": map[string]any{
										"type": "text_delta",
										"text": content,
									},
								}
								evtBytes, _ := json.Marshal(evt)
								chunks = append(chunks, []byte("event: content_block_delta\ndata: ")...)
								chunks = append(chunks, evtBytes...)
								chunks = append(chunks, []byte("\n\n")...)
							}
						}
					}
				}
			}
		case config.APITypeGemini:
			if candidates, ok := event["candidates"].([]any); ok && len(candidates) > 0 {
				if candidate, ok := candidates[0].(map[string]any); ok {
					if content, ok := candidate["content"].(map[string]any); ok {
						if parts, ok := content["parts"].([]any); ok {
							for _, part := range parts {
								if partMap, ok := part.(map[string]any); ok {
									if text, ok := partMap["text"].(string); ok {
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
											evtBytes, _ := json.Marshal(evt)
											chunks = append(chunks, []byte("event: content_block_delta\ndata: ")...)
											chunks = append(chunks, evtBytes...)
											chunks = append(chunks, []byte("\n\n")...)
										}
									}
								}
							}
						}
					}
				}
			}
			if usage, ok := event["usageMetadata"].(map[string]any); ok {
				if prompt, ok := usage["promptTokenCount"].(float64); ok {
					inputTokens = int(prompt)
				}
				if candidates, ok := usage["candidatesTokenCount"].(float64); ok {
					outputTokens = int(candidates)
				}
			}
			if mv, ok := event["modelVersion"].(string); ok {
				model = mv
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
		startBytes, _ := json.Marshal(startEvt)
		chunks = append(chunks, []byte("event: message_start\ndata: ")...)
		chunks = append(chunks, startBytes...)
		chunks = append(chunks, []byte("\n\n")...)

		// content_block_start
		blockStartEvt := map[string]any{
			"type":  "content_block_start",
			"index": 0,
			"content_block": map[string]any{
				"type": "text",
				"text": "",
			},
		}
		blockStartBytes, _ := json.Marshal(blockStartEvt)
		chunks = append(chunks, []byte("event: content_block_start\ndata: ")...)
		chunks = append(chunks, blockStartBytes...)
		chunks = append(chunks, []byte("\n\n")...)
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
	blockStopBytes, _ := json.Marshal(blockStopEvt)
	chunks = append(chunks, []byte("event: content_block_stop\ndata: ")...)
	chunks = append(chunks, blockStopBytes...)
	chunks = append(chunks, []byte("\n\n")...)

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
	deltaBytes, _ := json.Marshal(deltaEvt)
	chunks = append(chunks, []byte("event: message_delta\ndata: ")...)
	chunks = append(chunks, deltaBytes...)
	chunks = append(chunks, []byte("\n\n")...)

	// message_stop
	stopEvt := map[string]any{"type": "message_stop"}
	stopBytes, _ := json.Marshal(stopEvt)
	chunks = append(chunks, []byte("event: message_stop\ndata: ")...)
	chunks = append(chunks, stopBytes...)
	chunks = append(chunks, []byte("\n\n")...)

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
