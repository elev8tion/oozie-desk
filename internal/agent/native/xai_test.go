package native

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestXAIUsesResponsesAPI(t *testing.T) {
	var gotURL, gotAuth string
	var gotBody string
	client := &ChatClient{
		HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			gotURL = r.URL.String()
			gotAuth = r.Header.Get("Authorization")
			raw, _ := io.ReadAll(r.Body)
			gotBody = string(raw)
			body := `{"status":"completed","output_text":"ok","output":[{"type":"function_call","call_id":"call_1","name":"bash","arguments":"{\"command\":\"go build\"}"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		})},
		Keys: Keys{XAI: "xai-test-key"},
	}
	msg, usage, err := client.Complete(context.Background(), "xai/grok-4", []chatMessage{{Role: "user", Content: "build the page"}})
	if err != nil {
		t.Fatal(err)
	}
	if gotURL != xaiResponsesURL {
		t.Fatalf("url = %s, want %s", gotURL, xaiResponsesURL)
	}
	if strings.Contains(gotURL, "chat/completions") {
		t.Fatalf("xAI was sent to chat completions: %s", gotURL)
	}
	if gotAuth != "Bearer xai-test-key" {
		t.Fatalf("auth = %s", gotAuth)
	}
	if !strings.Contains(gotBody, `"model":"grok-4"`) || !strings.Contains(gotBody, `"input"`) {
		t.Fatalf("body = %s", gotBody)
	}
	if strings.Contains(gotBody, `"messages"`) {
		t.Fatalf("responses body used chat messages: %s", gotBody)
	}
	if msg.Content != "ok" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("msg=%+v", msg)
	}
	if msg.ToolCalls[0].Function.Arguments != `{"command":"go build"}` {
		t.Fatalf("arguments=%s", msg.ToolCalls[0].Function.Arguments)
	}
	if usage == nil || usage.Total != 3 {
		t.Fatalf("usage=%+v", usage)
	}
}

func TestParseXAIResponseMapsTextAndCalls(t *testing.T) {
	raw := []byte(`{"status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"Hello"}]},{"type":"function_call","call_id":"call_9","name":"write","arguments":{"path":"main.go"}}]}`)
	msg, _, err := parseXAIResponse(raw)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Hello" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].ID != "call_9" || msg.ToolCalls[0].Function.Name != "write" {
		t.Fatalf("msg=%+v", msg)
	}
	if !strings.Contains(msg.ToolCalls[0].Function.Arguments, "main.go") {
		t.Fatalf("arguments=%s", msg.ToolCalls[0].Function.Arguments)
	}
}
