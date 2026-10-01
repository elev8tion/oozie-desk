package native

import (
	"testing"

	"oozie-desk/internal/agent/pi"
)

func TestPromptMessagesKeepHistory(t *testing.T) {
	msgs := promptMessages("sys", "new ask", []pi.Turn{{Role: "user", Content: "first job"}, {Role: "assistant", Content: "built it"}})
	if len(msgs) != 4 || msgs[0].Role != "system" || msgs[1].Content != "first job" || msgs[3].Content != "new ask" {
		t.Fatalf("msgs=%+v", msgs)
	}
}
