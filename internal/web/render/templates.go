package render

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"html/template"
	"io/fs"
	"net/http"
	"time"
)

type Renderer struct {
	templates *template.Template
}

// New parses all templates from the given filesystem (rooted at the
// templates directory). cssStamp is a cache-busting token appended to the
// stylesheet URL — pass a hash of the stylesheet contents.
func New(templatesFS fs.FS, cssStamp string) (*Renderer, error) {
	renderer := &Renderer{}

	patterns := []string{
		"layouts/*.html",
		"pages/*/*.html",
		"partials/*/*.html",
	}

	tmpl := template.New("app").Funcs(template.FuncMap{
		"dict":       dict,
		"include":    renderer.include,
		"cssVersion": func() string { return cssStamp },
		"ints":       intRange,
		"ts":         humanTime,
		"ago":        humanAgo,
		"tokens":     humanTokens,
	})
	for _, pattern := range patterns {
		matches, err := fs.Glob(templatesFS, pattern)
		if err != nil {
			return nil, err
		}
		if len(matches) == 0 {
			continue
		}
		if _, err := tmpl.ParseFS(templatesFS, matches...); err != nil {
			return nil, err
		}
	}

	renderer.templates = tmpl
	return renderer, nil
}

// CSSStamp hashes a file's contents for use as the cssVersion token.
func CSSStamp(fsys fs.FS, path string) string {
	body, err := fs.ReadFile(fsys, path)
	if err != nil {
		return "0"
	}
	h := fnv.New32a()
	_, _ = h.Write(body)
	return fmt.Sprintf("%x", h.Sum32())
}

func (r *Renderer) HTML(w http.ResponseWriter, status int, name string, data ViewData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := r.templates.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, "template render error", http.StatusInternalServerError)
	}
}

// include renders a named template to HTML so layouts can embed a
// per-request page body (html/template has no dynamic template calls).
func (r *Renderer) include(name string, data any) (template.HTML, error) {
	if name == "" {
		return "", fmt.Errorf("include: empty template name")
	}
	var buf bytes.Buffer
	if err := r.templates.ExecuteTemplate(&buf, name, data); err != nil {
		return "", err
	}
	return template.HTML(buf.String()), nil
}

// humanTokens shortens token counts: 950 → "950", 12_340 → "12.3k".
func humanTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	if n < 1_000_000 {
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	}
	return fmt.Sprintf("%.2fM", float64(n)/1_000_000)
}

// humanTime renders timestamps in a compact local format instead of Go's
// raw time.Time string.
func humanTime(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("Jan 2, 2006 · 3:04 PM")
}

// humanAgo renders a timestamp as a compact relative duration ("2h ago").
// Accepts time.Time or *time.Time since model timestamps come both ways.
func humanAgo(v any) string {
	var t time.Time
	switch x := v.(type) {
	case time.Time:
		t = x
	case *time.Time:
		if x != nil {
			t = *x
		}
	}
	if t.IsZero() {
		return "—"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	default:
		return t.Local().Format("Jan 2, 2006")
	}
}

// intRange yields [from, to) for template loops like hour pickers.
func intRange(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

func dict(values ...any) map[string]any {
	result := map[string]any{}
	for i := 0; i+1 < len(values); i += 2 {
		key, ok := values[i].(string)
		if !ok {
			continue
		}
		result[key] = values[i+1]
	}
	return result
}
