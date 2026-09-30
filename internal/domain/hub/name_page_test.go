package hub

import (
	"context"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"

	"oozie"
	"oozie/internal/web/render"
)

func TestCreatedNameShowsOnSidebar(t *testing.T) {
	_, desk := openDesk(t)
	if err := desk.SaveIdentity(context.Background(), "Ada", "L", "Proof Circle"); err != nil {
		t.Fatal(err)
	}
	ident, err := desk.Identity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ident.DisplayName != "Ada L" || ident.FirstName != "Ada" || ident.LastInitial != "L" {
		t.Fatalf("stored name = %+v", ident)
	}
	if ident.CircleName != "Proof Circle" {
		t.Fatalf("circle = %q", ident.CircleName)
	}

	templatesFS, err := fs.Sub(oozie.Assets, "templates")
	if err != nil {
		t.Fatal(err)
	}
	renderer, err := render.New(templatesFS, "test")
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandlers(desk, renderer)
	rec := httptest.NewRecorder()
	h.SidebarFragment(rec, httptest.NewRequest("GET", "/fragments/sidebar", nil))
	body := rec.Body.String()
	if !strings.Contains(body, "Ada L") || !strings.Contains(body, "Proof Circle") {
		t.Fatalf("sidebar missing the created name:\\n%s", body)
	}
	if strings.Contains(body, "Solo Builder") {
		t.Fatalf("sidebar still says Solo Builder:\\n%s", body)
	}
}
