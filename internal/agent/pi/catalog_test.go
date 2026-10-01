package pi

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadModelStoreKeepsChatModels(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models-store.json")
	body := []byte(`{
	  "xai": {"models": [{"id":"grok-4.7","type":"chat"},{"id":"grok-image","type":"image"}]},
	  "openrouter": {"models": [{"id":"google/gemini-3.8-flash","type":"chat"},{"id":"black-forest-labs/flux.2-pro","type":"image"}]}
	}`)
	if err := os.WriteFile(path, body, 0o644); err != nil {
		t.Fatal(err)
	}
	got := readModelStore(path)
	if len(got["xai"]) != 1 || got["xai"][0] != "grok-4.7" {
		t.Fatalf("xai=%v", got["xai"])
	}
	if len(got["openrouter"]) != 1 || got["openrouter"][0] != "google/gemini-3.8-flash" {
		t.Fatalf("openrouter=%v", got["openrouter"])
	}
}

func TestKeepChatModelDropsImage(t *testing.T) {
	if !keepChatModel("chat", "text") || !keepChatModel("", "text+image") {
		t.Fatal("chat models should stay")
	}
	if keepChatModel("image", "text") || keepChatModel("chat", "image") {
		t.Fatal("image models should not pass as chat")
	}
}

func TestRefreshChatModelsKeepsXAIStoreModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte(`{
	  "xai": {"models": [{"id":"grok-4.7","type":"chat"},{"id":"grok-image","type":"image"}]},
	  "openrouter": {"models": [{"id":"google/gemini-3.8-flash","type":"chat"}]}
	}`)
	if err := os.WriteFile(filepath.Join(dir, "models-store.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	got := RefreshChatModels("")
	var xai, image, openrouter int
	for _, m := range got {
		switch m.Full {
		case "xai/grok-4.7":
			xai++
		case "xai/grok-image":
			image++
		case "openrouter/google/gemini-3.8-flash":
			openrouter++
		}
	}
	if xai != 1 || image != 0 || openrouter != 1 {
		t.Fatalf("refresh = %+v", got)
	}
}

func TestMergeRefreshedModelsKeepsXAI(t *testing.T) {
	existing := []ModelOption{
		{Provider: "xai", ID: "grok-4.7", Full: "xai/grok-4.7"},
		{Provider: "openrouter", ID: "old", Full: "openrouter/old"},
	}
	fresh := []ModelOption{
		{Provider: "openrouter", ID: "google/gemini-3.8-flash", Full: "openrouter/google/gemini-3.8-flash"},
	}
	got := MergeRefreshedModels(existing, fresh)
	var sawXAI, sawOld, sawFresh bool
	for _, m := range got {
		switch m.Full {
		case "xai/grok-4.7":
			sawXAI = true
		case "openrouter/old":
			sawOld = true
		case "openrouter/google/gemini-3.8-flash":
			sawFresh = true
		}
	}
	if !sawXAI || sawOld || !sawFresh {
		t.Fatalf("merged = %+v", got)
	}
	if len(MergeRefreshedModels(existing, nil)) != len(existing) {
		t.Fatal("empty refresh should keep the store list")
	}
}
