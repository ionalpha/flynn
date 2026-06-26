package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/vault"
)

// runAuth implements the `flynn auth` command group: managing the credentials the
// agent uses, stored in the encrypted vault rather than handed in on every run.
//
//	flynn auth set <provider>   read an API key (hidden) and seal it
//	flynn auth rm  <provider>   remove a stored key
//	flynn auth list             show which providers have a key stored
func runAuth(args []string, dataDir string) error {
	if len(args) == 0 {
		return errors.New(`usage: flynn auth <set|rm|list> [provider]`)
	}
	store := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))
	ctx := context.Background()

	switch args[0] {
	case "set":
		return authSet(ctx, store, args[1:])
	case "rm", "remove", "delete":
		return authRemove(ctx, store, args[1:])
	case "list", "ls":
		return authList(ctx, store)
	default:
		return fmt.Errorf("auth: unknown subcommand %q (want set, rm, or list)", args[0])
	}
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
