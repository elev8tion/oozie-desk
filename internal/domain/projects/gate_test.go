package projects

import (
	"context"
	"errors"
	"strings"
	"testing"

	"oozie-desk/internal/agent/pi"
)

func TestCheckedJobDropsAgentWrappers(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()

	recipe, err := s.CreateProject(ctx, "Sourdough Log", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SaveDraft(ctx, PublishDraft{
		ProjectID:   recipe.ID,
		AppName:     "Sourdough Log",
		Headline:    "Track feedings",
		Description: "A local log of starter feedings",
	}); err != nil {
		t.Fatal(err)
	}
	wrapper := recipeBuildMessage("Sourdough Log", "Track feedings", "A local log of starter feedings", []string{"track sourdough feedings"})
	got := s.checkedJob(ctx, recipe.ID, wrapper)
	if strings.HasPrefix(got, "Rebuild this tool") || strings.Contains(got, "Quality bar") || strings.Contains(got, "Prompts that shaped") {
		t.Fatalf("recipe wrapper used as the job: %s", got)
	}
	for _, want := range []string{"Sourdough Log", "Track feedings", "A local log of starter feedings"} {
		if !strings.Contains(got, want) {
			t.Fatalf("job %q missing %q", got, want)
		}
	}

	sentence := "track sourdough feedings"
	name := wishProjectName(sentence)
	if strings.HasPrefix(name, "Wish ") {
		name = "Tool " + name[len("Wish "):]
	}
	makeProj, err := s.CreateProject(ctx, name, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SaveDraft(ctx, PublishDraft{
		ProjectID:   makeProj.ID,
		AppName:     name,
		Headline:    wishHeadline(sentence),
		Description: sentence,
	}); err != nil {
		t.Fatal(err)
	}
	got = s.checkedJob(ctx, makeProj.ID, pageBuildMessage(sentence))
	if got != sentence {
		t.Fatalf("make job = %q, want the desk sentence", got)
	}

	remix, err := s.CreateProject(ctx, "Sourdough Remix", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	mutation := "show the last feeding time on the page"
	if err := s.repo.SaveDraft(ctx, PublishDraft{
		ProjectID:   remix.ID,
		AppName:     "Sourdough Remix",
		Headline:    "renamed title",
		Description: mutation,
	}); err != nil {
		t.Fatal(err)
	}
	got = s.checkedJob(ctx, remix.ID, remixMessage("Sourdough", mutation))
	if got != mutation {
		t.Fatalf("remix job = %q, want the mutation", got)
	}

	if err := s.AddWish(ctx, "count starter jars"); err != nil {
		t.Fatal(err)
	}
	wishes, err := s.ListWishes(ctx)
	if err != nil || len(wishes) == 0 {
		t.Fatalf("wishes=%v err=%v", wishes, err)
	}
	s.wishByRequest.Store(int64(77), wishes[0].ID)
	if got := s.jobText(ctx, recipe.ID, 77); got != "count starter jars" {
		t.Fatalf("wish job = %q", got)
	}

	note := "show yesterday's feeding"
	if got := s.checkedJob(ctx, recipe.ID, note); got != note {
		t.Fatalf("fix note = %q", got)
	}
}

func TestAcceptPageJudgesTheJobNotTheWrapper(t *testing.T) {
	s := newTestService(t)
	var seen string
	s.UsePageProbe(func(workdir, request string) (bool, string, string) {
		seen = request
		return false, "GET / does not show the job (sourdough).", "<h1>Hello</h1>"
	})
	ctx := context.Background()
	p, err := s.CreateProject(ctx, "Sourdough Log", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SaveDraft(ctx, PublishDraft{
		ProjectID:   p.ID,
		AppName:     "Sourdough Log",
		Headline:    "Track feedings",
		Description: "A local log of starter feedings",
	}); err != nil {
		t.Fatal(err)
	}
	wrapper := recipeBuildMessage("Sourdough Log", "Track feedings", "A local log of starter feedings", []string{"track sourdough feedings"})
	proceed, _ := s.acceptPage(ctx, p.ID, wrapper, nil)
	if proceed {
		t.Fatal("wrapper page was accepted")
	}
	if strings.HasPrefix(seen, "Rebuild this tool") || strings.Contains(seen, "Quality bar") {
		t.Fatalf("probe saw the wrapper: %s", seen)
	}
	if !strings.Contains(seen, "Sourdough Log") || !strings.Contains(seen, "starter feedings") {
		t.Fatalf("probe saw %q", seen)
	}
}

func TestPublishRefusesAMiss(t *testing.T) {
	s := newTestService(t)
	s.builder = fakeBuilder{}
	var seen string
	s.UsePageProbe(func(workdir, request string) (bool, string, string) {
		seen = request
		return false, "GET / does not show the job (sourdough).", "<h1>Hello</h1>"
	})
	ctx := context.Background()
	p, err := s.CreateProject(ctx, "Sourdough Log", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SaveDraft(ctx, PublishDraft{
		ProjectID:   p.ID,
		AppName:     "Sourdough Log",
		Headline:    "Track feedings",
		Description: "A local log of starter feedings",
	}); err != nil {
		t.Fatal(err)
	}
	err = s.Publish(ctx, p.ID)
	if err == nil || !strings.Contains(err.Error(), "sourdough") {
		t.Fatalf("publish err = %v", err)
	}
	if strings.HasPrefix(seen, "Rebuild this tool") || !strings.Contains(seen, "Sourdough Log") {
		t.Fatalf("publish checked %q", seen)
	}
	jobs, err := s.ListJobs(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) == 0 {
		t.Fatal("miss did not record a failed job")
	}
	for _, job := range jobs {
		if job.Status != "failed" {
			t.Fatalf("job status = %s, want failed", job.Status)
		}
	}
	apps, err := s.ListStoreApps(ctx, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 0 {
		t.Fatalf("publish stored a miss: %+v", apps)
	}
}

func TestRefusedModelDoesNotKillProvider(t *testing.T) {
	s := newTestService(t)
	ctx := context.Background()
	s.noteModelRefusal("openrouter/anthropic/claude-haiku-4.5", errors.New("404 not found"))
	if !s.deadModels["openrouter/anthropic/claude-haiku-4.5"] {
		t.Fatal("refused model should be dead")
	}
	if s.deadModels["provider:openrouter"] {
		t.Fatal("a refused model killed its provider")
	}
	s.noteModelRefusal("xai/grok-4.3", errors.New("no API key for xai"))
	if !s.deadModels["xai/grok-4.3"] || !s.deadModels["provider:xai"] {
		t.Fatalf("missing key should mark the provider: %#v", s.deadModels)
	}

	p, err := s.CreateProject(ctx, "Hop", t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	session, err := s.repo.GetSession(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.repo.SetSessionModel(ctx, session.ID, "openrouter/anthropic/claude-sonnet-4.6"); err != nil {
		t.Fatal(err)
	}
	s.deadModels = nil
	s.AgentError(p.ID, 1, "credit balance too low")
	if !s.deadModels["openrouter/anthropic/claude-sonnet-4.6"] || s.deadModels["provider:openrouter"] {
		t.Fatalf("agent refusal scope = %#v", s.deadModels)
	}

	s.catalog = pi.Catalog{
		Models: []pi.ModelOption{
			{Provider: "openrouter", ID: "claude-haiku-4.5", Full: "openrouter/anthropic/claude-haiku-4.5"},
			{Provider: "openrouter", ID: "claude-sonnet-4.6", Full: "openrouter/anthropic/claude-sonnet-4.6"},
			{Provider: "xai", ID: "grok-4.3", Full: "xai/grok-4.3"},
		},
	}
	s.signedIn = func() map[string]bool {
		return map[string]bool{"openrouter": true, "xai": true}
	}
	s.deadModels = nil
	next, err := s.modelForRetry(ctx, "openrouter/anthropic/claude-haiku-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if s.deadModels["provider:openrouter"] {
		t.Fatal("retry marked the provider dead")
	}
	if next != "openrouter/anthropic/claude-sonnet-4.6" {
		t.Fatalf("sibling model hidden, next=%s dead=%#v", next, s.deadModels)
	}
}
