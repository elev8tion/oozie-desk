package projects

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"oozie-desk/internal/build"
)

const oozieProbeRow = "oozie-probe-row"

// JudgePage reports whether HTML does the user's job. A compiling page is
// not enough: the body must show a noun from the request, not only footer links.
func JudgePage(request, html string) (bool, string) {
	body := visibleText(html)
	if strings.TrimSpace(body) == "" || !looksLikeHTML(html) {
		return false, "GET / did not return a usable HTML page."
	}
	stripped := stripFooter(body)
	if strings.TrimSpace(stripped) == "" {
		return false, "GET / only has footer links. The job is not on the page."
	}
	nouns := requestNouns(request)
	if len(nouns) == 0 {
		return true, ""
	}
	low := strings.ToLower(stripped)
	var hit int
	for _, n := range nouns {
		if strings.Contains(low, n) {
			hit++
		}
	}
	need := 1
	if len(nouns) >= 2 {
		need = 2
	}
	if hit >= need {
		return true, ""
	}
	return false, "GET / does not show the job (" + strings.Join(nouns, ", ") + ")."
}

func looksLikeHTML(s string) bool {
	low := strings.ToLower(s)
	return strings.Contains(low, "<html") || strings.Contains(low, "<body") || strings.Contains(low, "<main") || strings.Contains(low, "<form") || strings.Contains(low, "<h1")
}

func visibleText(html string) string {
	noScript := regexp.MustCompile(`(?is)<script[^>]*>.*?</script>`).ReplaceAllString(html, " ")
	noStyle := regexp.MustCompile(`(?is)<style[^>]*>.*?</style>`).ReplaceAllString(noScript, " ")
	text := regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(noStyle, " ")
	return strings.Join(strings.Fields(text), " ")
}

func stripFooter(text string) string {
	low := strings.ToLower(text)
	for _, cut := range []string{"back to desk", "improve this app"} {
		if i := strings.Index(low, cut); i >= 0 {
			text = text[:i]
			low = strings.ToLower(text)
		}
	}
	return text
}

func requestNouns(request string) []string {
	stop := map[string]bool{
		"that": true, "this": true, "with": true, "from": true, "your": true, "tool": true,
		"page": true, "want": true, "need": true, "make": true, "build": true, "should": true,
		"have": true, "into": true, "and": true, "the": true, "for": true, "app": true,
		"apps": true, "when": true, "then": true, "just": true, "like": true, "user": true,
		"rebuild": true, "recipe": true, "prompts": true, "below": true, "spec": true,
		"implement": true, "project": true, "remix": true, "quality": true, "scope": true,
		"desk": true, "contract": true,
	}
	var out []string
	seen := map[string]bool{}
	for _, raw := range strings.Fields(strings.ToLower(request)) {
		w := strings.Trim(raw, ".,;:!?\"'`()[]")
		if len(w) < 4 || stop[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
		if len(out) == 6 {
			break
		}
	}
	return out
}

// ProbeWorkdir compiles the project, GETs /, and fails closed if the page
// does not show the job. snippet is a short slice of the HTML for the wait screen.
func ProbeWorkdir(ctx context.Context, workdir, request string) (ok bool, reason, snippet string) {
	if !build.Buildable(workdir) {
		return false, "The model stopped before the tool had a Go source file.", ""
	}
	bin, err := (build.GoBuilder{Timeout: 2 * time.Minute}).Build(workdir, "outcome-probe")
	if err != nil {
		return false, "The tool did not compile: " + oneLine(err.Error()), ""
	}
	return ProbeBinary(ctx, bin, workdir, request)
}

// ProbeBinary starts a built tool and checks GET /.
func ProbeBinary(ctx context.Context, bin, workdir, request string) (ok bool, reason, snippet string) {
	port, release, err := leasePort(nil)
	if err != nil {
		return false, "Couldn't find a free port to check the page.", ""
	}
	defer release()
	addr := "127.0.0.1:" + strconv.Itoa(port)
	cmd := exec.CommandContext(ctx, bin)
	cmd.Dir = workdir
	dataDir := filepath.Join(workdir, "data")
	_ = os.MkdirAll(dataDir, 0o755)
	cmd.Env = append(os.Environ(), "ADDR="+addr, "PORT="+strconv.Itoa(port), "OOZIE_DATA_DIR="+dataDir)
	if err := cmd.Start(); err != nil {
		return false, "The tool built, but it did not start.", ""
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	jar, _ := cookiejar.New(nil)
	client := &http.Client{Timeout: 3 * time.Second, Jar: jar, CheckRedirect: localRedirect}
	var body string
	var last error
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		res, err := client.Get("http://" + addr + "/")
		if err != nil {
			last = err
			time.Sleep(40 * time.Millisecond)
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 32_000))
		res.Body.Close()
		body = string(raw)
		if res.StatusCode != http.StatusOK {
			return false, "GET / returned " + res.Status + ", not a finished page.", clip(body, 180)
		}
		break
	}
	if body == "" {
		msg := "The tool built, but it did not start."
		if last != nil {
			msg = "The tool built, but GET / never answered."
		}
		return false, msg, ""
	}
	ok, reason = JudgePage(request, body)
	if !ok {
		return false, reason, clip(visibleText(body), 180)
	}
	if hasTextInput(body) {
		if err := postProbeRow(client, addr, body); err != nil {
			return false, "The form did not accept a row.", clip(visibleText(body), 180)
		}
		res, err := client.Get("http://" + addr + "/")
		if err != nil {
			return false, "The form did not keep the row.", clip(visibleText(body), 180)
		}
		raw, _ := io.ReadAll(io.LimitReader(res.Body, 32_000))
		res.Body.Close()
		body = string(raw)
		if res.StatusCode != http.StatusOK || !strings.Contains(body, oozieProbeRow) {
			return false, "GET / did not show the saved row (" + oozieProbeRow + ").", clip(visibleText(body), 180)
		}
	}
	return true, "", clip(visibleText(body), 180)
}

func localRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 4 {
		return http.ErrUseLastResponse
	}
	host := req.URL.Hostname()
	if host != "127.0.0.1" && host != "localhost" && host != "" {
		return http.ErrUseLastResponse
	}
	return nil
}

func hasTextInput(html string) bool {
	return len(textInputFields(html)) > 0
}

func textInputFields(html string) []string {
	tagRe := regexp.MustCompile(`(?is)<(input|textarea)\b([^>]*)>`)
	var names []string
	seen := map[string]bool{}
	for _, m := range tagRe.FindAllStringSubmatch(html, -1) {
		kind := strings.ToLower(m[1])
		if kind == "input" && !inputIsText(m[2]) {
			continue
		}
		name := attrValue(m[2], "name")
		if name == "" {
			name = oozieProbeRow
		}
		if seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	return names
}

func inputIsText(attrs string) bool {
	switch strings.ToLower(attrValue(attrs, "type")) {
	case "", "text", "search", "email", "url", "tel":
		return true
	default:
		return false
	}
}

func attrValue(attrs, name string) string {
	re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(name) + `\s*=\s*("([^"]*)"|'([^']*)'|([^\s>]+))`)
	m := re.FindStringSubmatch(attrs)
	if m == nil {
		return ""
	}
	for _, g := range m[2:] {
		if g != "" {
			return g
		}
	}
	return ""
}

// postProbeRow POSTs oozie-probe-row to the local form only. External actions are ignored.
func postProbeRow(client *http.Client, addr, html string) error {
	fields := url.Values{}
	for _, name := range textInputFields(html) {
		fields.Set(name, oozieProbeRow)
	}
	action := "http://" + addr + "/"
	if form := regexp.MustCompile(`(?is)<form\b([^>]*)>`).FindStringSubmatch(html); form != nil {
		if act := attrValue(form[1], "action"); act != "" && !remoteAction(act) {
			action = resolveLocalAction(addr, act)
		}
	}
	res, err := client.PostForm(action, fields)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(res.Body, 32_000))
	res.Body.Close()
	if res.StatusCode >= 400 {
		return fmtProbe(res.Status)
	}
	return nil
}

func fmtProbe(status string) error {
	return &probeError{status}
}

type probeError struct{ status string }

func (e *probeError) Error() string { return e.status }

func remoteAction(act string) bool {
	u, err := url.Parse(act)
	if err != nil {
		return true
	}
	if u.Host == "" {
		return false
	}
	host := u.Hostname()
	return host != "127.0.0.1" && host != "localhost"
}

func resolveLocalAction(addr, act string) string {
	base, err := url.Parse("http://" + addr + "/")
	if err != nil {
		return "http://" + addr + "/"
	}
	ref, err := url.Parse(act)
	if err != nil || remoteAction(act) {
		return base.String()
	}
	return base.ResolveReference(ref).String()
}

func oneLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i > 0 {
		s = s[:i]
	}
	return clip(s, 160)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
