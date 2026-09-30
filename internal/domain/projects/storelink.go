package projects

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// Store kinds allowed as recipe sources. Everything else is refused before
// any network call.
const (
	storeKindChrome   = "chrome"
	storeKindAppStore = "appstore"
	storeKindPlay     = "play"
)

// storeListing is the public marketing text scraped from an allowlisted
// product page. It never includes private usage data.
type storeListing struct {
	Kind        string
	URL         string
	Name        string
	Headline    string
	Description string
}

// fetchStoreListing is swappable in tests so URL guards and draft flow can
// run without the network.
var fetchStoreListing = defaultFetchStoreListing

var storeHTTPClient = &http.Client{
	Timeout: 15 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return fmt.Errorf("too many redirects")
		}
		if err := assertAllowlistedStoreURL(req.URL); err != nil {
			return err
		}
		return nil
	},
}

var (
	reMetaProperty = regexp.MustCompile(`(?is)<meta[^>]+property=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	reMetaProperty2 = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]+property=["']([^"']+)["'][^>]*>`)
	reMetaName     = regexp.MustCompile(`(?is)<meta[^>]+name=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	reMetaName2    = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]+name=["']([^"']+)["'][^>]*>`)
	reTitle        = regexp.MustCompile(`(?is)<title[^>]*>([^<]+)</title>`)
	reTags         = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace        = regexp.MustCompile(`\s+`)
)

// classifyStoreURL accepts only Chrome Web Store, Apple App Store, and
// Google Play product links. Returns kind + canonical https URL.
func classifyStoreURL(raw string) (kind, canonical string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", ErrValidation{"Paste a Chrome Web Store, Apple App Store, or Google Play link."}
	}
	// Block obvious non-http schemes before parse surprises.
	lower := strings.ToLower(raw)
	if strings.HasPrefix(lower, "javascript:") ||
		strings.HasPrefix(lower, "data:") ||
		strings.HasPrefix(lower, "file:") ||
		strings.HasPrefix(lower, "ftp:") {
		return "", "", ErrValidation{storeLinkRejectMsg}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", "", ErrValidation{storeLinkRejectMsg}
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if u.Scheme != "https" {
		return "", "", ErrValidation{"Only https store links are accepted."}
	}
	if err := assertAllowlistedStoreURL(u); err != nil {
		return "", "", err
	}
	kind, ok := storeKindForHostPath(u.Hostname(), u.EscapedPath(), u.RawQuery)
	if !ok {
		return "", "", ErrValidation{storeLinkRejectMsg}
	}
	// Drop fragments and normalize.
	u.Fragment = ""
	u.RawFragment = ""
	return kind, u.String(), nil
}

const storeLinkRejectMsg = "That link is not a Chrome Web Store, Apple App Store, or Google Play product page. Paste one of those and try again."

func assertAllowlistedStoreURL(u *url.URL) error {
	if u == nil {
		return ErrValidation{storeLinkRejectMsg}
	}
	if strings.ToLower(u.Scheme) != "https" {
		return ErrValidation{"Only https store links are accepted."}
	}
	host := strings.ToLower(u.Hostname())
	// No userinfo tricks (https://user@evil/).
	if u.User != nil {
		return ErrValidation{storeLinkRejectMsg}
	}
	switch host {
	case "chromewebstore.google.com", "chrome.google.com", "apps.apple.com", "play.google.com":
		return nil
	default:
		return ErrValidation{storeLinkRejectMsg}
	}
}

func storeKindForHostPath(host, path, query string) (string, bool) {
	host = strings.ToLower(host)
	path = strings.ToLower(path)
	switch host {
	case "chromewebstore.google.com":
		if strings.HasPrefix(path, "/detail/") {
			return storeKindChrome, true
		}
	case "chrome.google.com":
		if strings.HasPrefix(path, "/webstore/detail/") {
			return storeKindChrome, true
		}
	case "apps.apple.com":
		// /app/.../id123 or /xx/app/.../id123 — require a numeric App Store id.
		if strings.Contains(path, "/app") {
			if matched, _ := regexp.MatchString(`/id\d+`, path); matched {
				return storeKindAppStore, true
			}
		}
	case "play.google.com":
		if strings.HasPrefix(path, "/store/apps/details") && strings.Contains(query, "id=") {
			return storeKindPlay, true
		}
	}
	return "", false
}

