package native

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()

	sum, body, err := runTool(ctx, dir, "write", `{"path":"hello.txt","content":"hi"}`)
	if err != nil || !strings.Contains(sum, "ok") {
		t.Fatalf("write: %v %s %s", err, sum, body)
	}
	sum, body, err = runTool(ctx, dir, "read", `{"path":"hello.txt"}`)
	if err != nil || body != "hi" {
		t.Fatalf("read: %v %s %q", err, sum, body)
	}
	sum, body, err = runTool(ctx, dir, "edit", `{"path":"hello.txt","old_text":"hi","new_text":"hello"}`)
	if err != nil {
		t.Fatalf("edit: %v %s %s", err, sum, body)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if string(b) != "hello" {
		t.Fatalf("file = %q", b)
	}
	sum, body, err = runTool(ctx, dir, "ls", `{"path":"."}`)
	if err != nil || !strings.Contains(body, "hello.txt") {
		t.Fatalf("ls: %v %s %s", err, sum, body)
	}
	sum, body, err = runTool(ctx, dir, "bash", `{"command":"echo ok"}`)
	if err != nil || !strings.Contains(body, "ok") {
		t.Fatalf("bash: %v %s %s", err, sum, body)
	}
}

func TestToolsRefuseEscape(t *testing.T) {
	dir := t.TempDir()
	_, body, err := runTool(context.Background(), dir, "read", `{"path":"../secret"}`)
	if err == nil {
		t.Fatal("expected escape error")
	}
	if !strings.Contains(body, "escapes") {
		t.Fatalf("body = %q", body)
	}
}

func TestBashStaysInTheProject(t *testing.T) {
	dir := t.TempDir()
	_, body, err := runTool(context.Background(), dir, "bash", `{"command":"cat /etc/passwd"}`)
	if err == nil || !strings.Contains(body, "leaves the project") {
		t.Fatalf("err=%v body=%q", err, body)
	}
	_, body, err = runTool(context.Background(), dir, "bash", `{"command":"echo ok"}`)
	if err != nil || !strings.Contains(body, "ok") {
		t.Fatalf("in-project bash failed: %v %s", err, body)
	}
}

func TestResolveEndpointOpenRouter(t *testing.T) {
	k := Keys{OpenRouter: "sk-test"}
	base, key, model, err := k.ResolveEndpoint("openrouter/anthropic/claude-sonnet-4.6")
	if err != nil {
		t.Fatal(err)
	}
	if base == "" || key != "sk-test" || model != "anthropic/claude-sonnet-4.6" {
		t.Fatalf("%s %s %s", base, key, model)
	}
	if _, _, _, err := (Keys{}).ResolveEndpoint("openrouter/x"); err == nil {
		t.Fatal("expected missing key")
	}
}
