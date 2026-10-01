package native

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxToolOut = 6000

func runTool(ctx context.Context, workdir, name, argsJSON string) (summary, body string, err error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return name + " (error)", "invalid tool arguments: " + err.Error(), err
	}
	switch name {
	case "bash":
		cmd, _ := args["command"].(string)
		return toolBash(ctx, workdir, cmd)
	case "read":
		path, _ := args["path"].(string)
		return toolRead(workdir, path)
	case "write":
		path, _ := args["path"].(string)
		content, _ := args["content"].(string)
		return toolWrite(workdir, path, content)
	case "edit":
		path, _ := args["path"].(string)
		oldT, _ := args["old_text"].(string)
		if oldT == "" {
			oldT, _ = args["oldText"].(string)
		}
		newT, _ := args["new_text"].(string)
		if newT == "" {
			newT, _ = args["newText"].(string)
		}
		return toolEdit(workdir, path, oldT, newT)
	case "ls":
		path, _ := args["path"].(string)
		return toolLS(workdir, path)
	default:
		return name + " (error)", "unknown tool: " + name, fmt.Errorf("unknown tool")
	}
}

func resolveInWorkdir(workdir, path string) (string, error) {
	workdir, err := filepath.Abs(workdir)
	if err != nil {
		return "", err
	}
	path = strings.TrimSpace(path)
	if path == "" || path == "." {
		return workdir, nil
	}
	var full string
	if filepath.IsAbs(path) {
		full = filepath.Clean(path)
	} else {
		full = filepath.Clean(filepath.Join(workdir, path))
	}
	rel, err := filepath.Rel(workdir, full)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("path escapes project directory")
	}
	return full, nil
}

func bashEscapes(workdir, command string) error {
	workdir, err := filepath.Abs(workdir)
	if err != nil {
		return err
	}
	if strings.Contains(command, "~") || strings.Contains(command, "../") || strings.Contains(command, "..\\") {
		return fmt.Errorf("command leaves the project directory")
	}
	for _, field := range strings.Fields(command) {
		if !filepath.IsAbs(field) {
			continue
		}
		rel, err := filepath.Rel(workdir, filepath.Clean(field))
		if err != nil || strings.HasPrefix(rel, "..") {
			return fmt.Errorf("command leaves the project directory")
		}
	}
	return nil
}

func toolBash(ctx context.Context, workdir, command string) (string, string, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return "bash (error)", "empty command", fmt.Errorf("empty command")
	}
	if err := bashEscapes(workdir, command); err != nil {
		return "bash (error)", err.Error(), err
	}
	cctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, "/bin/zsh", "-lc", command)
	cmd.Dir = workdir
	cmd.Env = append(os.Environ(), "HOME="+workdir)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	body := out.String()
	if body == "" && err != nil {
		body = err.Error()
	}
	body = clamp(body, maxToolOut)
	sum := fmt.Sprintf("bash: %s", short(command, 100))
	if err != nil {
		return sum + " (error)", body, err
	}
	return sum + " (ok)", body, nil
}

func toolRead(workdir, path string) (string, string, error) {
	full, err := resolveInWorkdir(workdir, path)
	if err != nil {
		return "read (error)", err.Error(), err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return fmt.Sprintf("read: %s (error)", path), err.Error(), err
	}
	if len(b) > 200_000 {
		return fmt.Sprintf("read: %s (error)", path), "file too large", fmt.Errorf("file too large")
	}
	body := clamp(string(b), maxToolOut)
	return fmt.Sprintf("read: %s (ok)", path), body, nil
}

func toolWrite(workdir, path, content string) (string, string, error) {
	full, err := resolveInWorkdir(workdir, path)
	if err != nil {
		return "write (error)", err.Error(), err
	}
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return fmt.Sprintf("write: %s (error)", path), err.Error(), err
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		return fmt.Sprintf("write: %s (error)", path), err.Error(), err
	}
	return fmt.Sprintf("write: %s (ok)", path), clamp(content, maxToolOut), nil
}

func toolEdit(workdir, path, oldT, newT string) (string, string, error) {
	full, err := resolveInWorkdir(workdir, path)
	if err != nil {
		return "edit (error)", err.Error(), err
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return fmt.Sprintf("edit: %s (error)", path), err.Error(), err
	}
	src := string(b)
	if oldT == "" {
		return fmt.Sprintf("edit: %s (error)", path), "old_text is required", fmt.Errorf("old_text required")
	}
	count := strings.Count(src, oldT)
	if count == 0 {
		return fmt.Sprintf("edit: %s (error)", path), "old_text not found", fmt.Errorf("old_text not found")
	}
	if count > 1 {
		return fmt.Sprintf("edit: %s (error)", path), fmt.Sprintf("old_text matched %d times; must be unique", count), fmt.Errorf("ambiguous edit")
	}
	out := strings.Replace(src, oldT, newT, 1)
	if err := os.WriteFile(full, []byte(out), 0o644); err != nil {
		return fmt.Sprintf("edit: %s (error)", path), err.Error(), err
	}
	return fmt.Sprintf("edit: %s (ok)", path), clamp(newT, maxToolOut), nil
}

func toolLS(workdir, path string) (string, string, error) {
	full, err := resolveInWorkdir(workdir, path)
	if err != nil {
		return "ls (error)", err.Error(), err
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		return fmt.Sprintf("ls: %s (error)", path), err.Error(), err
	}
	var b strings.Builder
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() {
			name += "/"
		}
		b.WriteString(name)
		b.WriteByte('\n')
	}
	label := path
	if label == "" {
		label = "."
	}
	return fmt.Sprintf("ls: %s (ok)", label), strings.TrimSpace(b.String()), nil
}

func clamp(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "\n…(truncated)"
}

func short(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func mutatingTool(name string) bool {
	switch name {
	case "bash", "write", "edit":
		return true
	default:
		return false
	}
}
