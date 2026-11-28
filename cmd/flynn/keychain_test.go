package main

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

const vaultFileEnv = "FLYNN_VAULT_FILE"

// TestNoTestReachesTheKeychain: a credential stored under test goes to the sealed file in
// the data directory. With the keychain reachable the write would land in the OS keychain
// and no file would appear, so on a machine with a keychain this fails when [TestMain]
// stops turning it off. The source check catches a test that turns it back on, which a
// machine with no keychain (CI) could not see at run time.
func TestNoTestReachesTheKeychain(t *testing.T) {
	t.Setenv("FLYNN_VAULT_PASSPHRASE", "probe-passphrase")
	dir := t.TempDir()
	ctx := context.Background()
	const ref = "FLYNN_TEST_KEYCHAIN_PROBE"
	store := vault.New(dir)
	if err := store.Set(ctx, ref, secret.New("probe")); err != nil {
		t.Fatalf("store the probe: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(ctx, ref) })
	if _, err := os.Stat(filepath.Join(dir, "vault.sealed")); err != nil {
		t.Fatalf("the probe did not reach the sealed file, so the OS keychain took it: %v", err)
	}

	for _, k := range provider.CredentialEnvVars() {
		if v := os.Getenv(k); v != "" {
			t.Errorf("%s is set in the test binary's environment", k)
		}
		if _, err := credentialSource(t.TempDir()).Lookup(ctx, k); err == nil {
			t.Errorf("%s resolved from a fresh data directory", k)
		}
	}

	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	reenable := regexp.MustCompile(`"` + vaultFileEnv + `",\s*"([^"]*)"`)
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range reenable.FindAllStringSubmatch(string(src), -1) {
			if v := strings.ToLower(m[1]); v != "1" && v != "true" && v != "on" && v != "yes" {
				t.Errorf("%s sets %s to %q, which lets a test read the real keychain", f, vaultFileEnv, m[1])
			}
		}
	}
}
