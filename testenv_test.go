package main

import "testing"

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
