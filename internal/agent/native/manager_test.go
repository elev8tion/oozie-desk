package native

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"oozie/internal/agent/pi"
)

type memSink struct {
	mu       sync.Mutex
	messages []string
	tools    []string
	settled  string
	errs     []string
}

func (m *memSink) AssistantMessage(_, _ int64, content string) {
	m.mu.Lock()
	m.messages = append(m.messages, content)
	m.mu.Unlock()
}
func (m *memSink) AssistantPartial(_, _ int64, _ string) {}
func (m *memSink) ToolStarted(_, _ int64, _, content string) {
	m.mu.Lock()
	m.tools = append(m.tools, "start:"+content)
	m.mu.Unlock()
}
func (m *memSink) ToolFinished(_, _ int64, _, content, _ string) {
	m.mu.Lock()
	m.tools = append(m.tools, "end:"+content)
	m.mu.Unlock()
}
func (m *memSink) RequestSettled(_, _ int64, status string) {
	m.mu.Lock()
	m.settled = status
	m.mu.Unlock()
}
func (m *memSink) Question(_, _ int64, _, _, _ string)   {}
func (m *memSink) Permission(_, _ int64, _, _, _ string) {}
func (m *memSink) AgentError(_, _ int64, message string) {
	m.mu.Lock()
	m.errs = append(m.errs, message)
	m.mu.Unlock()
}

func TestManagerToolLoop(t *testing.T) {
	dir := t.TempDir()
	var calls int
	var sawMaxTokens int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req chatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.MaxTokens > 0 {
			sawMaxTokens = req.MaxTokens
		}
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			// ask to write a file
			_ = json.NewEncoder(w).Encode(map[string]any{
				"choices": []map[string]any{{
					"message": map[string]any{
						"role": "assistant",
						"tool_calls": []map[string]any{{
							"id":   "c1",
							"type": "function",
							"function": map[string]any{
								"name":      "write",
								"arguments": `{"path":"note.txt","content":"built"}`,
							},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "Done writing note.txt"},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	sink := &memSink{}
	m := NewManager(DefaultCatalog(), sink, Keys{OpenRouter: "test-key"})
	// Point openrouter base at mock by overriding Resolve via custom client URL:
	// use a tiny wrapper — patch by using openai provider mapped to mock.
	// Easier: replace Keys.Resolve by using openrouter host... can't. Use HTTP client transport?
	// Instead set env-free: override client with base rewrite in Complete — for test, inject fake.
	m.client = &ChatClient{
		Keys: Keys{OpenRouter: "test-key"},
		HTTP: srv.Client(),
	}
	// Monkey: make ResolveEndpoint return mock server for openrouter
	// by temporarily replacing Complete through a local test client type.
	// Simplest path: redefine keys resolve via openrouter URL is fixed.
	// Use a custom ChatClient method — change Complete to accept BaseURL override in test.
	// Hack: run against srv by making provider "openrouter" and replacing http.DefaultTransport.
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		return orig.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
	m.client.HTTP = http.DefaultClient

	err := m.Prompt(pi.StartOptions{
		ProjectID: 1,
		Workdir:   dir,
		Model:     "openrouter/anthropic/claude-sonnet-4.6",
		Trusted:   true,
	}, 9, "write a note")
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		done := sink.settled
		sink.mu.Unlock()
		if done != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if sawMaxTokens != 1024 {
		t.Fatalf("max_tokens=%d want 1024", sawMaxTokens)
	}
	if sink.settled != "completed" {
		t.Fatalf("settled=%q errs=%v", sink.settled, sink.errs)
	}
	if len(sink.messages) == 0 || !strings.Contains(sink.messages[0], "Done") {
		t.Fatalf("messages=%v", sink.messages)
	}
	body, err := osRead(dir, "note.txt")
	if err != nil || body != "built" {
		t.Fatalf("file=%q err=%v", body, err)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func osRead(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), err
}