func defaultFetchStoreListing(ctx context.Context, kind, canonical string) (storeListing, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, canonical, nil)
	if err != nil {
		return storeListing{}, ErrValidation{"Couldn't open that store link."}
	}
	req.Header.Set("User-Agent", "oozie-desk/1.0 (+local recipe import; public product pages only)")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	resp, err := storeHTTPClient.Do(req)
	if err != nil {
		return storeListing{}, ErrValidation{"Couldn't reach that store page. Check the link and try again."}
	}
	defer resp.Body.Close()
	if resp.Request != nil && resp.Request.URL != nil {
		if err := assertAllowlistedStoreURL(resp.Request.URL); err != nil {
			return storeListing{}, err
		}
		// Final URL must still be a product page of the same kind family.
		if _, ok := storeKindForHostPath(resp.Request.URL.Hostname(), resp.Request.URL.EscapedPath(), resp.Request.URL.RawQuery); !ok {
			return storeListing{}, ErrValidation{storeLinkRejectMsg}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return storeListing{}, ErrValidation{"That store page didn't load (" + resp.Status + "). Try another link."}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return storeListing{}, ErrValidation{"Couldn't read that store page."}
	}
	listing, err := parseStoreHTML(kind, canonical, string(body))
	if err != nil {
		return storeListing{}, err
	}
	return listing, nil
}

func parseStoreHTML(kind, canonical, html string) (storeListing, error) {
	meta := collectMeta(html)
	title := cleanText(firstNonEmpty(
		meta["og:title"],
		meta["twitter:title"],
		meta["og:site_name"],
		titleFromHTML(html),
	))
	desc := cleanText(firstNonEmpty(
		meta["og:description"],
		meta["twitter:description"],
		meta["description"],
	))
	name := storeNameFromTitle(kind, title)
	if name == "" || looksLikeNotAProductPage(title, desc, html) {
		return storeListing{}, ErrValidation{"That page doesn't look like a public app or extension listing. Use a Chrome Web Store, App Store, or Play Store product link."}
	}
	headline := cleanText(firstNonEmpty(meta["og:description"], desc))
	if utf8.RuneCountInString(headline) > 160 {
		headline = trimRunes(headline, 157) + "…"
	}
	if desc == "" {
		desc = headline
	}
	if desc == "" {
		return storeListing{}, ErrValidation{"That listing has no description to work from. Try another link."}
	}
	return storeListing{
		Kind:        kind,
		URL:         canonical,
		Name:        name,
		Headline:    headline,
		Description: desc,
	}, nil
}

func looksLikeNotAProductPage(title, desc, html string) bool {
	blob := strings.ToLower(title + " " + desc)
	// Hard rejects: login walls, generic home, wrong properties.
	for _, bad := range []string{
		"sign in", "log in", "access denied", "404", "not found",
		"just a moment", "attention required", "captcha",
	} {
		if strings.Contains(blob, bad) && len(desc) < 40 {
			return true
		}
	}
	lower := strings.ToLower(html)
	// Empty shells with almost no product markup.
	if len(strings.TrimSpace(reTags.ReplaceAllString(html, " "))) < 80 && desc == "" {
		return true
	}
	_ = lower
	return false
}

