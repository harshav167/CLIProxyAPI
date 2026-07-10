package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseConfigBytes_CodexResponseChainingDefault(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want bool
	}{
		{name: "absent defaults on", yaml: "{}\n", want: true},
		{name: "explicit false stays off", yaml: "codex-response-chaining:\n  enabled: false\n", want: false},
		{name: "explicit true stays on", yaml: "codex-response-chaining:\n  enabled: true\n", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			if got := cfg.CodexResponseChaining.Enabled; got != tt.want {
				t.Fatalf("CodexResponseChaining.Enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigOptional_CodexResponseChainingDefault(t *testing.T) {
	tests := []struct {
		name    string
		content *string
		want    bool
	}{
		{name: "missing optional file defaults on", want: true},
		{name: "empty optional file defaults on", content: stringPtr(""), want: true},
		{name: "absent key defaults on", content: stringPtr("{}\n"), want: true},
		{name: "explicit false stays off", content: stringPtr("codex-response-chaining:\n  enabled: false\n"), want: false},
		{name: "explicit true stays on", content: stringPtr("codex-response-chaining:\n  enabled: true\n"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if tt.content != nil {
				if err := os.WriteFile(configPath, []byte(*tt.content), 0o600); err != nil {
					t.Fatalf("WriteFile() error = %v", err)
				}
			}

			cfg, err := LoadConfigOptional(configPath, true)
			if err != nil {
				t.Fatalf("LoadConfigOptional() error = %v", err)
			}
			if got := cfg.CodexResponseChaining.Enabled; got != tt.want {
				t.Fatalf("CodexResponseChaining.Enabled = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSaveConfigPreserveComments_CodexResponseChainingDefault(t *testing.T) {
	tests := []struct {
		name            string
		yaml            string
		setEnabled      *bool
		wantSection     bool
		wantEnabled     bool
		wantWrittenBool string
	}{
		{name: "absent default stays absent", yaml: "# keep this comment\ndebug: true\n", wantEnabled: true},
		{name: "new explicit false persists", yaml: "# keep this comment\ndebug: true\n", setEnabled: new(bool), wantSection: true, wantEnabled: false, wantWrittenBool: "false"},
		{name: "existing true stays written", yaml: "# keep this comment\ncodex-response-chaining:\n  enabled: true\n", wantSection: true, wantEnabled: true, wantWrittenBool: "true"},
		{name: "existing false stays written", yaml: "# keep this comment\ncodex-response-chaining:\n  enabled: false\n", wantSection: true, wantEnabled: false, wantWrittenBool: "false"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(configPath, []byte(tt.yaml), 0o600); err != nil {
				t.Fatalf("os.WriteFile() error = %v", err)
			}

			cfg, err := ParseConfigBytes([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			if tt.setEnabled != nil {
				cfg.CodexResponseChaining.Enabled = *tt.setEnabled
			}

			if err := SaveConfigPreserveComments(configPath, cfg); err != nil {
				t.Fatalf("SaveConfigPreserveComments() error = %v", err)
			}

			saved, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatalf("os.ReadFile() error = %v", err)
			}
			savedText := string(saved)
			if !strings.Contains(savedText, "# keep this comment") {
				t.Fatalf("saved config lost existing comment:\n%s", savedText)
			}
			if got := strings.Contains(savedText, "codex-response-chaining:"); got != tt.wantSection {
				t.Fatalf("saved config section presence = %v, want %v:\n%s", got, tt.wantSection, savedText)
			}
			if tt.wantSection && !strings.Contains(savedText, "enabled: "+tt.wantWrittenBool) {
				t.Fatalf("saved config does not preserve enabled: %s:\n%s", tt.wantWrittenBool, savedText)
			}

			reloaded, err := LoadConfigOptional(configPath, true)
			if err != nil {
				t.Fatalf("LoadConfigOptional() error = %v", err)
			}
			if got := reloaded.CodexResponseChaining.Enabled; got != tt.wantEnabled {
				t.Fatalf("reloaded CodexResponseChaining.Enabled = %v, want %v", got, tt.wantEnabled)
			}
		})
	}
}

func stringPtr(value string) *string {
	return &value
}
