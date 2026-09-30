package projects

import "testing"

func TestBuildProgress(t *testing.T) {
	cases := []struct {
		role, status, content, want string
	}{
		{"", "", "", "Starting."},
		{"tool", "running", "write: main.go (running)", "Writing the page."},
		{"tool", "done", "bash: go build -o /tmp/app . (done)", "Checking the build."},
		{"assistant", "streaming", "I'll make the page", "Writing the page."},
		{"tool", "running", "bash: ls (running)", "Running a check."},
	}
	for _, c := range cases {
		if got := buildProgress(c.role, c.status, c.content); got != c.want {
			t.Errorf("%q => %q, want %q", c.content, got, c.want)
		}
	}
}

func TestPlainPageErrorHidesPiAuth(t *testing.T) {
	got := plainPageError("pi rejected the prompt: No API key found for openai-codex.")
	if got != "This model is not signed in." {
		t.Fatalf("got %q", got)
	}
}
