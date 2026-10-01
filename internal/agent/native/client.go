package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChatClient talks OpenAI-compatible chat completions (tools, non-stream for simplicity).
type ChatClient struct {
	HTTP *http.Client
	Keys Keys
}

func (c *ChatClient) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 180 * time.Second}
}

type chatMessage struct {
	Role string `json:"role"`
	// Content must serialize even when empty — OpenRouter/Anthropic reject
	// assistant/tool turns that omit the text field after tool_calls.
	Content    string     `json:"content"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type toolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function toolFunction `json:"function"`
}

type toolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatRequest struct {
	Model      string        `json:"model"`
	Messages   []chatMessage `json:"messages"`
	Tools      []toolDef     `json:"tools,omitempty"`
	ToolChoice any           `json:"tool_choice,omitempty"`
	// Cap completion size. Omitting this lets some providers (OpenRouter)
	// assume a huge default and refuse low-credit accounts.
	MaxTokens int `json:"max_tokens,omitempty"`
}

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type chatResponse struct {
	Choices []struct {
		Message      chatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int64 `json:"prompt_tokens"`
		CompletionTokens int64 `json:"completion_tokens"`
		TotalTokens      int64 `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (c *ChatClient) Complete(ctx context.Context, fullModel string, messages []chatMessage) (chatMessage, *piUsage, error) {
	return c.complete(ctx, fullModel, messages, true, 1024)
}

// CompleteText is a single-turn completion without tools (recipe plans, etc.).
func (c *ChatClient) CompleteText(ctx context.Context, fullModel, system, user string) (string, error) {
	msgs := []chatMessage{}
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: system})
	}
	msgs = append(msgs, chatMessage{Role: "user", Content: user})
	msg, _, err := c.complete(ctx, fullModel, msgs, false, 1200)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(msg.Content), nil
}

func (c *ChatClient) complete(ctx context.Context, fullModel string, messages []chatMessage, withTools bool, maxTokens int) (chatMessage, *piUsage, error) {
	base, key, modelID, err := c.Keys.ResolveEndpoint(fullModel)
	if err != nil {
		return chatMessage{}, nil, err
	}
	reqBody := chatRequest{
		Model:     modelID,
		Messages:  messages,
		MaxTokens: maxTokens,
	}
	if withTools {
		reqBody.Tools = codingTools()
		reqBody.ToolChoice = "auto"
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return chatMessage{}, nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return chatMessage{}, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	if strings.Contains(base, "openrouter") {
		req.Header.Set("HTTP-Referer", "https://oozie.local")
		req.Header.Set("X-Title", "oozie-web")
	}
	res, err := c.http().Do(req)
	if err != nil {
		return chatMessage{}, nil, err
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	var parsed chatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return chatMessage{}, nil, fmt.Errorf("bad model response (%d): %s", res.StatusCode, truncate(string(raw), 240))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return chatMessage{}, nil, fmt.Errorf("%s", parsed.Error.Message)
	}
	if res.StatusCode >= 300 {
		return chatMessage{}, nil, fmt.Errorf("model HTTP %d: %s", res.StatusCode, truncate(string(raw), 240))
	}
	if len(parsed.Choices) == 0 {
		return chatMessage{}, nil, fmt.Errorf("model returned no choices")
	}
	var usage *piUsage
	if parsed.Usage != nil {
		usage = &piUsage{
			Input:  parsed.Usage.PromptTokens,
			Output: parsed.Usage.CompletionTokens,
			Total:  parsed.Usage.TotalTokens,
		}
	}
	return parsed.Choices[0].Message, usage, nil
}

type piUsage struct {
	Input, Output, Total int64
}

func codingTools() []toolDef {
	obj := func(name, desc string, props map[string]any, required []string) toolDef {
		var t toolDef
		t.Type = "function"
		t.Function.Name = name
		t.Function.Description = desc
		t.Function.Parameters = map[string]any{
			"type":       "object",
			"properties": props,
			"required":   required,
		}
		return t
	}
	return []toolDef{
		obj("bash", "Run a shell command in the project directory. Prefer go build to verify.",
			map[string]any{"command": map[string]any{"type": "string", "description": "Shell command"}},
			[]string{"command"}),
		obj("read", "Read a UTF-8 text file under the project directory.",
			map[string]any{"path": map[string]any{"type": "string", "description": "Relative or absolute path inside the project"}},
			[]string{"path"}),
		obj("write", "Create or overwrite a file under the project directory.",
			map[string]any{
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string"},
			},
			[]string{"path", "content"}),
		obj("edit", "Replace exact text in a file. old_text must match uniquely.",
			map[string]any{
				"path":     map[string]any{"type": "string"},
				"old_text": map[string]any{"type": "string"},
				"new_text": map[string]any{"type": "string"},
			},
			[]string{"path", "old_text", "new_text"}),
		obj("ls", "List files in a directory under the project.",
			map[string]any{"path": map[string]any{"type": "string", "description": "Directory path; empty means project root"}},
			[]string{}),
	}
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
