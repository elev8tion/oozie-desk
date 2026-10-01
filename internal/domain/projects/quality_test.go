package projects

import (
	"context"
	"errors"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"

	"oozie"
	"oozie/internal/agent/pi"
	"oozie/internal/web/render"
)

func TestMakeRefusesUnsignedModelBeforeCreatingProject(t *testing.T) {
	s := newTestService(t)
	s.catalog = pi.Catalog{
		DefaultModel: "openai-codex/gpt-5.6-luna",
		Models: []pi.ModelOption{
			{Provider: "openai-codex", ID: "gpt-5.6-luna", Full: "openai-codex/gpt-5.6-luna"},
		},
	}
	s.signedIn = func() map[string]bool { return map[string]bool{} }

	_, err := s.Make(context.Background(), "A page that says hello")
	if err == nil || err.Error() != "This model is not signed in." {
		t.Fatalf("err = %v", err)
	}
	projects, listErr := s.ListProjects(context.Background(), "", "all")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(projects) != 0 {
		t.Fatalf("project was created: %+v", projects)
	}
}

func TestMakeStatusLineReachesTheWaitingFragment(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)
	project, err := s.CreateProject(ctx, "Wait Line", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.repo.GetSession(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	requestID, err := s.repo.CreateAgentRequest(ctx, session.ID, "build", "build the page")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.InsertToolStart(ctx, requestID, "call-1", "write: main.go (running)"); err != nil {
		t.Fatal(err)
	}

	st, err := s.MakeStatus(ctx, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	if st.Phase != "building" || st.Line != "Writing the tool." {
		t.Fatalf("status = %+v", st)
	}

	templatesFS, err := fs.Sub(oozie.Assets, "templates")
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := render.New(templatesFS, "test")
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	renderer.HTML(rec, 200, "partials/make/status", render.ViewData{Data: map[string]any{"Status": st}})
	body := rec.Body.String()
	if !strings.Contains(body, "Writing the tool.") {
		t.Fatalf("fragment missing the live line:\n%s", body)
	}
	if strings.Contains(body, "Building…") {
		t.Fatalf("old frozen line still rendered:\n%s", body)
	}
	if !strings.Contains(body, `hx-get="/fragments/make/`) {
		t.Fatalf("waiting fragment does not keep polling:\n%s", body)
	}
}

func TestMakeSkipsAModelThatDoesNotAnswer(t *testing.T) {
	s := newTestService(t)
	s.catalog = pi.Catalog{
		DefaultModel: "openai-codex/gpt-5.6-luna",
		Models: []pi.ModelOption{
			{Provider: "openrouter", ID: "claude-sonnet-4.6", Full: "openrouter/anthropic/claude-sonnet-4.6"},
			{Provider: "openrouter", ID: "claude-haiku-4.5", Full: "openrouter/anthropic/claude-haiku-4.5"},
			{Provider: "xai", ID: "grok-4.3", Full: "xai/grok-4.3"},
		},
	}
	s.signedIn = func() map[string]bool {
		return map[string]bool{"openrouter": true, "xai": true}
	}
	var tried []string
	s.modelProbe = func(model string) error {
		tried = append(tried, model)
		if strings.HasPrefix(model, "openrouter/") {
			return errors.New(`404 {"error":{"message":"Not Found","code":404}}`)
		}
		return nil
	}

	got, err := s.modelForNewBuild("")
	if err != nil {
		t.Fatal(err)
	}
	if got != "xai/grok-4.3" {
		t.Fatalf("chose %s, tried %#v", got, tried)
	}
	// Cheap models first; one openrouter rejection marks the provider dead, then xai.
	if len(tried) != 2 || tried[0] != "openrouter/anthropic/claude-haiku-4.5" || tried[1] != "xai/grok-4.3" {
		t.Fatalf("probes = %#v", tried)
	}

	s.modelProbe = func(model string) error {
		return errors.New(`404 {"error":{"message":"Not Found","code":404}}`)
	}
	s.deadModels = nil
	_, err = s.Make(context.Background(), "A page that says hello")
	if err == nil || err.Error() != "No model answered." {
		t.Fatalf("err = %v", err)
	}
	projects, listErr := s.ListProjects(context.Background(), "", "all")
	if listErr != nil {
		t.Fatal(listErr)
	}
	if len(projects) != 0 {
		t.Fatalf("project created after every model failed: %+v", projects)
	}
}
