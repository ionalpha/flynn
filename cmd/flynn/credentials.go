package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/vault"
)

// openCredentialStore opens the durable store and returns a credential facade over
// it, plus a close function. The credential metadata lives in the same durable store
// as every other resource; the secret values live in the vault, separately.
func openCredentialStore(ctx context.Context, dataDir string) (*credential.Store, func() error, error) {
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return nil, nil, err
	}
	reg, err := missionRegistry()
	if err != nil {
		_ = durable.Close()
		return nil, nil, err
	}
	return credential.NewStore(durable.Resources(reg)), durable.Close, nil
}

// authCredAdd implements `flynn auth add <integration> --name <name> [flags]`. It
// reads the secret value with no echo, writes it to the vault under the credential's
// reference, and records the credential metadata. The value never appears on screen
// or in the resource store.
//
//	flynn auth add <integration> --name <name> [--role read|operator|admin]
//	                             [--type bearer|api_key|basic|oauth2] [--default]
func authCredAdd(ctx context.Context, vaultStore *vault.Store, dataDir string, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: flynn auth add <integration> --name <name> [--role r] [--type t] [--default]")
	}
	integration := args[0]

	fs := flag.NewFlagSet("auth add", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "", "credential name within the integration")
	role := fs.String("role", "", "role: read, operator, or admin")
	authType := fs.String("type", "", "auth scheme: bearer, api_key, basic, oauth2")
	isDefault := fs.Bool("default", false, "make this the integration's default credential")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("auth add: unexpected arguments %v (the integration must come first, then flags)", fs.Args())
	}
	if *name == "" {
		return errors.New("auth add: --name is required")
	}
	if *role != "" && !credential.Role(*role).Valid() {
		return fmt.Errorf("auth add: unknown role %q (want read, operator, or admin)", *role)
	}

	value, err := promptHidden(fmt.Sprintf("Enter secret for %s/%s: ", integration, *name))
	if err != nil {
		return err
	}
	if value.Empty() {
		return errors.New("auth add: no secret entered")
	}

	credStore, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = closeStore() }()

	spec := credential.Spec{
		Integration: integration,
		Name:        *name,
		AuthType:    *authType,
		Role:        credential.Role(*role),
		IsDefault:   *isDefault,
	}
	if err := addCredential(ctx, credStore, vaultStore, spec, value); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Stored credential %s/%s. The secret is sealed in the vault; only its reference is recorded.\n", integration, *name)
	return nil
}

// addCredential writes a credential's secret to the vault and its metadata to the
// store as one logical step. If recording the metadata fails, the vault write is
// rolled back so a secret is never left orphaned without a credential pointing at it.
// The terminal prompt is factored out so this core is testable without a TTY.
func addCredential(ctx context.Context, credStore *credential.Store, vaultStore *vault.Store, spec credential.Spec, value secret.Text) error {
	ref := credential.VaultRef(spec.Integration, spec.Name)
	if spec.VaultRef != "" {
		ref = spec.VaultRef
	}
	if err := vaultStore.Set(ctx, ref, value); err != nil {
		return err
	}
	if _, err := credStore.Put(ctx, spec); err != nil {
		// Roll back both sides: the vault write just made, and any metadata Put may
		// have persisted before failing (a default Put writes the record before
		// clearing the prior default). Either leftover would be an orphan.
		_ = vaultStore.Delete(ctx, ref)
		_ = credStore.Delete(ctx, spec.Integration, spec.Name)
		return err
	}
	return nil
}

// authCredUse implements `flynn auth use <integration>/<name>`: it makes the named
// credential the integration's default.
func authCredUse(ctx context.Context, dataDir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn auth use <integration>/<name>")
	}
	integration, name, ok := strings.Cut(args[0], "/")
	if !ok || name == "" || integration == "" {
		return errors.New("auth use: expected <integration>/<name>")
	}
	credStore, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = closeStore() }()
	if err := credStore.SetDefault(ctx, integration, name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "%s/%s is now the default credential for %s.\n", integration, name, integration)
	return nil
}

