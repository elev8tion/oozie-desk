package native

import (
	"strings"
	"testing"

	"oozie-desk/internal/agent/pi"
)

func TestOpenRouterCustomUsesPiConfig(t *testing.T) {
	k := Keys{OpenRouter: "sk-or", OpenRouterCustom: "sk-custom", OpenRouterBase: "https://openrouter.ai/api/v1"}
	base, key, model, err := k.ResolveEndpoint("openrouter-custom/z-ai/glm-5.3-flash")
	if err != nil {
		t.Fatal(err)
	}
	if base != "https://openrouter.ai/api/v1" || key != "sk-custom" || model != "z-ai/glm-5.3-flash" {
		t.Fatalf("base=%s key=%s model=%s", base, key, model)
	}
	signed := SignedFromKeys(k)
	if !signed["openrouter"] || !signed["openrouter-custom"] {
		t.Fatalf("signed=%v", signed)
	}
	cat := MergeCatalog(pi.Catalog{Models: []pi.ModelOption{{Provider: "xai", ID: "grok", Full: "xai/grok"}}, DefaultModel: "xai/grok"})
	found := false
	for _, m := range cat.Models {
		if strings.HasPrefix(m.Full, "openrouter-custom/") {
			found = true
		}
	}
	if !found {
		t.Fatal("openrouter-custom model from ~/.pi/agent/models.json was not added")
	}
}
