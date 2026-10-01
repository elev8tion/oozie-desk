// Package build turns a project working directory into a runnable web app
// binary. The supported project shape is a Go module whose main package
// listens on the ADDR environment variable (127.0.0.1:port).
package build

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// AppBuilder produces a web-app binary for a project and returns its path.
type AppBuilder interface {
	Build(workdir, appName string) (string, error)
}

// GoBuilder compiles a Go module with `go build` and writes the binary to
// dist/<slug>. The process is not started here; the store starts it.
type GoBuilder struct {
	Timeout time.Duration // zero means 10 minutes
}

// Buildable reports whether workdir has a Go module and at least one .go file
// Publish can compile (go.mod alone is not enough — free models often stop there).
func Buildable(workdir string) bool {
	if _, err := os.Stat(filepath.Join(workdir, "go.mod")); err != nil {
		return false
	}
	return hasGoSource(workdir)
}

func hasGoSource(workdir string) bool {
	if matches, _ := filepath.Glob(filepath.Join(workdir, "*.go")); len(matches) > 0 {
		return true
	}
	if matches, _ := filepath.Glob(filepath.Join(workdir, "cmd", "*", "*.go")); len(matches) > 0 {
		return true
	}
	return false
}

func (b GoBuilder) Build(workdir, appName string) (string, error) {
	if !Buildable(workdir) {
		return "", fmt.Errorf("no go.mod found in %s — ask the agent to scaffold a Go web app (go mod init, main.go serving HTTP on $ADDR) first", workdir)
	}
	timeout := b.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	outPath := filepath.Join(workdir, "dist", Slug(appName))
	if err := os.MkdirAll(filepath.Dir(outPath), 0o755); err != nil {
		return "", err
	}

	rootOut, rootErr := goBuild(ctx, workdir, outPath, ".")
	if rootErr == nil {
		return outPath, nil
	}
	if strings.Contains(rootOut, "no Go files") || strings.Contains(rootOut, "not a main package") {
		cmdOut, cmdErr := goBuild(ctx, workdir, outPath, "./cmd/app")
		if cmdErr == nil {
			return outPath, nil
		}
		if cmdOut == "" {
			cmdOut = rootOut
		}
		return "", fmt.Errorf("go build failed: %s", cmdOut)
	}
	return "", fmt.Errorf("go build failed: %s", rootOut)
}

func goBuild(ctx context.Context, workdir, outPath, target string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "build", "-o", outPath, target)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	return tail(string(out), 2000), err
}

func sanitizeAppName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == ' ', r == '-', r == '_':
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	if out == "" {
		return "App"
	}
	return out
}

// Slug is the app's stable identity, derived from its name.
func Slug(name string) string {
	return strings.ToLower(strings.ReplaceAll(sanitizeAppName(name), " ", "-"))
}

func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
