package pi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChooseModelUsesSignedInProvider(t *testing.T) {
	c := Catalog{
		DefaultModel: "openai-codex/gpt-5.6-luna",
		Models: []ModelOption{
			{Provider: "openai-codex", ID: "gpt-5.6-luna", Full: "openai-codex/gpt-5.6-luna"},
			{Provider: "xai", ID: "grok-4.3", Full: "xai/grok-4.3"},
		},
	}
	got, err := ChooseModel(c, "", map[string]bool{"xai": true})
	if err != nil {
		t.Fatal(err)
	}
	if got != "xai/grok-4.3" {
		t.Fatalf("got %s", got)
	}
	got, err = ChooseModel(c, "", map[string]bool{"openai-codex": true})
	if err != nil || got != c.DefaultModel {
		t.Fatalf("default = %s err=%v", got, err)
	}
	if _, err := ChooseModel(c, "", map[string]bool{}); !errorsIsUnsigned(err) {
		t.Fatalf("err = %v", err)
	}
}

func TestSignedProvidersIgnoresSecretValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	body := []byte(`{"xai-auth":{"type":"oauth","access":"secret"},"openai-codex":null}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	got := SignedProviders(path)
	if !got["xai"] || !got["xai-auth"] {
		t.Fatalf("providers = %#v", got)
	}
	if got["openai-codex"] {
		t.Fatal("null credential counted as signed in")
	}
}

func TestCandidateModelsSkipsUnsigned(t *testing.T) {
	c := Catalog{
		DefaultModel: "openai-codex/gpt-5.6-luna",
		Models: []ModelOption{
			{Provider: "openrouter", ID: "claude-sonnet-4.6", Full: "openrouter/anthropic/claude-sonnet-4.6"},
			{Provider: "xai", ID: "grok-4.3", Full: "xai/grok-4.3"},
		},
	}
	got := CandidateModels(c, "", map[string]bool{"openrouter": true, "xai": true})
	if len(got) != 2 || got[0] != "openrouter/anthropic/claude-sonnet-4.6" || got[1] != "xai/grok-4.3" {
		t.Fatalf("candidates = %#v", got)
	}
}

func TestModelRejected(t *testing.T) {
	if !ModelRejected(`404 {"error":{"message":"Not Found","code":404}}`) {
		t.Fatal("404 should be a rejected model")
	}
	if ModelRejected("connection reset") {
		t.Fatal("unrelated error was treated as a dead model")
	}
}

func errorsIsUnsigned(err error) bool {
	return err != nil && err.Error() == ErrModelUnsigned.Error()
}
