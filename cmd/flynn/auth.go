package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/term"

	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/tools/github"
)

// runAuth implements the `flynn auth` command group: managing the credentials the
// agent uses, stored in the encrypted vault rather than handed in on every run.
//
//	flynn auth set <provider>                 read a model-provider API key (hidden)
//	flynn auth set-app --issuer --installation --key-file   store a GitHub App identity
//	flynn auth rm-app                         forget the stored GitHub App identity
//	flynn auth list                           show which providers have a key stored
//	flynn auth add <integration> [--name <n>] add an integration credential (name defaults to "default")
//	flynn auth use <integration>/<name>       make a credential the integration default
//	flynn auth ls  <integration>              list an integration's credentials
//	flynn auth rm  <provider> | <integration>/<name>   remove a key or credential
//
// The model-provider verbs (set, list) manage the one key per model provider. The
// integration verbs (add, use, ls) manage the named, role-bearing credentials an
// integration holds. rm removes a model-provider key when given a bare provider, or
// an integration credential when given an <integration>/<name> reference.
func runAuth(args []string, dataDir string) error {
	if len(args) == 0 {
		return errors.New("usage: flynn auth <set|list|add|use|ls|rm>")
	}
	store := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))
	ctx := context.Background()

	switch args[0] {
	case "set":
		return authSet(ctx, store, args[1:])
	case "set-app":
		return authSetApp(ctx, store, args[1:], os.ReadFile, os.Remove)
	case "rm-app":
		return authRemoveApp(ctx, store)
	case "add":
		return authCredAdd(ctx, store, dataDir, args[1:])
	case "use":
		return authCredUse(ctx, dataDir, args[1:])
	case "ls":
		// `auth ls <integration>` lists that integration's credentials; bare `auth ls`
		// falls back to the model-provider listing.
		if len(args) >= 2 {
			return authCredList(ctx, dataDir, args[1])
		}
		return authList(ctx, store)
	case "list":
		return authList(ctx, store)
	case "rm", "remove", "delete":
		// An <integration>/<name> reference removes a credential; a bare provider name
		// removes a model-provider key.
		if len(args) >= 2 && strings.Contains(args[1], "/") {
			return authCredRemove(ctx, store, dataDir, args[1:])
		}
		return authRemove(ctx, store, args[1:])
	default:
		return fmt.Errorf("auth: unknown subcommand %q (want set, set-app, list, add, use, ls, rm, or rm-app)", args[0])
	}
}

// authSetApp stores a GitHub App identity in the vault: the App id it signs its
// assertions with, the installation it acts as, and the private key it signs with.
//
// The three land under the same references `flynn review` resolves, and the vault is
// consulted before the environment, so a stored identity wins over an exported one and
// a review needs no variable set at all. Only the key is a secret; an App id and an
// installation id identify, they do not authenticate. All three are stored together
// because a partial App identity is refused rather than silently downgraded to a
// weaker credential, so storing them separately could only produce a broken state.
//
// The key is parsed before it is stored. A file that is not an RSA private key fails
// here, where the operator is watching, rather than on the first review.
func authSetApp(ctx context.Context, store *vault.Store, args []string, readFile func(string) ([]byte, error), removeFile func(string) error) error {
	fs := flag.NewFlagSet("auth set-app", flag.ContinueOnError)
	var (
		issuer       = fs.String("issuer", "", "the App id, which signs its assertions (App settings: \"App ID\")")
		installation = fs.String("installation", "", "the installation id the App acts as")
		keyFile      = fs.String("key-file", "", "path to the App's private key in PEM form")
		forget       = fs.Bool("forget-key-file", false, "delete the key file once the vault holds it, so the key stops living in plaintext on disk")
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: flynn auth set-app --issuer <app-id> --installation <id> --key-file <path> [--forget-key-file]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *issuer == "" || *installation == "" || *keyFile == "" {
		fs.Usage()
		return errors.New("auth: set-app needs --issuer, --installation, and --key-file")
	}
	if _, err := strconv.ParseInt(*issuer, 10, 64); err != nil {
		return fmt.Errorf("auth: --issuer %q is not an App id", *issuer)
	}
	if _, err := strconv.ParseInt(*installation, 10, 64); err != nil {
		return fmt.Errorf("auth: --installation %q is not an installation id", *installation)
	}

	pemBytes, err := readFile(*keyFile)
	if err != nil {
		return fmt.Errorf("auth: read App private key: %w", err)
	}
	if _, err := github.ParsePrivateKey(pemBytes); err != nil {
		return fmt.Errorf("auth: %s is not an App private key: %w", *keyFile, err)
	}

	// The key is written last. A failure part-way leaves no key in the vault, and the
	// review's own check refuses a part-configured App rather than acting on one.
	for _, entry := range []struct {
		ref   string
		value secret.Text
	}{
		{refAppIssuer, secret.New(*issuer)},
		{refAppInstallation, secret.New(*installation)},
		{refAppKey, secret.New(string(pemBytes))},
	} {
		if err := store.Set(ctx, entry.ref, entry.value); err != nil {
			return fmt.Errorf("auth: store %s: %w", entry.ref, err)
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "Stored the GitHub App identity in the vault (%s, %s, %s).\n",
		refAppIssuer, refAppInstallation, refAppKey)

	if !*forget {
		_, _ = fmt.Fprintf(os.Stdout, "The key is still in plaintext at %s. Delete it, or re-run with --forget-key-file.\n", *keyFile)
		return nil
	}
	if err := removeFile(*keyFile); err != nil {
		return fmt.Errorf("auth: the vault holds the key, but deleting %s failed: %w", *keyFile, err)
	}
	_, _ = fmt.Fprintf(os.Stdout, "Deleted %s. The key now exists only in the vault.\n", *keyFile)
	return nil
}

// authRemoveApp forgets the stored GitHub App identity, which is how a compromised
// key is rotated: remove, then set-app with the newly generated one. Removing a
// reference that was never stored is not an error, so a partial identity left by an
// interrupted set-app can always be cleared.
func authRemoveApp(ctx context.Context, store *vault.Store) error {
	for _, ref := range []string{refAppIssuer, refAppInstallation, refAppKey} {
		if err := store.Delete(ctx, ref); err != nil {
			return fmt.Errorf("auth: remove %s: %w", ref, err)
		}
	}
	_, _ = fmt.Fprintln(os.Stdout, "Removed the GitHub App identity from the vault.")
	return nil
}

func authSet(ctx context.Context, store *vault.Store, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: flynn auth set <provider> (one of %s)", strings.Join(provider.Providers(), ", "))
	}
	ref, ok := provider.KeyRef(args[0])
	if !ok {
		if isKnownProvider(args[0]) {
			return fmt.Errorf("auth: provider %q needs no API key", args[0])
		}
		return fmt.Errorf("auth: unknown provider %q (want one of %s)", args[0], strings.Join(provider.Providers(), ", "))
	}
	key, err := promptHidden(fmt.Sprintf("Enter API key for %s: ", args[0]))
	if err != nil {
		return err
	}
	if key.Empty() {
		return errors.New("auth: no key entered")
	}
	if err := store.Set(ctx, ref, key); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Stored %s in the vault. It is encrypted at rest and revealed only to call the model.\n", ref)
	return nil
}

func authRemove(ctx context.Context, store *vault.Store, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn auth rm <provider>")
	}
	ref, ok := provider.KeyRef(args[0])
	if !ok {
		if isKnownProvider(args[0]) {
			return fmt.Errorf("auth: provider %q needs no API key", args[0])
		}
		return fmt.Errorf("auth: unknown provider %q", args[0])
	}
	if err := store.Delete(ctx, ref); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Removed %s from the vault.\n", ref)
	return nil
}

