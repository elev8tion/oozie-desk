package pi

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ModelOption is one selectable model, mirroring an entry from pi's
// enabledModels list ("provider/id").
type ModelOption struct {
	Provider string
	ID       string
	Full     string
}

// Catalog holds the models the user has enabled in their terminal pi
// instance (~/.pi/agent/settings.json), so the web UI offers the same set.
type Catalog struct {
	Models        []ModelOption
	DefaultModel  string
	ThinkingLevel string
}

type piSettings struct {
	DefaultProvider      string   `json:"defaultProvider"`
	DefaultModel         string   `json:"defaultModel"`
	DefaultThinkingLevel string   `json:"defaultThinkingLevel"`
	EnabledModels        []string `json:"enabledModels"`
}

// LoadCatalog reads the user's pi settings, then replaces stale provider
// lists with ~/.pi/agent/models-store.json when that store has a catalog.
// enabledModels is a snapshot; the store is the list pi last downloaded.
func LoadCatalog() Catalog {
	var c Catalog
	home, err := os.UserHomeDir()
	if err != nil {
		return c
	}
	agentDir := filepath.Join(home, ".pi", "agent")
	body, err := os.ReadFile(filepath.Join(agentDir, "settings.json"))
	if err != nil {
		return c
	}
	var s piSettings
	if err := json.Unmarshal(body, &s); err != nil {
		return c
	}
	c.ThinkingLevel = s.DefaultThinkingLevel
	store := readModelStore(filepath.Join(agentDir, "models-store.json"))
	refreshed := map[string]bool{}
	for _, provider := range []string{"xai", "openai-codex", "openrouter"} {
		ids := store[provider]
		if len(ids) == 0 {
			continue
		}
		refreshed[provider] = true
		for _, id := range ids {
			c.Models = append(c.Models, ModelOption{Provider: provider, ID: id, Full: provider + "/" + id})
		}
	}
	for _, full := range s.EnabledModels {
		opt, ok := splitModel(full)
		if !ok || refreshed[opt.Provider] {
			continue
		}
		c.Models = append(c.Models, opt)
	}
	if s.DefaultProvider != "" && s.DefaultModel != "" {
		c.DefaultModel = s.DefaultProvider + "/" + s.DefaultModel
	} else if len(c.Models) > 0 {
		c.DefaultModel = c.Models[0].Full
	}
	// Ensure the default is always offered even if not in enabledModels.
	if c.DefaultModel != "" && !c.contains(c.DefaultModel) {
		if opt, ok := splitModel(c.DefaultModel); ok {
			c.Models = append([]ModelOption{opt}, c.Models...)
		}
	}
	return c
}

// readModelStore returns chat model ids per provider from pi's downloaded catalog.
func readModelStore(path string) map[string][]string {
	out := map[string][]string{}
	body, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var doc map[string]struct {
		Models []struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"models"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return out
	}
	for provider, entry := range doc {
		for _, m := range entry.Models {
			id := strings.TrimSpace(m.ID)
			if id == "" {
				continue
			}
			if m.Type != "" && m.Type != "chat" {
				continue
			}
			out[provider] = append(out[provider], id)
		}
	}
	return out
}

func (c Catalog) contains(full string) bool {
	for _, m := range c.Models {
		if m.Full == full {
			return true
		}
	}
	return false
}

func splitModel(full string) (ModelOption, bool) {
	parts := strings.SplitN(full, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return ModelOption{}, false
	}
	return ModelOption{Provider: parts[0], ID: parts[1], Full: full}, true
}

const openRouterModelsURL = "https://openrouter.ai/api/v1/models"

// OpenRouterKey is OPENROUTER_API_KEY, else the openrouter secret in pi auth.
func OpenRouterKey() string {
	if k := strings.TrimSpace(os.Getenv("OPENROUTER_API_KEY")); k != "" {
		return k
	}
	path := DefaultAuthPath()
	if p := strings.TrimSpace(os.Getenv("OOZIE_AUTH_PATH")); p != "" {
		path = p
	}
	body, err := os.ReadFile(path)
	if err != nil || len(body) == 0 {
		return ""
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return ""
	}
	return authSecret(raw["openrouter"])
}

func authSecret(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	for _, key := range []string{"key", "apiKey", "access", "token"} {
		if v, ok := m[key].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// RefreshChatModels reloads chat models from ~/.pi/agent/models-store.json.
// When an OpenRouter key is set, it also tries the live models API and keeps
// type=chat. A network failure keeps the store list. xAI chat models from the
// store stay in the result.
func RefreshChatModels(openRouterKey string) []ModelOption {
	path := modelStorePath()
	if path == "" {
		return nil
	}
	store := readModelStore(path)
	orIDs := append([]string{}, store["openrouter"]...)
	if strings.TrimSpace(openRouterKey) != "" {
		if live, err := fetchOpenRouterChatModels(openRouterKey); err == nil && len(live) > 0 {
			orIDs = live
		}
	}
	if len(store) == 0 && len(orIDs) == 0 {
		return nil
	}
	var out []ModelOption
	add := func(provider, id string) {
		id = strings.TrimSpace(id)
		if provider == "" || id == "" {
			return
		}
		out = append(out, ModelOption{Provider: provider, ID: id, Full: provider + "/" + id})
	}
	for _, id := range store["xai"] {
		add("xai", id)
	}
	for _, id := range store["openai-codex"] {
		add("openai-codex", id)
	}
	for _, id := range orIDs {
		add("openrouter", id)
	}
	for provider, ids := range store {
		if provider == "xai" || provider == "openai-codex" || provider == "openrouter" {
			continue
		}
		for _, id := range ids {
			add(provider, id)
		}
	}
	return out
}

func modelStorePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".pi", "agent", "models-store.json")
}

// MergeRefreshedModels replaces providers present in fresh and keeps the rest.
// xAI models that arrived from the store therefore stay in the picker.
func MergeRefreshedModels(existing, fresh []ModelOption) []ModelOption {
	if len(fresh) == 0 {
		return existing
	}
	replaced := map[string]bool{}
	for _, m := range fresh {
		replaced[m.Provider] = true
	}
	out := append([]ModelOption{}, fresh...)
	seen := map[string]bool{}
	for _, m := range out {
		seen[m.Full] = true
	}
	for _, m := range existing {
		if replaced[m.Provider] || seen[m.Full] {
			continue
		}
		seen[m.Full] = true
		out = append(out, m)
	}
	return out
}

func fetchOpenRouterChatModels(key string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, openRouterModelsURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Timeout: 6 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode >= 300 {
		return nil, fmt.Errorf("openrouter models HTTP %d", res.StatusCode)
	}
	var doc struct {
		Data []struct {
			ID           string `json:"id"`
			Type         string `json:"type"`
			Architecture struct {
				Modality string `json:"modality"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("openrouter models: bad json")
	}
	var ids []string
	seen := map[string]bool{}
	for _, m := range doc.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] || !keepChatModel(m.Type, m.Architecture.Modality) {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("openrouter returned no chat models")
	}
	return ids, nil
}

func keepChatModel(typeName, modality string) bool {
	switch strings.ToLower(strings.TrimSpace(typeName)) {
	case "", "chat":
	default:
		return false
	}
	m := strings.ToLower(modality)
	if strings.Contains(m, "image") && !strings.Contains(m, "text") {
		return false
	}
	return true
}
