package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/provider"
)

// TestMain makes the package's tests hermetic for the whole binary, before any test runs.
//
// The OS keychain is turned off through FLYNN_VAULT_FILE, the vault's own switch,
// so every vault this binary opens uses the sealed file in the test's data directory. A
// developer machine with a stored key would otherwise resolve a model in a test that
// expects none, and a test that then serves or runs a goal waits on a real provider
// until the binary times out. The provider key variables are cleared for the same
// reason. [TestNoTestReachesTheKeychain] fails when either stops holding.
//
// The claude and codex backends' homes point at a temporary directory: building either
// spawner creates its home, which would otherwise land in the real user's data directory.
func TestMain(m *testing.M) {
	if mode := os.Getenv(fakeCLIEnv); mode != "" {
		os.Exit(runFakeAgentCLI(mode, os.Args[1:]))
	}
	dir, err := os.MkdirTemp("", "flynn-agent-homes-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.Setenv(vaultFileEnv, "1"); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, k := range provider.CredentialEnvVars() {
		_ = os.Unsetenv(k)
	}
	claudeHome = func() string { return filepath.Join(dir, "claude") }
	codexHome = func() string { return filepath.Join(dir, "codex") }
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