func collectMeta(html string) map[string]string {
	out := map[string]string{}
	add := func(k, v string) {
		k = strings.ToLower(strings.TrimSpace(k))
		v = strings.TrimSpace(htmlUnescape(v))
		if k == "" || v == "" {
			return
		}
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	for _, m := range reMetaProperty.FindAllStringSubmatch(html, -1) {
		add(m[1], m[2])
	}
	for _, m := range reMetaProperty2.FindAllStringSubmatch(html, -1) {
		add(m[2], m[1])
	}
	for _, m := range reMetaName.FindAllStringSubmatch(html, -1) {
		add(m[1], m[2])
	}
	for _, m := range reMetaName2.FindAllStringSubmatch(html, -1) {
		add(m[2], m[1])
	}
	return out
}

func titleFromHTML(html string) string {
	m := reTitle.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	return htmlUnescape(m[1])
}

func storeNameFromTitle(kind, title string) string {
	title = cleanText(title)
	if title == "" {
		return ""
	}
	// Strip common store suffixes.
	cut := []string{
		" - Chrome Web Store",
		" – Chrome Web Store",
		" | Chrome Web Store",
		" on the App Store",
		" - App Store",
		" – App Store",
		" - Apps on Google Play",
		" – Apps on Google Play",
		" on Google Play",
		" - Google Play",
	}
	for _, c := range cut {
		if i := strings.Index(title, c); i > 0 {
			title = strings.TrimSpace(title[:i])
		}
	}
	// Chrome titles sometimes "Name - short pitch"
	if kind == storeKindChrome {
		if i := strings.Index(title, " - "); i > 2 {
			title = strings.TrimSpace(title[:i])
		}
	}
	title = trimRunes(title, 80)
	return title
}

func synthesizePlan(listing storeListing) string {
	source := "a public product listing"
	switch listing.Kind {
	case storeKindChrome:
		source = "a Chrome Web Store extension listing"
	case storeKindAppStore:
		source = "an Apple App Store listing"
	case storeKindPlay:
		source = "a Google Play Store listing"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "We'll rebuild **%s** as a small local web tool on this desk.\n\n", listing.Name)
	fmt.Fprintf(&b, "This plan comes from %s (not from anyone's private usage data):\n%s\n\n", source, listing.URL)
	b.WriteString("What the store says it does:\n")
	b.WriteString(listing.Description)
	b.WriteString("\n\n")
	b.WriteString("What will be built here:\n")
	b.WriteString("- A single local Go web page that covers the same job in a simple way.\n")
	b.WriteString("- Sensible defaults — no account system, no store APIs, no cloning proprietary code.\n")
	b.WriteString("- If anything is saved, it lives only under data/ on this desk.\n")
	b.WriteString("- Footer links: Back to desk and Fix.\n")
	b.WriteString("- Verify with `go build` and listen on $ADDR.\n\n")
	b.WriteString("This is a fresh tool inspired by the listing's purpose, not a binary or data import from the original app.")
	return b.String()
}

func recipeFromListing(listing storeListing, plan string) Recipe {
	headline := listing.Headline
	if headline == "" {
		headline = trimRunes(listing.Description, 120)
	}
	prompt := fmt.Sprintf(
		"Build one small local tool inspired by this public store listing.\n\nName: %s\nSource (%s): %s\n\nStore description:\n%s\n\nBuild plan (follow this):\n%s\n\nRules:\n- Go web app at the project root; prefer the standard library.\n- Listen on $ADDR (or 127.0.0.1:$PORT).\n- Do not call the original app's APIs or embed its code, assets, or user data.\n- Durable records only under data/ (or $OOZIE_DATA_DIR).\n- Footer: Back to desk ($OOZIE_DESK_URL, target=_top) and Fix (improve URL when set).\n- Verify with: go build -o /tmp/app .",
		listing.Name, listing.Kind, listing.URL, listing.Description, plan,
	)
	return Recipe{
		Kind:        recipeKind,
		Name:        listing.Name,
		Headline:    headline,
		Description: listing.Description,
		Prompts:     []string{prompt},
		ExportedAt:  time.Now().UTC(),
	}
}

func recipeFromEditedPlan(base Recipe, plan string) Recipe {
	plan = strings.TrimSpace(plan)
	base.Prompts = []string{fmt.Sprintf(
		"Build one small local tool from this accepted plan (edited by the user after reviewing a store listing).\n\nApp name: %s\n\nPlan:\n%s\n\nRules:\n- Go web app; listen on $ADDR.\n- No third-party store APIs; no proprietary code copy.\n- Durable records only under data/.\n- Footer: Back to desk and Fix.\n- Verify with: go build -o /tmp/app .",
		base.Name, plan,
	)}
	if base.Description == "" {
		base.Description = trimRunes(plan, 280)
	}
	base.ExportedAt = time.Now().UTC()
	return base
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func cleanText(s string) string {
	s = htmlUnescape(s)
	s = reTags.ReplaceAllString(s, " ")
	s = reSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

func trimRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n])
}

func htmlUnescape(s string) string {
	repl := []struct{ old, new string }{
		{"&amp;", "&"},
		{"&lt;", "<"},
		{"&gt;", ">"},
		{"&quot;", `"`},
		{"&#39;", "'"},
		{"&apos;", "'"},
		{"&#x27;", "'"},
		{"&nbsp;", " "},
	}
	for _, r := range repl {
		s = strings.ReplaceAll(s, r.old, r.new)
	}
	return s
}

func sourceKindLabel(kind string) string {
	switch kind {
	case storeKindChrome:
		return "Chrome Web Store"
	case storeKindAppStore:
		return "Apple App Store"
	case storeKindPlay:
		return "Google Play"
	default:
		return kind
	}
}
