package pi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// ErrModelUnsigned is the desk sentence when no enabled model has credentials.
var ErrModelUnsigned = errors.New("This model is not signed in.")

// DefaultAuthPath is pi's credential file. Values are never logged.
func DefaultAuthPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "auth.json")
}

// SignedProviders reports provider ids that have a stored credential.
// A key such as "xai-auth" also counts as "xai". Secret values are ignored.
func SignedProviders(authPath string) map[string]bool {
	out := map[string]bool{}
	if authPath == "" {
		return out
	}
	body, err := os.ReadFile(authPath)
	if err != nil {
		return out
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return out
	}
	for id, cred := range raw {
		if id == "" || string(cred) == "null" {
			continue
		}
		out[id] = true
		if base, ok := strings.CutSuffix(id, "-auth"); ok && base != "" {
			out[base] = true
		}
	}
	return out
}

// ChooseModel keeps a signed-in model. It prefers the session model, then
// the pi default, then the first enabled model that is signed in.
func ChooseModel(c Catalog, sessionModel string, signed map[string]bool) (string, error) {
	if modelSigned(sessionModel, signed) {
		return sessionModel, nil
	}
	if modelSigned(c.DefaultModel, signed) {
		return c.DefaultModel, nil
	}
	for _, m := range c.Models {
		if signed[m.Provider] {
			return m.Full, nil
		}
	}
	return "", ErrModelUnsigned
}

func modelSigned(full string, signed map[string]bool) bool {
	opt, ok := splitModel(full)
	return ok && signed[opt.Provider]
}

// CandidateModels lists signed-in models to try, session first, then the
// default, then the enabled list. Duplicates are dropped.
func CandidateModels(c Catalog, sessionModel string, signed map[string]bool) []string {
	var out []string
	add := func(full string) {
		if !modelSigned(full, signed) {
			return
		}
		for _, have := range out {
			if have == full {
				return
			}
		}
		out = append(out, full)
	}
	add(sessionModel)
	add(c.DefaultModel)
	for _, m := range c.Models {
		add(m.Full)
	}
	return out
}

// ProbeModel asks pi if this model answers. A 404 or missing key is a rejection.
// The process is killed as soon as the first answer or refusal arrives.
func ProbeModel(model string) error {
	opt, ok := splitModel(model)
	if !ok {
		return errors.New("unknown model")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, resolvePiBinary(), "--mode", "rpc", "--provider", opt.Provider, "--model", opt.ID)
	cmd.Dir = os.TempDir()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
	}()
	_, _ = stdin.Write([]byte("{\"type\":\"prompt\",\"message\":\"Reply with ok.\"}\n"))
	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		line := scanner.Text()
		if ModelRejected(line) || ModelRejected(stderr.String()) {
			return errors.New(line)
		}
		if strings.Contains(line, `"success":false`) && strings.Contains(line, "prompt") {
			return errors.New(line)
		}
		if strings.Contains(line, "agent_end") || strings.Contains(line, "message_end") || (strings.Contains(line, `"command":"prompt"`) && strings.Contains(line, `"success":true`)) {
			return nil
		}
	}
	if msg := stderr.String(); ModelRejected(msg) {
		return errors.New(msg)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New("model did not answer")
}

// ModelRejected reports a model that is signed in but did not answer.
func ModelRejected(msg string) bool {
	msg = strings.ToLower(msg)
	return strings.Contains(msg, "not found") || strings.Contains(msg, "404") || strings.Contains(msg, "no api key") || strings.Contains(msg, "no models match")
}
