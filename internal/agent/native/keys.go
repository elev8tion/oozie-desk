package native

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"oozie/internal/agent/pi"
)

// Keys holds API credentials the desk agent uses. Values never log.
type Keys struct {
	OpenRouter string
	XAI        string
	ZAI        string
	OpenAI     string
}

// LoadKeys reads env overrides first, then ~/.pi/agent/auth.json so existing
// pi logins work without the pi binary.
func LoadKeys() Keys {
	k := Keys{
		OpenRouter: strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
		XAI:        firstEnv("XAI_API_KEY", "GROK_API_KEY"),
		ZAI:        firstEnv("ZAI_API_KEY", "Z_AI_API_KEY"),
		OpenAI:     strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
	}
	path := pi.DefaultAuthPath()
	if p := os.Getenv("OOZIE_AUTH_PATH"); p != "" {
		path = p
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return k
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return k
	}
	if k.OpenRouter == "" {
		k.OpenRouter = credString(raw["openrouter"])
	}
	if k.XAI == "" {
		k.XAI = firstNonEmpty(credString(raw["xai"]), credField(raw["xai"], "access"), credField(raw["xai-auth"], "access"))
	}
	if k.ZAI == "" {
		k.ZAI = firstNonEmpty(credString(raw["zai"]), credField(raw["zai"], "key"))
	}
	if k.OpenAI == "" {
		k.OpenAI = firstNonEmpty(credString(raw["openai"]), credField(raw["openai"], "key"), credString(raw["openai-codex"]))
	}
	return k
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := strings.TrimSpace(os.Getenv(n)); v != "" {
			return v
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func credString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return firstNonEmpty(credField(raw, "key"), credField(raw, "apiKey"), credField(raw, "access"), credField(raw, "token"))
}

func credField(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	v, _ := m[field].(string)
	return strings.TrimSpace(v)
}

// SignedFromKeys maps loaded keys to provider ids for CandidateModels.
func SignedFromKeys(k Keys) map[string]bool {
	out := map[string]bool{}
	if k.OpenRouter != "" {
		out["openrouter"] = true
	}
	if k.XAI != "" {
		out["xai"] = true
	}
	if k.ZAI != "" {
		out["zai"] = true
	}
	if k.OpenAI != "" {
		out["openai"] = true
		out["openai-codex"] = true
	}
	return out
}

// ResolveEndpoint maps provider/model to an OpenAI-compatible chat URL + key + model id.
func (k Keys) ResolveEndpoint(full string) (baseURL, apiKey, modelID string, err error) {
	opt, ok := splitFull(full)
	if !ok {
		return "", "", "", errUnknownModel
	}
	switch opt.Provider {
	case "openrouter":
		if k.OpenRouter == "" {
			return "", "", "", errNoKey("openrouter")
		}
		return "https://openrouter.ai/api/v1", k.OpenRouter, opt.ID, nil
	case "xai":
		if k.XAI == "" {
			return "", "", "", errNoKey("xai")
		}
		return "https://api.x.ai/v1", k.XAI, opt.ID, nil
	case "zai":
		if k.ZAI == "" {
			return "", "", "", errNoKey("zai")
		}
		// Z.AI OpenAI-compatible surface.
		return "https://api.z.ai/api/paas/v4", k.ZAI, opt.ID, nil
	case "openai", "openai-codex":
		if k.OpenAI == "" {
			return "", "", "", errNoKey("openai")
		}
		return "https://api.openai.com/v1", k.OpenAI, opt.ID, nil
	default:
		// Unknown provider: try OpenRouter with full id if we have a key.
		if k.OpenRouter != "" {
			return "https://openrouter.ai/api/v1", k.OpenRouter, full, nil
		}
		return "", "", "", errNoKey(opt.Provider)
	}
}

type keyError string

func (e keyError) Error() string { return string(e) }

var errUnknownModel = keyError("unknown model")

func errNoKey(provider string) error {
	return keyError("No API key for " + provider + ". Set it in Settings or the environment, or keep a key in ~/.pi/agent/auth.json.")
}

func splitFull(full string) (pi.ModelOption, bool) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return pi.ModelOption{}, false
	}
	return pi.ModelOption{Provider: parts[0], ID: parts[1], Full: full}, true
}

// DefaultCatalog is used when pi settings are missing so the desk still has models.
func DefaultCatalog() pi.Catalog {
	models := []string{
		"openrouter/nvidia/nemotron-3-ultra-550b-a55b:free",
		"openrouter/anthropic/claude-haiku-4.5",
		"openrouter/google/gemini-2.5-flash",
		"openrouter/anthropic/claude-sonnet-4.6",
		"xai/grok-3-mini",
		"zai/glm-4.5-flash",
	}
	c := pi.Catalog{DefaultModel: models[0]}
	for _, full := range models {
		if opt, ok := splitFull(full); ok {
			c.Models = append(c.Models, opt)
		}
	}
	return c
}

// MergeCatalog prefers user settings; fills empty with defaults.
func MergeCatalog(user pi.Catalog) pi.Catalog {
	if len(user.Models) == 0 {
		return DefaultCatalog()
	}
	if user.DefaultModel == "" {
		user.DefaultModel = user.Models[0].Full
	}
	return user
}

// DataDir is where the desk may store agent-side files later.
func DataDir() string {
	if v := os.Getenv("OOZIE_DATA_ROOT"); v != "" {
		return v
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "oozie-web")
}

// keyCache avoids re-reading auth on every prompt in tests; LoadKeys is cheap enough.
var (
	keysMu    sync.Mutex
	keysCache *Keys
)

// CachedKeys returns LoadKeys with optional refresh.
func CachedKeys(refresh bool) Keys {
	keysMu.Lock()
	defer keysMu.Unlock()
	if keysCache == nil || refresh {
		k := LoadKeys()
		keysCache = &k
	}
	return *keysCache
}
