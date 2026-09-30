package hub

import (
	"errors"
	"strings"
	"unicode"
)

const soloName = "Solo Builder"

// FormatDeskName stores a chosen name as "First L". Both fields blank keeps
// the Solo Builder label. A half-filled name is refused.
func FormatDeskName(first, initial string) (string, error) {
	first = strings.Join(strings.Fields(first), " ")
	initial = strings.TrimSuffix(strings.TrimSpace(initial), ".")
	if first == "" && initial == "" {
		return soloName, nil
	}
	if first == "" {
		return "", errors.New("Your name is required.")
	}
	if len([]rune(initial)) != 1 || !unicode.IsLetter([]rune(initial)[0]) {
		return "", errors.New("Last initial is one letter.")
	}
	if len([]rune(first)) > 40 {
		return "", errors.New("Name and circle must be 80 characters or fewer.")
	}
	for _, r := range first {
		if unicode.IsLetter(r) || r == ' ' || r == '-' || r == '\'' {
			continue
		}
		return "", errors.New("First name uses letters only.")
	}
	return tidyFirst(first) + " " + strings.ToUpper(initial), nil
}

// SplitDeskName fills the settings fields from a stored "First L" name.
// Solo Builder, or any older free-form name, leaves the fields blank.
func SplitDeskName(display string) (string, string) {
	display = strings.TrimSpace(display)
	if display == "" || display == soloName {
		return "", ""
	}
	i := strings.LastIndex(display, " ")
	if i <= 0 {
		return "", ""
	}
	initial := strings.TrimSuffix(display[i+1:], ".")
	if len([]rune(initial)) != 1 || !unicode.IsLetter([]rune(initial)[0]) {
		return "", ""
	}
	return display[:i], strings.ToUpper(initial)
}

func tidyFirst(s string) string {
	rs := []rune(strings.ToLower(s))
	capNext := true
	for i, r := range rs {
		if r == ' ' || r == '-' || r == '\'' {
			capNext = true
			continue
		}
		if capNext {
			rs[i] = unicode.ToUpper(r)
		}
		capNext = false
	}
	return string(rs)
}
