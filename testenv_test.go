package main

import (
	"os"
	"path/filepath"
	"testing"
)

// isolateUserConfigDir points the user config dir at a temp dir for one test.
//
// os.UserConfigDir() resolves differently per platform, and setting only
// XDG_CONFIG_HOME (the pre-existing pattern) is a no-op on darwin, where it
// reads $HOME/Library/Application Support instead. That silently made these
// tests write OAuth token files into the real user config directory. Setting
// HOME as well covers darwin and windows, and XDG_CONFIG_HOME covers Linux.
func isolateUserConfigDir(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	// Windows uses %AppData%; HOME is not consulted there.
	t.Setenv("AppData", dir)
	return dir
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