// authCredList implements `flynn auth ls <integration>`: it lists the integration's
// credentials with their role and which is the default.
func authCredList(ctx context.Context, dataDir, integration string) error {
	credStore, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = closeStore() }()
	creds, err := credStore.List(ctx, integration)
	if err != nil {
		return err
	}
	if len(creds) == 0 {
		_, _ = fmt.Fprintf(os.Stdout, "no credentials for %s\n", integration)
		return nil
	}
	writeCredTable(os.Stdout, creds)
	return nil
}

// authCredRemove implements `flynn auth rm <integration>/<name>`: it removes the
// credential metadata and its vault secret together.
func authCredRemove(ctx context.Context, vaultStore *vault.Store, dataDir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn auth rm <integration>/<name>")
	}
	integration, name, ok := strings.Cut(args[0], "/")
	if !ok || name == "" || integration == "" {
		return errors.New("auth rm: expected <integration>/<name>")
	}
	credStore, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = closeStore() }()
	if err := removeCredential(ctx, credStore, vaultStore, integration, name); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Removed credential %s/%s and its vault secret.\n", integration, name)
	return nil
}

// removeCredential deletes a credential's metadata and its vault secret. The vault
// reference is read from the credential before the metadata is removed, so a custom
// reference is honoured. A missing credential is an error.
func removeCredential(ctx context.Context, credStore *credential.Store, vaultStore *vault.Store, integration, name string) error {
	cred, err := credStore.Get(ctx, integration, name)
	if err != nil {
		return err
	}
	if err := credStore.Delete(ctx, integration, name); err != nil {
		return err
	}
	// The secret is best-effort: the metadata is gone, so a leftover vault entry is
	// unreferenced, but removing it keeps the vault tidy.
	_ = vaultStore.Delete(ctx, cred.Ref())
	return nil
}

// writeCredTable renders credentials as an aligned table.
func writeCredTable(w *os.File, creds []credential.Credential) {
	_, _ = fmt.Fprintf(w, "  %-16s %-9s %-8s %s\n", "NAME", "ROLE", "DEFAULT", "TYPE")
	for _, c := range creds {
		def := ""
		if c.Spec.IsDefault {
			def = "*"
		}
		role := string(c.Spec.Role)
		if role == "" {
			role = "-"
		}
		authType := c.Spec.AuthType
		if authType == "" {
			authType = "-"
		}
		_, _ = fmt.Fprintf(w, "  %-16s %-9s %-8s %s\n", c.Spec.Name, role, def, authType)
	}
}

// runIntegrations implements `flynn integrations <subcommand>`: a view over the
// integrations that have credentials configured.
//
//	flynn integrations ls   list integrations and their default credential
func runIntegrations(args []string, dataDir string) error {
	if len(args) == 0 {
		return errors.New("usage: flynn integrations <ls>")
	}
	ctx := context.Background()
	switch args[0] {
	case "ls", "list":
		return integrationsList(ctx, dataDir)
	default:
		return fmt.Errorf("integrations: unknown subcommand %q (want ls)", args[0])
	}
}

// integrationsList prints each integration that has credentials, with how many it
// has and which is the default.
func integrationsList(ctx context.Context, dataDir string) error {
	credStore, closeStore, err := openCredentialStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = closeStore() }()
	all, err := credStore.All(ctx)
	if err != nil {
		return err
	}
	if len(all) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no integrations configured")
		return nil
	}

	type summary struct {
		count int
		deflt string
	}
	byIntegration := map[string]*summary{}
	var order []string
	for _, c := range all {
		s := byIntegration[c.Spec.Integration]
		if s == nil {
			s = &summary{}
			byIntegration[c.Spec.Integration] = s
			order = append(order, c.Spec.Integration)
		}
		s.count++
		if c.Spec.IsDefault {
			s.deflt = c.Spec.Name
		}
	}
	_, _ = fmt.Fprintf(os.Stdout, "  %-20s %-6s %s\n", "INTEGRATION", "CREDS", "DEFAULT")
	for _, name := range order {
		s := byIntegration[name]
		deflt := s.deflt
		if deflt == "" {
			deflt = "-"
		}
		_, _ = fmt.Fprintf(os.Stdout, "  %-20s %-6d %s\n", name, s.count, deflt)
	}
	return nil
}
