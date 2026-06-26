package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/term"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/vault"
)

// resolveModelOrOnboard resolves the model and, when the requested provider has no
// credential, recovers instead of failing with a raw error. It first prefers a
// provider whose key is already in the vault (so a stored key is used without
// asking), and only when none is configured and a terminal is attached does it run
// the first-run setup that stores a key. Without a terminal and with nothing
// configured, the original error is returned for the caller to surface.
func resolveModelOrOnboard(ctx context.Context, modelSpec, dataDir string) (llm.Model, error) {
	model, err := resolveModel(ctx, modelSpec, dataDir)
	if !errors.Is(err, provider.ErrCredentialNotSet) {
		return model, err
	}

	// The requested provider has no key. If another provider is already configured,
	// use it rather than prompting; the default model spec just named a provider the
	// user has not set up.
	configured := configuredProviders(ctx, credentialSource(dataDir))
	switch {
	case len(configured) == 1:
		fmt.Fprintf(os.Stderr, "Using %s (already configured). Pass --model to choose another.\n", configured[0])
		return resolveModel(ctx, configured[0], dataDir)
	case len(configured) > 1 && term.IsTerminal(int(os.Stdin.Fd())):
		in := bufio.NewReader(os.Stdin)
		fmt.Fprintf(os.Stderr, "Configured providers: %s\n", strings.Join(configured, ", "))
		name, perr := promptVisible(in, fmt.Sprintf("Provider [%s]: ", configured[0]))
		if perr != nil {
			return nil, perr
		}
		if name == "" {
			name = configured[0]
		}
		return resolveModel(ctx, name, dataDir)
	case len(configured) > 1:
		// Non-interactive with several keys: pick deterministically rather than fail.
		return resolveModel(ctx, configured[0], dataDir)
	}

	// Nothing configured at all.
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		return nil, err
	}
	spec, oerr := onboardCredential(ctx, modelSpec, dataDir)
	if oerr != nil {
		return nil, oerr
	}
	return resolveModel(ctx, spec, dataDir)
}

// configuredProviders returns the known providers that already have a credential in
// src, in the canonical provider order, so a caller can prefer a set-up provider
// over prompting for a new one.
func configuredProviders(ctx context.Context, src secret.Source) []string {
	var out []string
	for _, name := range provider.Providers() {
		ref, ok := provider.KeyRef(name)
		if !ok {
			continue
		}
		if _, err := src.Lookup(ctx, ref); err == nil {
			out = append(out, name)
		}
	}
	return out
}

// keyedProviders lists the providers that take an API key, in canonical order, so
// the credential prompt offers only providers there is actually a key to enter for.
func keyedProviders() []string {
	var out []string
	for _, name := range provider.Providers() {
		if _, ok := provider.KeyRef(name); ok {
			out = append(out, name)
		}
	}
	return out
}

// onboardCredential is the first-run setup: it asks which provider to use
// (defaulting to the one in modelSpec), reads its API key without echoing, and
// seals it in the vault. It returns the model spec to resolve with: the original
// spec when the chosen provider matches it (so a specific model is kept), or the
// bare provider name (its default model) when the user picked a different one.
func onboardCredential(ctx context.Context, modelSpec, dataDir string) (string, error) {
	def, _, _ := strings.Cut(modelSpec, ":")
	if _, ok := provider.KeyRef(def); !ok {
		def = provider.Providers()[0]
	}

	fmt.Fprintln(os.Stderr, "Welcome to Flynn. No model credential is set yet, so let's add one.")
	keyed := keyedProviders()
	fmt.Fprintf(os.Stderr, "Providers: %s\n", strings.Join(keyed, ", "))

	in := bufio.NewReader(os.Stdin)
	name, err := promptVisible(in, fmt.Sprintf("Provider [%s]: ", def))
	if err != nil {
		return "", err
	}
	if name == "" {
		name = def
	}
	ref, ok := provider.KeyRef(name)
	if !ok {
		return "", fmt.Errorf("provider %q does not take an API key here (want one of %s)", name, strings.Join(keyed, ", "))
	}

	key, err := promptHidden(fmt.Sprintf("Enter API key for %s: ", name))
	if err != nil {
		return "", err
	}
	if key.Empty() {
		return "", errors.New("no key entered")
	}

	store := vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))
	if err := store.Set(ctx, ref, key); err != nil {
		return "", err
	}
	fmt.Fprintf(os.Stderr, "Stored %s in the vault (encrypted at rest, revealed only to call the model).\n\n", ref)

	// Keep the caller's specific model when it belongs to the chosen provider;
	// otherwise resolve the chosen provider's default model.
	if name == def {
		return modelSpec, nil
	}
	return name, nil
}

// promptVisible reads one echoed line from in and returns it trimmed. Unlike
// promptHidden it is for non-secret answers (a provider name), so the choice is
// visible as the user types it.
func promptVisible(in *bufio.Reader, label string) (string, error) {
	fmt.Fprint(os.Stderr, label)
	line, err := in.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(line), nil
}
