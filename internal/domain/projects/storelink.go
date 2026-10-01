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
	reMetaProperty  = regexp.MustCompile(`(?is)<meta[^>]+property=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	reMetaProperty2 = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]+property=["']([^"']+)["'][^>]*>`)
	reMetaName      = regexp.MustCompile(`(?is)<meta[^>]+name=["']([^"']+)["'][^>]+content=["']([^"']*)["'][^>]*>`)
	reMetaName2     = regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']*)["'][^>]+name=["']([^"']+)["'][^>]*>`)
	reTitle         = regexp.MustCompile(`(?is)<title[^>]*>([^<]+)</title>`)
	reScripts       = regexp.MustCompile(`(?is)<script\b[^>]*>[\s\S]*?</script>`)
	reStyles        = regexp.MustCompile(`(?is)<style\b[^>]*>[\s\S]*?</style>`)
	reTags          = regexp.MustCompile(`(?s)<[^>]+>`)
	reSpace         = regexp.MustCompile(`\s+`)
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
	// Browser-like UA: some store CDNs serve a bare shell or home page to unknown bots.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
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
	// Prefer real product titles over generic store chrome in og:* tags.
	title := cleanText(firstNonEmpty(
		preferProductTitle(meta["og:title"], titleFromHTML(html)),
		preferProductTitle(meta["twitter:title"], titleFromHTML(html)),
		titleFromHTML(html),
		meta["og:title"],
		meta["twitter:title"],
	))
	desc := cleanText(firstNonEmpty(
		meta["og:description"],
		meta["twitter:description"],
		meta["description"],
	))
	// Only use body copy when meta description is missing or generic store chrome.
	// Chrome Web Store HTML bodies are noisy (nav, ratings chrome); a clean og:description wins.
	if desc == "" || isGenericStoreBlurb(desc) {
		if body := storeBodyExcerpt(html, title); body != "" {
			desc = body
		}
	}
	name := storeNameFromTitle(kind, title)
	if name == "" || looksLikeNotAProductPage(title, desc, html) {
		return storeListing{}, ErrValidation{"That page doesn't look like a public app or extension listing. Use a Chrome Web Store, App Store, or Play Store product link."}
	}
	headline := cleanText(firstNonEmpty(meta["og:description"], desc))
	if isGenericStoreBlurb(headline) {
		headline = desc
	}
	if utf8.RuneCountInString(headline) > 160 {
		headline = trimRunes(headline, 157) + "…"
	}
	if desc == "" {
		desc = headline
	}
	if desc == "" || isGenericStoreBlurb(desc) {
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

func isGenericStoreBlurb(s string) bool {
	lower := strings.ToLower(strings.TrimSpace(s))
	switch {
	case lower == "",
		strings.Contains(lower, "add new features to your browser"),
		strings.Contains(lower, "personalize your browsing experience"),
		lower == "chrome web store",
		lower == "google play",
		lower == "app store":
		return true
	}
	return false
}

// preferProductTitle skips generic store-home titles when a real <title> exists.
func preferProductTitle(candidate, fallback string) string {
	candidate = cleanText(candidate)
	if candidate == "" || isGenericStoreBlurb(candidate) {
		return ""
	}
	// "Chrome Web Store" alone is useless; keep compound titles that include a product.
	lower := strings.ToLower(candidate)
	if lower == "chrome web store" || lower == "google play" || lower == "app store" {
		return ""
	}
	_ = fallback
	return candidate
}

// storeBodyExcerpt pulls readable product copy from the HTML body after scripts.
func storeBodyExcerpt(html, title string) string {
	plain := reScripts.ReplaceAllString(html, " ")
	plain = reStyles.ReplaceAllString(plain, " ")
	plain = reTags.ReplaceAllString(plain, " ")
	plain = htmlUnescape(plain)
	plain = reSpace.ReplaceAllString(plain, " ")
	plain = strings.TrimSpace(plain)
	if plain == "" {
		return ""
	}
	// Drop common store chrome prefixes when we can anchor on the product title.
	name := storeNameFromTitle("", title)
	if name == "" {
		name = cleanText(title)
	}
	// Strip chrome-web-store chrome that often precedes the real blurb.
	for _, noise := range []string{
		"Skip to main content", "My extensions & themes", "Developer Dashboard",
		"Give feedback", "Sign in", "Discover Extensions", "Discover apps",
		"Follows recommended practices for Chrome extensions.",
		"Learn more.", "Ratings are updated daily and may not reflect the most recent reviews.",
		"Share Extension", "Add to Chrome",
	} {
		plain = strings.ReplaceAll(plain, noise, " ")
	}
	plain = reSpace.ReplaceAllString(plain, " ")
	plain = strings.TrimSpace(plain)
	lower := strings.ToLower(plain)
	if name != "" {
		if i := strings.Index(lower, strings.ToLower(name)); i >= 0 {
			plain = strings.TrimSpace(plain[i:])
		}
	}
	// Cut before related-product noise when present.
	for _, stop := range []string{
		" Similar ", " People also ", " More by ", " Related ",
		" Data safety", " What's new", " App support",
		" Featured ", // often starts a carousel of other extensions
	} {
		if i := strings.Index(plain, stop); i > 80 {
			plain = plain[:i]
			break
		}
	}
	if utf8.RuneCountInString(plain) > 900 {
		plain = trimRunes(plain, 897) + "…"
	}
	if utf8.RuneCountInString(plain) < 40 || looksLikeStoreChromeNoise(plain) {
		return ""
	}
	return cleanText(plain)
}

func looksLikeStoreChromeNoise(s string) bool {
	lower := strings.ToLower(s)
	hits := 0
	for _, n := range []string{
		"skip to main", "developer dashboard", "my extensions",
		"chrome web store", "add to chrome", "ratings are updated",
		"give feedback", "sign in",
	} {
		if strings.Contains(lower, n) {
			hits++
		}
	}
	return hits >= 2
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
	// Chrome/Play home shell often has only the store name as title.
	t := strings.ToLower(strings.TrimSpace(title))
	if t == "chrome web store" || t == "google play" || t == "app store" {
		return true
	}
	if isGenericStoreBlurb(desc) && (t == "" || strings.Contains(t, "chrome web store")) {
		return true
	}
	// Empty shells with almost no product markup.
	if len(strings.TrimSpace(reTags.ReplaceAllString(html, " "))) < 80 && desc == "" {
		return true
	}
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
	fmt.Fprintf(&b, "Rebuild %s as a small local web tool on this desk, matching the job described on %s.\n\n", listing.Name, source)
	fmt.Fprintf(&b, "Source listing (public page only — not private usage data):\n%s\n\n", listing.URL)
	b.WriteString("What the store says it does:\n")
	b.WriteString(listing.Description)
	b.WriteString("\n\nJob:\n")
	fmt.Fprintf(&b, "Do the same core job as %s, locally, with no store account.\n", listing.Name)
	b.WriteString("Screens:\nOne main screen for that job. Add a second screen only if the listing describes a separate detail or history view.\n")
	b.WriteString("Fields:\n")
	for _, bullet := range planBulletsFromDescription(listing.Description) {
		fmt.Fprintf(&b, "- %s\n", bullet)
	}
	b.WriteString("Saved:\nAnything the user types, under data/ on this desk. Start empty. No imported store data.\n")
	b.WriteString("Done when:\nGET / shows the job from the listing (not a brochure), the fields above work, and go build succeeds. Listen on $ADDR. Footer: Back to desk and Fix.\n")
	b.WriteString("Flow:\nOpen the tool, do the job the listing describes, see the result on the page.\n")
	return b.String()
}

// planBulletsFromDescription turns listing copy into a few concrete goals so
// the fallback plan is not a generic placeholder when the model is offline.
func planBulletsFromDescription(desc string) []string {
	desc = strings.TrimSpace(desc)
	if desc == "" {
		return []string{"Cover the same core job as the listing in a simple local UI."}
	}
	// Prefer sentence splits from the start of the description.
	parts := strings.FieldsFunc(desc, func(r rune) bool {
		return r == '.' || r == '!' || r == '?' || r == '\n'
	})
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		p = strings.Trim(p, "-•* ")
		if utf8.RuneCountInString(p) < 24 {
			continue
		}
		if utf8.RuneCountInString(p) > 140 {
			p = trimRunes(p, 137) + "…"
		}
		// Capitalize lightly for a goal line.
		out = append(out, p+".")
		if len(out) >= 4 {
			break
		}
	}
	if len(out) == 0 {
		out = append(out, "Deliver the listing's main job in one clear local screen: "+trimRunes(desc, 160))
	}
	return out
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
