package hub

import "testing"

func TestFormatDeskName(t *testing.T) {
	got, err := FormatDeskName("  ken ", "c")
	if err != nil || got != "Ken C" {
		t.Fatalf("got %q %v", got, err)
	}
	got, err = FormatDeskName("", "")
	if err != nil || got != "Solo Builder" {
		t.Fatalf("blank = %q %v", got, err)
	}
	if _, err := FormatDeskName("Ken", ""); err == nil {
		t.Fatal("missing initial was accepted")
	}
	if _, err := FormatDeskName("", "C"); err == nil {
		t.Fatal("missing first name was accepted")
	}
	if _, err := FormatDeskName("Ken2", "C"); err == nil {
		t.Fatal("digit was accepted")
	}
	first, initial := SplitDeskName("Ken C")
	if first != "Ken" || initial != "C" {
		t.Fatalf("split = %q %q", first, initial)
	}
	first, initial = SplitDeskName("Solo Builder")
	if first != "" || initial != "" {
		t.Fatalf("solo split = %q %q", first, initial)
	}
}
