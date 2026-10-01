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

	"oozie-desk/internal/agent/pi"
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
	if sawMaxTokens != 4096 {
		t.Fatalf("max_tokens=%d want 4096", sawMaxTokens)
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

// retrySink starts a second Prompt from RequestSettled (credit-retry shape).
type retrySink struct {
	memSink
	m        *Manager
	opts     pi.StartOptions
	retried  bool
	secondID int64
}

func (r *retrySink) RequestSettled(projectID, requestID int64, status string) {
	r.memSink.RequestSettled(projectID, requestID, status)
	if r.retried || status != "failed" || requestID != 1 {
		return
	}
	r.retried = true
	r.secondID = 2
	if err := r.m.Prompt(r.opts, 2, "retry build"); err != nil {
		r.mu.Lock()
		r.errs = append(r.errs, "retry prompt: "+err.Error())
		r.mu.Unlock()
	}
}

// TestCreditRetryPromptSurvivesPriorRunCleanup proves the first run's defer
// must not cancel a credit-retry session started from RequestSettled.
func TestCreditRetryPromptSurvivesPriorRunCleanup(t *testing.T) {
	dir := t.TempDir()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		if calls == 1 {
			http.Error(w, `{"error":{"message":"This request requires more credits, or fewer max_tokens. You requested up to 1024 tokens, but can only afford 511."}}`, 402)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"message": map[string]any{"role": "assistant", "content": "retry ok"},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	opts := pi.StartOptions{ProjectID: 42, Workdir: dir, Model: "openrouter/anthropic/claude-haiku-4.5", Trusted: true}
	sink := &retrySink{opts: opts}
	m := NewManager(DefaultCatalog(), sink, Keys{OpenRouter: "test-key"})
	sink.m = m
	m.client = &ChatClient{Keys: Keys{OpenRouter: "test-key"}, HTTP: http.DefaultClient}
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		return orig.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })

	if err := m.Prompt(opts, 1, "first build"); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		sink.mu.Lock()
		done := sink.settled == "completed" && sink.retried
		sink.mu.Unlock()
		if done {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if !sink.retried {
		t.Fatalf("expected credit retry Prompt, errs=%v settled=%q", sink.errs, sink.settled)
	}
	if sink.settled != "completed" {
		t.Fatalf("retry settled=%q errs=%v calls=%d", sink.settled, sink.errs, calls)
	}
	if len(sink.messages) == 0 || !strings.Contains(sink.messages[len(sink.messages)-1], "retry ok") {
		t.Fatalf("messages=%v errs=%v", sink.messages, sink.errs)
	}
	if calls < 2 {
		t.Fatalf("expected retry LLM call, calls=%d", calls)
	}
}

func TestManagerStopsOnCutOffToolCall(t *testing.T) {
	dir := t.TempDir()
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{
				"finish_reason": "length",
				"message": map[string]any{
					"role": "assistant",
					"tool_calls": []map[string]any{{
						"id":   "c1",
						"type": "function",
						"function": map[string]any{
							"name":      "write",
							"arguments": `{"path":"main.go","content":"package main`,
						},
					}},
				},
			}},
		})
	}))
	t.Cleanup(srv.Close)

	sink := &memSink{}
	m := NewManager(DefaultCatalog(), sink, Keys{OpenRouter: "test-key"})
	orig := http.DefaultTransport
	http.DefaultTransport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL.Scheme = "http"
		req.URL.Host = strings.TrimPrefix(srv.URL, "http://")
		return orig.RoundTrip(req)
	})
	t.Cleanup(func() { http.DefaultTransport = orig })
	m.client = &ChatClient{Keys: Keys{OpenRouter: "test-key"}, HTTP: http.DefaultClient}

	if err := m.Prompt(pi.StartOptions{
		ProjectID: 7,
		Workdir:   dir,
		Model:     "openrouter/anthropic/claude-haiku-4.5",
		Trusted:   true,
	}, 3, "build a notes app"); err != nil {
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
	if sink.settled != "failed" {
		t.Fatalf("settled=%q calls=%d errs=%v", sink.settled, calls, sink.errs)
	}
	if calls > 3 {
		t.Fatalf("cut-off looped too long: calls=%d", calls)
	}
	if len(sink.errs) == 0 || !strings.Contains(sink.errs[0], "cut off") {
		t.Fatalf("errs=%v", sink.errs)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err == nil {
		t.Fatal("cut-off write must not land on disk")
	}
}
