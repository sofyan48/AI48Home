package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPluginsDisabledByDefault(t *testing.T) {
	for _, source := range []string{
		"port: 8317\n",
		"plugins: {}\n",
		"plugins:\n  configs:\n    sample:\n      priority: 1\n",
	} {
		t.Run(source, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(path, []byte(source), 0600); errWrite != nil {
				t.Fatal(errWrite)
			}
			cfg, errLoad := LoadConfigOptional(path, false)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if cfg.Plugins.Enabled {
				t.Fatal("plugins must be disabled when enabled is omitted")
			}
			for id, plugin := range cfg.Plugins.Configs {
				if plugin.Enabled != nil && *plugin.Enabled {
					t.Fatalf("plugin %s must be disabled when enabled is omitted", id)
				}
			}
		})
	}
}

func TestExamplePluginsDisabled(t *testing.T) {
	cfg, errLoad := LoadConfigOptional("../../config.example.yaml", false)
	if errLoad != nil {
		t.Fatal(errLoad)
	}
	if cfg.Plugins.Enabled {
		t.Fatal("example configuration must disable plugins globally")
	}
	for id, plugin := range cfg.Plugins.Configs {
		if plugin.Enabled == nil || *plugin.Enabled {
			t.Fatalf("example plugin %s must explicitly be disabled", id)
		}
	}
}