// isKnownProvider reports whether name is a provider Flynn can resolve, so a
// keyless provider can be told apart from a genuine typo when there is no key to set.
func isKnownProvider(name string) bool {
	for _, p := range provider.Providers() {
		if p == name {
			return true
		}
	}
	return false
}

func authList(ctx context.Context, store *vault.Store) error {
	for _, name := range provider.Providers() {
		ref, ok := provider.KeyRef(name)
		if !ok {
			// A keyless provider (a local server) has nothing to store.
			_, _ = fmt.Fprintf(os.Stdout, "  %-10s no key required\n", name)
			continue
		}
		status := "not set"
		if _, err := store.Lookup(ctx, ref); err == nil {
			status = "stored"
		}
		_, _ = fmt.Fprintf(os.Stdout, "  %-10s %s (%s)\n", name, status, ref)
	}
	// A stored App identity authenticates every review, so it belongs in the same
	// listing rather than being discoverable only by running one.
	status := "not set"
	if _, err := store.Lookup(ctx, refAppKey); err == nil {
		status = "stored"
	}
	_, _ = fmt.Fprintf(os.Stdout, "  %-10s %s (%s)\n", "github app", status, refAppKey)
	return nil
}

// promptHidden reads a line from the terminal without echoing it, so a secret
// never appears on screen or in shell history. It requires an interactive
// terminal; piping a secret in is refused so it is not captured in a process
// listing or a script.
func promptHidden(label string) (secret.Text, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return secret.Text{}, errors.New("auth: a terminal is required to enter a secret (do not pipe it in)")
	}
	fmt.Fprint(os.Stderr, label)
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(os.Stderr)
	if err != nil {
		return secret.Text{}, fmt.Errorf("auth: read secret: %w", err)
	}
	return secret.New(strings.TrimRight(string(b), "\r\n")), nil
}

// terminalPassphrase is the sealed-file fallback's passphrase source for
// interactive commands: it prompts on the terminal, confirming a new passphrase
// when a vault is first created so a typo cannot lock the vault permanently. When
// there is no terminal it defers to the environment passphrase.
func terminalPassphrase(newVault bool) (secret.Text, error) {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return vault.EnvPassphrase(newVault)
	}
	p, err := promptHidden("Vault passphrase: ")
	if err != nil {
		return secret.Text{}, err
	}
	if newVault {
		again, err := promptHidden("Confirm vault passphrase: ")
		if err != nil {
			return secret.Text{}, err
		}
		if !p.Equal(again) {
			return secret.Text{}, errors.New("auth: passphrases did not match")
		}
	}
	return p, nil
}
