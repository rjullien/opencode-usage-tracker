package opencode

import (
	"os"
	"sort"
	"strings"
)

// KeyConfig holds an API key and its display label.
type KeyConfig struct {
	EnvVar string
	Label  string
	Key    string
}

// LoadKeysFromEnv discovers all OpenCode Go API keys from environment variables.
// Any env var matching the prefix OPENCODE_GO_API_KEY is loaded automatically.
// No code change needed when adding or removing subscriptions — just set/unset the env var.
//
// Examples:
//
//	OPENCODE_GO_API_KEY=sk-...       → label "Main"
//	OPENCODE_GO_API_KEY_R=sk-...     → label "R"
//	OPENCODE_GO_API_KEY_ALICE=sk-... → label "Alice"
func LoadKeysFromEnv() []KeyConfig {
	const prefix = "OPENCODE_GO_API_KEY"

	var keys []KeyConfig
	for _, envEntry := range os.Environ() {
		parts := strings.SplitN(envEntry, "=", 2)
		if len(parts) != 2 {
			continue
		}
		name := parts[0]
		val := strings.TrimSpace(parts[1])

		if !strings.HasPrefix(name, prefix) || val == "" {
			continue
		}

		// Derive label from suffix: OPENCODE_GO_API_KEY → "Main", OPENCODE_GO_API_KEY_R → "R"
		suffix := strings.TrimPrefix(name, prefix)
		label := "Main"
		if suffix != "" {
			label = strings.TrimPrefix(suffix, "_")
		}

		keys = append(keys, KeyConfig{
			EnvVar: name,
			Label:  label,
			Key:    val,
		})
	}

	// Sort by env var name for deterministic display order
	sort.Slice(keys, func(i, j int) bool {
		return keys[i].EnvVar < keys[j].EnvVar
	})

	return keys
}
