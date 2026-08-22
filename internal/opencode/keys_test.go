package opencode

import (
	"os"
	"testing"
)

func TestLoadKeysFromEnv_DynamicDiscovery(t *testing.T) {
	// Clear any existing keys
	for _, env := range os.Environ() {
		if len(env) > 19 && env[:19] == "OPENCODE_GO_API_KEY" {
			parts := splitEnv(env)
			os.Unsetenv(parts[0])
		}
	}

	// Set test keys
	os.Setenv("OPENCODE_GO_API_KEY", "sk-main")
	os.Setenv("OPENCODE_GO_API_KEY_R", "sk-rene")
	os.Setenv("OPENCODE_GO_API_KEY_ALICE", "sk-alice")
	defer os.Unsetenv("OPENCODE_GO_API_KEY")
	defer os.Unsetenv("OPENCODE_GO_API_KEY_R")
	defer os.Unsetenv("OPENCODE_GO_API_KEY_ALICE")

	keys := LoadKeysFromEnv()

	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}

	// Should be sorted by env var name
	expected := []struct {
		envVar string
		label  string
		key    string
	}{
		{"OPENCODE_GO_API_KEY", "Main", "sk-main"},
		{"OPENCODE_GO_API_KEY_ALICE", "ALICE", "sk-alice"},
		{"OPENCODE_GO_API_KEY_R", "R", "sk-rene"},
	}

	for i, exp := range expected {
		if keys[i].EnvVar != exp.envVar {
			t.Errorf("keys[%d].EnvVar = %q, want %q", i, keys[i].EnvVar, exp.envVar)
		}
		if keys[i].Label != exp.label {
			t.Errorf("keys[%d].Label = %q, want %q", i, keys[i].Label, exp.label)
		}
		if keys[i].Key != exp.key {
			t.Errorf("keys[%d].Key = %q, want %q", i, keys[i].Key, exp.key)
		}
	}
}

func TestLoadKeysFromEnv_EmptyValuesIgnored(t *testing.T) {
	os.Setenv("OPENCODE_GO_API_KEY", "sk-valid")
	os.Setenv("OPENCODE_GO_API_KEY_EMPTY", "")
	os.Setenv("OPENCODE_GO_API_KEY_SPACES", "   ")
	defer os.Unsetenv("OPENCODE_GO_API_KEY")
	defer os.Unsetenv("OPENCODE_GO_API_KEY_EMPTY")
	defer os.Unsetenv("OPENCODE_GO_API_KEY_SPACES")

	keys := LoadKeysFromEnv()

	if len(keys) != 1 {
		t.Fatalf("expected 1 key (empty/spaces ignored), got %d", len(keys))
	}
	if keys[0].Key != "sk-valid" {
		t.Errorf("expected 'sk-valid', got %q", keys[0].Key)
	}
}

func TestLoadKeysFromEnv_NoKeys(t *testing.T) {
	// Unset all potential keys
	for _, env := range os.Environ() {
		if len(env) > 19 && env[:19] == "OPENCODE_GO_API_KEY" {
			parts := splitEnv(env)
			os.Unsetenv(parts[0])
		}
	}

	keys := LoadKeysFromEnv()
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys, got %d", len(keys))
	}
}

func TestLoadKeysFromEnv_MainLabelForBaseKey(t *testing.T) {
	os.Setenv("OPENCODE_GO_API_KEY", "sk-base")
	defer os.Unsetenv("OPENCODE_GO_API_KEY")

	keys := LoadKeysFromEnv()
	if len(keys) != 1 {
		t.Fatalf("expected 1 key, got %d", len(keys))
	}
	if keys[0].Label != "Main" {
		t.Errorf("base key should have label 'Main', got %q", keys[0].Label)
	}
}

func splitEnv(entry string) [2]string {
	for i := 0; i < len(entry); i++ {
		if entry[i] == '=' {
			return [2]string{entry[:i], entry[i+1:]}
		}
	}
	return [2]string{entry, ""}
}
