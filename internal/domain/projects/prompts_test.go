package projects

import (
	"strings"
	"testing"
)

func TestPageBuildMessageLeadsWithTheRequest(t *testing.T) {
	msg := pageBuildMessage("A grocery list I can check off")
	req := strings.Index(msg, "A grocery list I can check off")
	bar := strings.Index(msg, "Quality bar")
	contract := strings.Index(msg, "Desk contract")
	if req < 0 || bar < 0 || contract < 0 || !(req < bar && bar < contract) {
		t.Fatalf("request should lead the prompt, got %q", msg)
	}
	if !strings.Contains(msg, "5-line spec") {
		t.Fatal("missing done-spec")
	}
}

func TestWishBuildMessageExpandsVagueWishes(t *testing.T) {
	msg := wishBuildMessage("something for plants")
	if !strings.Contains(msg, "something for plants") || !strings.Contains(msg, "smallest set") {
		t.Fatalf("wish prompt=%q", msg)
	}
}

func TestIncompleteNudgeRepeatsTheJob(t *testing.T) {
	msg := incompleteScaffoldNudge("track the books I lend")
	if !strings.Contains(msg, "track the books I lend") || !strings.Contains(msg, "not done") {
		t.Fatalf("nudge=%q", msg)
	}
}

func TestTasteRulesSkipPlaceholder(t *testing.T) {
	if tasteRules(defaultTaste) != "" {
		t.Fatalf("placeholder taste should inject nothing, got %q", tasteRules(defaultTaste))
	}
	rules := tasteRules(strings.Replace(defaultTaste, "## Signals", "- always dark-mode first\n## Signals", 1) + "\n- 2026-01-01 noise\n")
	if !strings.Contains(rules, "dark-mode first") || strings.Contains(rules, "noise") {
		t.Fatalf("rules=%q", rules)
	}
}

func TestRestrainPlanDropsASuite(t *testing.T) {
	plan := restrainPlan("Goodnotes", "Job: notebook grid, drawing canvas, PDF import, and pen tools.")
	if !strings.Contains(plan, "Ignored a larger plan") {
		t.Fatalf("suite plan was not replaced: %q", plan)
	}
	if !strings.Contains(plan, "Goodnotes") || !strings.Contains(plan, "one local page") {
		t.Fatalf("plan=%q", plan)
	}
}

func TestBuildPromptsIncludeScopeLimit(t *testing.T) {
	for _, msg := range []string{
		pageBuildMessage("a list"),
		wishBuildMessage("plants"),
		incompleteScaffoldNudge("books"),
		improvementMessage("Timer", "pause"),
		remixMessage("Timer", "tea"),
		recipeBuildMessage("Notes", "local notes", "a list", []string{"one page"}),
	} {
		if !strings.Contains(msg, "under 180 lines") {
			t.Fatalf("missing scope restraint: %q", msg)
		}
	}
}

func TestImprovementAndRemixNameTheVisibleChange(t *testing.T) {
	fix := improvementMessage("Timer", "add a pause button")
	if !strings.Contains(fix, "pause button") || !strings.Contains(fix, "visible on GET /") {
		t.Fatalf("fix=%q", fix)
	}
	remix := remixMessage("Timer", "make it a tea timer")
	if !strings.Contains(remix, "tea timer") || !strings.Contains(remix, "renamed title alone") {
		t.Fatalf("remix=%q", remix)
	}
}
