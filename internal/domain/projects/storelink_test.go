package projects

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClassifyStoreURL(t *testing.T) {
	cases := []struct {
		in   string
		kind string
		ok   bool
	}{
		{"https://chromewebstore.google.com/detail/example/abcdefghijklmnopqrstuvwxyzabcdef", storeKindChrome, true},
		{"https://chrome.google.com/webstore/detail/example/abcdefghijklmnopqrstuvwxyzabcdef", storeKindChrome, true},
		{"https://apps.apple.com/us/app/numbers/id361304891", storeKindAppStore, true},
		{"https://play.google.com/store/apps/details?id=com.example.app", storeKindPlay, true},
		{"http://play.google.com/store/apps/details?id=com.example.app", "", false},
		{"https://example.com/app", "", false},
		{"https://evil.com/https://play.google.com/store/apps/details?id=x", "", false},
		{"javascript:alert(1)", "", false},
		{"https://play.google.com/store", "", false},
		{"https://apps.apple.com/us/developer/foo/id1", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		kind, canonical, err := classifyStoreURL(c.in)
		if c.ok {
			if err != nil || kind != c.kind || !strings.HasPrefix(canonical, "https://") {
				t.Fatalf("%q: kind=%q can=%q err=%v", c.in, kind, canonical, err)
			}
		} else if err == nil {
			t.Fatalf("%q: expected reject, got kind=%q", c.in, kind)
		}
	}
}

func TestParseStoreHTML(t *testing.T) {
	html := `<html><head>
<title>Focus Timer - Chrome Web Store</title>
<meta property="og:title" content="Focus Timer">
<meta property="og:description" content="A calm timer for deep work sessions.">
<meta name="description" content="A calm timer for deep work sessions.">
</head><body>Focus Timer extension</body></html>`
	listing, err := parseStoreHTML(storeKindChrome, "https://chromewebstore.google.com/detail/focus-timer/abc", html)
	if err != nil {
		t.Fatal(err)
	}
	if listing.Name != "Focus Timer" {
		t.Fatalf("name=%q", listing.Name)
	}
	if !strings.Contains(listing.Description, "calm timer") {
		t.Fatalf("desc=%q", listing.Description)
	}
	plan := synthesizePlan(listing)
	if !strings.Contains(plan, "Focus Timer") || !strings.Contains(plan, "local web tool") {
		t.Fatalf("plan=%q", plan)
	}
	if !strings.Contains(plan, "calm timer") {
		t.Fatalf("fallback plan should use listing copy, got %q", plan)
	}
	rec := recipeFromListing(listing, plan)
	if rec.Kind != recipeKind || len(rec.Prompts) != 1 {
		t.Fatalf("recipe=%+v", rec)
	}
}

func TestParseStoreHTMLRejectsThinPage(t *testing.T) {
	_, err := parseStoreHTML(storeKindPlay, "https://play.google.com/store/apps/details?id=x", `<html><title>Sign in</title></html>`)
	if err == nil {
		t.Fatal("expected rejection of thin/login page")
	}
}

func TestParseStoreHTMLRejectsChromeHomeShell(t *testing.T) {
	html := `<html><head><title>Chrome Web Store</title>
<meta property="og:title" content="Chrome Web Store">
<meta property="og:description" content="Add new features to your browser and personalize your browsing experience.">
</head><body>Chrome Web Store</body></html>`
	if _, err := parseStoreHTML(storeKindChrome, "https://chromewebstore.google.com/", html); err == nil {
		t.Fatal("generic Chrome home must be rejected")
	}
}

func TestParseStoreHTMLPrefersBodyOverGenericMeta(t *testing.T) {
	html := `<html><head>
<title>Note Nest - Chrome Web Store</title>
<meta property="og:title" content="Chrome Web Store">
<meta property="og:description" content="Add new features to your browser and personalize your browsing experience.">
</head><body><h1>Note Nest</h1><p>Note Nest keeps sticky notes beside any tab and syncs nothing to the cloud.</p></body></html>`
	listing, err := parseStoreHTML(storeKindChrome, "https://chromewebstore.google.com/detail/note-nest/abcdefghijklmnopqrstuvwxyzabcdef", html)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing.Description, "sticky notes") {
		t.Fatalf("desc=%q", listing.Description)
	}
	if listing.Name == "Chrome Web Store" || listing.Name == "" {
		// title tag still has the product name even when og:title is generic.
		if listing.Name != "Note Nest" && !strings.Contains(listing.Name, "Note Nest") {
			t.Fatalf("name=%q", listing.Name)
		}
	}
}

func TestRecipeDraftLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestService(t)

	orig := fetchStoreListing
	fetchStoreListing = func(ctx context.Context, kind, canonical string) (storeListing, error) {
		return storeListing{
			Kind:        kind,
			URL:         canonical,
			Name:        "Pocket Ledger",
			Headline:    "Track simple expenses",
			Description: "Log spending offline with categories and a monthly total.",
		}, nil
	}
	t.Cleanup(func() { fetchStoreListing = orig })

	// Bad link never hits fetch.
	if _, err := s.ProposeRecipeFromLink(ctx, "https://news.ycombinator.com/"); err == nil {
		t.Fatal("non-store link must be rejected")
	}

	draft, err := s.ProposeRecipeFromLink(ctx, "https://play.google.com/store/apps/details?id=com.example.ledger")
	if err != nil {
		t.Fatal(err)
	}
	if draft.Status != "pending" || draft.Name != "Pocket Ledger" || draft.Plan == "" {
		t.Fatalf("draft=%+v", draft)
	}
	var rec Recipe
	if err := json.Unmarshal([]byte(draft.RecipeJSON), &rec); err != nil || rec.Name != "Pocket Ledger" {
		t.Fatalf("recipe json: %v %+v", err, rec)
	}

	// Reject discards without a project.
	if err := s.RejectRecipeDraft(ctx, draft.ID); err != nil {
		t.Fatal(err)
	}
	gone, err := s.GetRecipeDraft(ctx, draft.ID)
	if err != nil || gone.Status != "rejected" {
		t.Fatalf("after reject: %+v err=%v", gone, err)
	}
	if _, err := s.AcceptRecipeDraft(ctx, draft.ID); err == nil {
		t.Fatal("accept after reject must fail")
	}

	// Fresh draft → edit → build attempt (agent missing still creates project).
	draft2, err := s.ProposeRecipeFromLink(ctx, "https://apps.apple.com/us/app/pocket-ledger/id123456789")
	if err != nil {
		t.Fatal(err)
	}
	edited := "Build a tiny local expense log with categories and a monthly total. Keep it offline under data/."
	project, err := s.EditRecipeDraft(ctx, draft2.ID, edited)
	if _, ok := err.(ErrValidation); !ok {
		t.Fatalf("expected agent validation error, got %v", err)
	}
	if project.ID == 0 || project.Name != "Pocket Ledger" {
		t.Fatalf("project=%+v", project)
	}
	settled, _ := s.GetRecipeDraft(ctx, draft2.ID)
	if settled.Status != "accepted" || settled.Plan != edited {
		t.Fatalf("settled=%+v", settled)
	}
	var rec2 Recipe
	_ = json.Unmarshal([]byte(settled.RecipeJSON), &rec2)
	if len(rec2.Prompts) != 1 || !strings.Contains(rec2.Prompts[0], edited) {
		t.Fatalf("adapted prompts=%v", rec2.Prompts)
	}

	// Accept path on a third draft.
	draft3, _ := s.ProposeRecipeFromLink(ctx, "https://chromewebstore.google.com/detail/pocket-ledger/abcdefghijklmnopqrstuvwxyzabcdef")
	p3, err := s.AcceptRecipeDraft(ctx, draft3.ID)
	if _, ok := err.(ErrValidation); !ok {
		t.Fatalf("expected agent validation error, got %v", err)
	}
	if p3.ID == 0 {
		t.Fatal("accept should still create a project")
	}

	home, _ := os.UserHomeDir()
	_ = os.RemoveAll(filepath.Join(home, "Projects", "pocket-ledger"))
}

func TestThinStoreChromeDoesNotBecomeASuite(t *testing.T) {
	if !isThinStoreChrome("Download Goodnotes. See screenshots, ratings and reviews.") {
		t.Fatal("expected thin chrome")
	}
	plan := synthesizePlan(storeListing{
		Kind:        storeKindAppStore,
		Name:        "Goodnotes",
		URL:         "https://apps.apple.com/app/id1",
		Description: "Download Goodnotes. See screenshots, ratings and reviews.",
	})
	if strings.Contains(plan, "See screenshots") {
		t.Fatalf("plan echoed store chrome: %q", plan)
	}
	if !strings.Contains(plan, "under 180 lines") || !strings.Contains(plan, "Goodnotes") {
		t.Fatalf("plan=%q", plan)
	}
}
