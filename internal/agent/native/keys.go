package native

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"oozie-desk/internal/agent/pi"
	"oozie-desk/internal/supportpath"
)

// Keys holds API credentials the desk agent uses. Values never log.
// Codex is the ChatGPT OAuth login from ~/.pi/agent/auth.json (openai-codex),
// not an OpenAI platform API key.
type Keys struct {
	OpenRouter       string
	OpenRouterCustom string
	OpenRouterBase   string
	XAI              string
	ZAI              string
	OpenAI           string
	CodexAccess      string
	CodexAccount     string
	CodexRefresh     string
	CodexExpires     int64
}

// SaveProviderKey writes one provider key into the desk auth file.
// The key is never logged. Env vars still override the file at read time.
func SaveProviderKey(provider, key string) error {
	provider = strings.TrimSpace(strings.ToLower(provider))
	key = strings.TrimSpace(key)
	switch provider {
	case "openrouter", "xai", "zai", "openai":
	default:
		return fmt.Errorf("unknown provider")
	}
	if key == "" {
		return fmt.Errorf("key is required")
	}
	path := pi.DefaultAuthPath()
	if p := os.Getenv("OOZIE_AUTH_PATH"); p != "" {
		path = p
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw := map[string]any{}
	if body, err := os.ReadFile(path); err == nil && len(body) > 0 {
		_ = json.Unmarshal(body, &raw)
	}
	raw[provider] = key
	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, body, 0o600)
}

// LoadKeys reads env overrides first, then ~/.pi/agent/auth.json so existing
// pi logins work without the pi binary.
func LoadKeys() Keys {
	k := Keys{
		OpenRouter:     strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")),
		OpenRouterBase: openRouterBase(),
		XAI:            firstEnv("XAI_API_KEY", "GROK_API_KEY"),
		ZAI:            firstEnv("ZAI_API_KEY", "Z_AI_API_KEY"),
		OpenAI:         strings.TrimSpace(os.Getenv("OPENAI_API_KEY")),
	}
	k.OpenRouterCustom = k.OpenRouter
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
	k.OpenRouterCustom = firstNonEmpty(credString(raw["openrouter-custom"]), k.OpenRouter)
	k.OpenRouterBase = openRouterBase()
	if k.XAI == "" {
		k.XAI = firstNonEmpty(credString(raw["xai"]), credField(raw["xai"], "access"), credField(raw["xai-auth"], "access"))
	}
	if k.ZAI == "" {
		k.ZAI = firstNonEmpty(credString(raw["zai"]), credField(raw["zai"], "key"))
	}
	if k.OpenAI == "" {
		k.OpenAI = firstNonEmpty(credString(raw["openai"]), credField(raw["openai"], "key"))
	}
	if cred := raw["openai-codex"]; len(cred) > 0 {
		if k.CodexAccess == "" {
			k.CodexAccess = firstNonEmpty(credField(cred, "access"), credString(cred))
		}
		if k.CodexAccount == "" {
			k.CodexAccount = credField(cred, "accountId")
		}
		if k.CodexRefresh == "" {
			k.CodexRefresh = credField(cred, "refresh")
		}
		if k.CodexExpires == 0 {
			k.CodexExpires = credInt(cred, "expires")
		}
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

func credInt(raw json.RawMessage, field string) int64 {
	if len(raw) == 0 {
		return 0
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return 0
	}
	switch v := m[field].(type) {
	case float64:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	}
	return 0
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
	if k.OpenRouterCustom != "" || k.OpenRouter != "" {
		out["openrouter-custom"] = true
	}
	if k.XAI != "" {
		out["xai"] = true
	}
	if k.ZAI != "" {
		out["zai"] = true
	}
	if k.OpenAI != "" {
		out["openai"] = true
	}
	if k.CodexAccess != "" {
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
		return openRouterURL(k.OpenRouterBase), k.OpenRouter, opt.ID, nil
	case "openrouter-custom":
		key := firstNonEmpty(k.OpenRouterCustom, k.OpenRouter)
		if key == "" {
			return "", "", "", errNoKey("openrouter")
		}
		return openRouterURL(k.OpenRouterBase), key, opt.ID, nil
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
	case "openai":
		if k.OpenAI == "" {
			return "", "", "", errNoKey("openai")
		}
		return "https://api.openai.com/v1", k.OpenAI, opt.ID, nil
	case "openai-codex":
		if k.CodexAccess == "" {
			return "", "", "", errNoKey("openai-codex")
		}
		return codexBaseURL, k.CodexAccess, opt.ID, nil
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
// Pi's openrouter-custom provider from models.json is always offered.
func MergeCatalog(user pi.Catalog) pi.Catalog {
	if len(user.Models) == 0 {
		user = DefaultCatalog()
	}
	if user.DefaultModel == "" {
		user.DefaultModel = user.Models[0].Full
	}
	for _, extra := range openRouterModels() {
		if !catalogHas(user, extra.Full) {
			user.Models = append(user.Models, extra)
		}
	}
	return user
}

func catalogHas(c pi.Catalog, full string) bool {
	for _, m := range c.Models {
		if m.Full == full {
			return true
		}
	}
	return false
}

func openRouterURL(base string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return "https://openrouter.ai/api/v1"
	}
	return base
}

func piModelsPath() string {
	if p := os.Getenv("OOZIE_AUTH_PATH"); p != "" {
		return filepath.Join(filepath.Dir(p), "models.json")
	}
	return filepath.Join(filepath.Dir(pi.DefaultAuthPath()), "models.json")
}

func openRouterBase() string {
	base, _ := readOpenRouterProvider()
	return openRouterURL(base)
}

func openRouterModels() []pi.ModelOption {
	_, models := readOpenRouterProvider()
	return models
}

func openRouterReasons(full string) bool {
	_, models := readOpenRouterProvider()
	for _, m := range models {
		if m.Full == full {
			return m.ID != "" && openRouterReasoning[m.Full]
		}
	}
	return openRouterReasoning[full]
}

var openRouterReasoning = map[string]bool{}

func readOpenRouterProvider() (string, []pi.ModelOption) {
	body, err := os.ReadFile(piModelsPath())
	if err != nil {
		return "", nil
	}
	var doc struct {
		Providers map[string]struct {
			BaseURL string `json:"baseUrl"`
			Models  []struct {
				ID        string `json:"id"`
				Reasoning bool   `json:"reasoning"`
			} `json:"models"`
		} `json:"providers"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return "", nil
	}
	prov, ok := doc.Providers["openrouter-custom"]
	if !ok {
		return "", nil
	}
	var models []pi.ModelOption
	for _, item := range prov.Models {
		if strings.TrimSpace(item.ID) == "" {
			continue
		}
		full := "openrouter-custom/" + item.ID
		if opt, ok := splitFull(full); ok {
			models = append(models, opt)
			openRouterReasoning[full] = item.Reasoning
		}
	}
	return prov.BaseURL, models
}

// DataDir is where the desk may store agent-side files later.
func DataDir() string {
	return supportpath.Dir()
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
