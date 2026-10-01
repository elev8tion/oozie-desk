package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// xAI models are openai-responses, not chat completions.
const xaiResponsesURL = "https://api.x.ai/v1/responses"

func (c *ChatClient) completeXAI(ctx context.Context, fullModel string, messages []chatMessage, withTools bool, maxTokens int) (chatMessage, *piUsage, error) {
	_, key, modelID, err := c.Keys.ResolveEndpoint(fullModel)
	if err != nil {
		return chatMessage{}, nil, err
	}
	body, err := xaiRequestBody(modelID, messages, withTools, maxTokens)
	if err != nil {
		return chatMessage{}, nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, xaiResponsesURL, bytes.NewReader(raw))
	if err != nil {
		return chatMessage{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	res, err := c.http().Do(req)
	if err != nil {
		return chatMessage{}, nil, err
	}
	defer res.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 300 {
		return chatMessage{}, nil, fmt.Errorf("model HTTP %d: %s", res.StatusCode, truncate(string(respBody), 240))
	}
	trimmed := bytes.TrimSpace(respBody)
	if bytes.HasPrefix(trimmed, []byte("data:")) || bytes.Contains(trimmed, []byte("\ndata:")) {
		return parseCodexSSE(bytes.NewReader(respBody))
	}
	return parseXAIResponse(respBody)
}

func xaiRequestBody(modelID string, messages []chatMessage, withTools bool, maxTokens int) (map[string]any, error) {
	instructions := "You are a helpful assistant."
	var input []any
	for _, msg := range messages {
		switch msg.Role {
		case "system":
			if strings.TrimSpace(msg.Content) != "" {
				instructions = msg.Content
			}
		case "user":
			input = append(input, map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": msg.Content}},
			})
		case "assistant":
			if strings.TrimSpace(msg.Content) != "" {
				input = append(input, map[string]any{
					"type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": msg.Content}},
				})
			}
			for _, tc := range msg.ToolCalls {
				callID, _ := splitToolID(tc.ID)
				input = append(input, map[string]any{
					"type": "function_call", "call_id": callID, "name": tc.Function.Name, "arguments": tc.Function.Arguments,
				})
			}
		case "tool":
			callID, _ := splitToolID(msg.ToolCallID)
			input = append(input, map[string]any{
				"type": "function_call_output", "call_id": callID, "output": msg.Content,
			})
		}
	}
	body := map[string]any{
		"model":        modelID,
		"store":        false,
		"stream":       false,
		"instructions": instructions,
		"input":        input,
	}
	if maxTokens > 0 {
		body["max_output_tokens"] = maxTokens
	}
	if withTools {
		var tools []any
		for _, t := range codingTools() {
			tools = append(tools, map[string]any{
				"type":        "function",
				"name":        t.Function.Name,
				"description": t.Function.Description,
				"parameters":  t.Function.Parameters,
			})
		}
		body["tools"] = tools
		body["tool_choice"] = "auto"
	}
	return body, nil
}

type xaiResponse struct {
	Status     string `json:"status"`
	OutputText string `json:"output_text"`
	Output     []struct {
		Type      string          `json:"type"`
		CallID    string          `json:"call_id"`
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Content   json.RawMessage `json:"content"`
	} `json:"output"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
		TotalTokens  int64 `json:"total_tokens"`
	} `json:"usage"`
}

func parseXAIResponse(raw []byte) (chatMessage, *piUsage, error) {
	var parsed xaiResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return chatMessage{}, nil, fmt.Errorf("bad model response: %s", truncate(string(raw), 240))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return chatMessage{}, nil, fmt.Errorf("%s", parsed.Error.Message)
	}
	var text strings.Builder
	var calls []toolCall
	if parsed.OutputText != "" {
		text.WriteString(parsed.OutputText)
	}
	for _, item := range parsed.Output {
		switch item.Type {
		case "message", "output_text":
			text.WriteString(xaiText(item.Content))
		case "function_call", "tool_call":
			calls = append(calls, toolCall{
				ID:   item.CallID,
				Type: "function",
				Function: toolFunction{
					Name:      item.Name,
					Arguments: xaiArguments(item.Arguments),
				},
			})
		}
	}
	if text.Len() == 0 && len(calls) == 0 {
		return chatMessage{}, nil, fmt.Errorf("model returned no output")
	}
	var usage *piUsage
	if parsed.Usage != nil {
		usage = &piUsage{Input: parsed.Usage.InputTokens, Output: parsed.Usage.OutputTokens, Total: parsed.Usage.TotalTokens}
	}
	finish := parsed.Status
	if finish == "" {
		finish = "completed"
	}
	if finish == "incomplete" {
		finish = "length"
	}
	return chatMessage{Role: "assistant", Content: text.String(), ToolCalls: calls, FinishReason: finish}, usage, nil
}

func xaiText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func xaiArguments(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return "{}"
	}
	if raw[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return s
		}
	}
	return string(raw)
}
