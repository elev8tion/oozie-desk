package projects

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"oozie-desk/internal/build"
)

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
	for _, n := range nouns {
		if strings.Contains(low, n) {
			return true, ""
		}
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
	client := &http.Client{Timeout: 3 * time.Second}
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
	return true, "", clip(visibleText(body), 180)
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
