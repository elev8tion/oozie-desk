package native

import (
	"strings"
	"testing"
)

func TestCodexEndpointIsNotPlatformAPI(t *testing.T) {
	k := Keys{CodexAccess: "tok", CodexAccount: "acc"}
	base, key, model, err := k.ResolveEndpoint("openai-codex/gpt-5.5")
	if err != nil {
		t.Fatal(err)
	}
	if base != codexBaseURL || key != "tok" || model != "gpt-5.5" {
		t.Fatalf("base=%s model=%s", base, model)
	}
	if _, _, _, err := (Keys{OpenAI: "sk"}).ResolveEndpoint("openai-codex/gpt-5.5"); err == nil {
		t.Fatal("platform key must not sign openai-codex")
	}
	signed := SignedFromKeys(Keys{CodexAccess: "tok"})
	if !signed["openai-codex"] || signed["openai"] {
		t.Fatalf("signed=%v", signed)
	}
}

func TestParseCodexSSE(t *testing.T) {
	raw := "" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"Hello\"}]},{\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"bash\",\"arguments\":\"{\\\"command\\\":\\\"go build\\\"}\"}],\"usage\":{\"input_tokens\":3,\"output_tokens\":4,\"total_tokens\":7}}}\n\n"
	msg, usage, err := parseCodexSSE(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Content != "Hello" || len(msg.ToolCalls) != 1 || msg.ToolCalls[0].Function.Name != "bash" {
		t.Fatalf("msg=%+v", msg)
	}
	if usage == nil || usage.Total != 7 {
		t.Fatalf("usage=%+v", usage)
	}
}
