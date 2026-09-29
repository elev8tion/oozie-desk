package pi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

// LoadCatalog reads the user's pi settings. A missing or unreadable file
// yields an empty catalog; pi itself then falls back to its own defaults.
func LoadCatalog() Catalog {
	var c Catalog
	home, err := os.UserHomeDir()
	if err != nil {
		return c
	}
	body, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "settings.json"))
	if err != nil {
		return c
	}
	var s piSettings
	if err := json.Unmarshal(body, &s); err != nil {
		return c
	}
	c.ThinkingLevel = s.DefaultThinkingLevel
	for _, full := range s.EnabledModels {
		if opt, ok := splitModel(full); ok {
			c.Models = append(c.Models, opt)
		}
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
