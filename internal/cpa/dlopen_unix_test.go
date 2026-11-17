//go:build darwin || linux

package cpa_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Mag1cFall/AIStudio2API/internal/cpa"
)

func resolveBuiltPluginLibPath() (string, error) {
	if p := strings.TrimSpace(os.Getenv("CPA_PLUGIN_LIB_PATH")); p != "" {
		return filepath.Abs(p)
	}
	ext := ".so"
	if runtime.GOOS == "darwin" {
		ext = ".dylib"
	}
	return filepath.Abs(filepath.Join("..", "..", "dist", "plugins", runtime.GOOS, runtime.GOARCH, "aistudio2api"+ext))
}

func TestDynamicLibraryDLOpenABI(t *testing.T) {
	dylibPath, err := resolveBuiltPluginLibPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dylibPath); err != nil {
		t.Skipf("shared library not built at %s: %v", dylibPath, err)
	}
	tmpDir := t.TempDir()
	cfgYAML := []byte("data_dir: " + tmpDir + "\nauto_start: false\nsync_auth_files: false\n")
	payloadBytes, _ := json.Marshal(map[string]any{
		"schema_version": 10,
		"config_yaml":    cfgYAML,
	})
	got, err := cpa.CallSharedLibraryABI(dylibPath, "plugin.register", string(payloadBytes))
	_, _ = cpa.CallSharedLibraryABI(dylibPath, "plugin.shutdown", `{}`)
	if err != nil {
		t.Fatalf("CallSharedLibraryABI failed: %v", err)
	}
	if !strings.Contains(got, `"ok":true`) || !strings.Contains(got, `"Name":"aistudio2api"`) {
		t.Fatalf("unexpected dlopen response from plugin.register: %s", got)
	}
	t.Logf("dlopen C ABI plugin.register response: %s", got)
}
