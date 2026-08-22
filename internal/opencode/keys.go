package opencode

import (
	"os"
	"strings"
)

// KeyConfig holds an API key and its display label.
type KeyConfig struct {
	EnvVar string
	Label  string
	Key    string
}

// LoadKeysFromEnv reads all configured OpenCode Go API keys from environment.
func LoadKeysFromEnv() []KeyConfig {
	candidates := []struct {
		env   string
		label string
	}{
		{"OPENCODE_GO_API_KEY", "Key 1"},
		{"OPENCODE_GO_API_KEY_R", "Key 2"},
		{"OPENCODE_GO_API_KEY_A", "Key 3"},
		{"OPENCODE_GO_API_KEY_N", "Key 4"},
	}

	var keys []KeyConfig
	for _, c := range candidates {
		val := strings.TrimSpace(os.Getenv(c.env))
		if val != "" {
			keys = append(keys, KeyConfig{
				EnvVar: c.env,
				Label:  c.label,
				Key:    val,
			})
		}
	}
	return keys
}
