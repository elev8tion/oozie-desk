package projects

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"oozie/internal/agent/pi"
)

func TestBuildProgress(t *testing.T) {
	cases := []struct {
		role, status, content, want string
	}{
		{"", "", "", "Starting."},
		{"tool", "running", "write: main.go (running)", "Writing the tool."},
		{"tool", "done", "bash: go build -o /tmp/app . (done)", "Checking the build."},
		{"assistant", "streaming", "I'll make the page", "Writing the tool."},
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

func TestSetupHintWhenUnsigned(t *testing.T) {
	s := newTestService(t)
	s.catalog = pi.Catalog{
		DefaultModel: "openrouter/anthropic/claude-sonnet-4.6",
		Models: []pi.ModelOption{
			{Provider: "openrouter", ID: "claude-sonnet-4.6", Full: "openrouter/anthropic/claude-sonnet-4.6"},
		},
	}
	s.signedIn = func() map[string]bool { return map[string]bool{} }
	hint := s.SetupHint()
	if hint == "" || !strings.Contains(hint, "pi /login") {
		t.Fatalf("setup hint = %q", hint)
	}
	s.signedIn = func() map[string]bool { return map[string]bool{"openrouter": true} }
	if got := s.SetupHint(); got != "" {
		t.Fatalf("signed-in hint = %q, want empty", got)
	}
}

func TestImproveStatusPhases(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	p, err := s.CreateProject(ctx, "FixMe", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	appID, err := s.repo.UpsertStoreApp(ctx, p.ID, PublishDraft{AppName: "FixMe", Headline: "h", Description: "d"}, "", "fix-me")
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.repo.GetSession(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	reqID, err := s.repo.CreateAgentRequest(ctx, session.ID, "build", "make it better")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.InsertImproveRequest(ctx, reqID, appID, "make it better"); err != nil {
		t.Fatal(err)
	}

	st, err := s.ImproveStatus(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "building" || st.AppID != appID || st.Note != "make it better" {
		t.Fatalf("building view = %+v", st)
	}

	imp, err := s.repo.ImproveByRequest(ctx, reqID)
	if err != nil || imp == nil {
		t.Fatalf("improve row: %v %#v", err, imp)
	}
	if err := s.repo.SetImproveStatus(ctx, imp.ID, "done"); err != nil {
		t.Fatal(err)
	}
	st, err = s.ImproveStatus(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	wantURL := fmt.Sprintf("/run/%d", appID)
	if st.Phase != "open" || st.URL != wantURL {
		t.Fatalf("done view = %+v, want open %s", st, wantURL)
	}

	if err := s.repo.SetImproveStatus(ctx, imp.ID, "failed"); err != nil {
		t.Fatal(err)
	}
	st, err = s.ImproveStatus(ctx, reqID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "failed" {
		t.Fatalf("failed view = %+v", st)
	}
}
