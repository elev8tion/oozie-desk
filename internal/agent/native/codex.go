package native

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"oozie-desk/internal/agent/pi"
)

// Same Codex surface pi uses for provider openai-codex.
const (
	codexBaseURL    = "https://chatgpt.com/backend-api"
	codexTokenURL   = "https://auth.openai.com/oauth/token"
	codexClientID   = "app_EMoamEEZ73f0CkXaXp7hrann"
	codexJWTClaim   = "https://api.openai.com/auth"
	codexOriginator = "pi"
)

func (c *ChatClient) completeCodex(ctx context.Context, fullModel string, messages []chatMessage, withTools bool, maxTokens int) (chatMessage, *piUsage, error) {
	if err := c.refreshCodex(ctx); err != nil {
		return chatMessage{}, nil, err
	}
	_, token, modelID, err := c.Keys.ResolveEndpoint(fullModel)
	if err != nil {
		return chatMessage{}, nil, err
	}
	account := c.Keys.CodexAccount
	if account == "" {
		account = codexAccountID(token)
	}
	if account == "" {
		return chatMessage{}, nil, fmt.Errorf("openai-codex token has no chatgpt account id")
	}
	body, err := codexRequestBody(modelID, messages, withTools, maxTokens)
	if err != nil {
		return chatMessage{}, nil, err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return chatMessage{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexBaseURL+"/codex/responses", bytes.NewReader(raw))
	if err != nil {
		return chatMessage{}, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("chatgpt-account-id", account)
	req.Header.Set("originator", codexOriginator)
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("accept", "text/event-stream")
	req.Header.Set("content-type", "application/json")
	req.Header.Set("User-Agent", "pi")
	res, err := c.http().Do(req)
	if err != nil {
		return chatMessage{}, nil, err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(res.Body, 4000))
		return chatMessage{}, nil, fmt.Errorf("model HTTP %d: %s", res.StatusCode, truncate(string(b), 240))
	}
	return parseCodexSSE(res.Body)
}

func codexRequestBody(modelID string, messages []chatMessage, withTools bool, maxTokens int) (map[string]any, error) {
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
		"model":               modelID,
		"store":               false,
		"stream":              true,
		"instructions":        instructions,
		"input":               input,
		"text":                map[string]any{"verbosity": "low"},
		"include":             []string{"reasoning.encrypted_content"},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": codexReasoningEffort(), "summary": "auto"},
	}
	_ = maxTokens // Codex rejects max_output_tokens; pi does not send it.
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
	}
	return body, nil
}

func codexReasoningEffort() string {
	level := strings.TrimSpace(pi.LoadCatalog().ThinkingLevel)
	switch level {
	case "minimal", "low", "medium", "high", "xhigh":
		return level
	default:
		return "medium"
	}
}

func splitToolID(id string) (callID, itemID string) {
	callID, itemID, _ = strings.Cut(id, "|")
	if callID == "" {
		callID = id
	}
	return callID, itemID
}

type codexEvent struct {
	Type     string `json:"type"`
	Delta    string `json:"delta"`
	Message  string `json:"message"`
	Code     string `json:"code"`
	Response struct {
		Status string `json:"status"`
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
		Error *struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		Usage *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
			TotalTokens  int64 `json:"total_tokens"`
		} `json:"usage"`
	} `json:"response"`
}

func parseCodexSSE(r io.Reader) (chatMessage, *piUsage, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4<<20)
	var text strings.Builder
	var calls []toolCall
	var usage *piUsage
	var finish string
	sawTerminal := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var ev codexEvent
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "response.output_text.delta":
			text.WriteString(ev.Delta)
		case "error":
			msg := ev.Message
			if msg == "" {
				msg = ev.Code
			}
			return chatMessage{}, nil, fmt.Errorf("%s", msg)
		case "response.failed":
			if ev.Response.Error != nil && ev.Response.Error.Message != "" {
				return chatMessage{}, nil, fmt.Errorf("%s", ev.Response.Error.Message)
			}
			return chatMessage{}, nil, fmt.Errorf("codex response failed")
		case "response.completed", "response.incomplete":
			sawTerminal = true
			finish = ev.Response.Status
			if ev.Response.Usage != nil {
				usage = &piUsage{Input: ev.Response.Usage.InputTokens, Output: ev.Response.Usage.OutputTokens, Total: ev.Response.Usage.TotalTokens}
			}
			text.Reset()
			calls = nil
			for _, item := range ev.Response.Output {
				switch item.Type {
				case "message":
					for _, part := range item.Content {
						text.WriteString(part.Text)
					}
				case "function_call":
					calls = append(calls, toolCall{
						ID:   item.CallID,
						Type: "function",
						Function: toolFunction{
							Name:      item.Name,
							Arguments: item.Arguments,
						},
					})
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return chatMessage{}, nil, err
	}
	if !sawTerminal && text.Len() == 0 && len(calls) == 0 {
		return chatMessage{}, nil, fmt.Errorf("codex stream ended before a response")
	}
	msg := chatMessage{Role: "assistant", Content: text.String(), ToolCalls: calls, FinishReason: finish}
	if finish == "incomplete" {
		msg.FinishReason = "length"
	}
	return msg, usage, nil
}

func (c *ChatClient) refreshCodex(ctx context.Context) error {
	if c.Keys.CodexAccess == "" || c.Keys.CodexRefresh == "" || !codexExpired(c.Keys.CodexExpires) {
		return nil
	}
	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", c.Keys.CodexRefresh)
	form.Set("client_id", codexClientID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, codexTokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	res, err := c.http().Do(req)
	if err != nil {
		return fmt.Errorf("openai-codex token refresh: %w", err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode >= 300 {
		return fmt.Errorf("openai-codex token refresh failed (%d)", res.StatusCode)
	}
	var parsed struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		ExpiresIn    int64  `json:"expires_in"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil || parsed.AccessToken == "" || parsed.RefreshToken == "" {
		return fmt.Errorf("openai-codex token refresh response missing fields")
	}
	c.Keys.CodexAccess = parsed.AccessToken
	c.Keys.CodexRefresh = parsed.RefreshToken
	c.Keys.CodexExpires = time.Now().Add(time.Duration(parsed.ExpiresIn) * time.Second).UnixMilli()
	if id := codexAccountID(parsed.AccessToken); id != "" {
		c.Keys.CodexAccount = id
	}
	return saveCodexCreds(c.Keys)
}

func codexExpired(expires int64) bool {
	if expires == 0 {
		return false
	}
	ms := expires
	if ms < 1_000_000_000_000 {
		ms *= 1000
	}
	return time.Now().Add(time.Minute).UnixMilli() >= ms
}

func codexAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return ""
		}
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	auth, _ := claims[codexJWTClaim].(map[string]any)
	id, _ := auth["chatgpt_account_id"].(string)
	return id
}

func saveCodexCreds(k Keys) error {
	path := pi.DefaultAuthPath()
	if p := os.Getenv("OOZIE_AUTH_PATH"); p != "" {
		path = p
	}
	raw := map[string]any{}
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &raw)
	}
	raw["openai-codex"] = map[string]any{
		"type":      "oauth",
		"access":    k.CodexAccess,
		"refresh":   k.CodexRefresh,
		"expires":   k.CodexExpires,
		"accountId": k.CodexAccount,
	}
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}
