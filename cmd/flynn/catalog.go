package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/credential"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/integrations"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/vault"
)

// runIntegrations implements `flynn integrations <subcommand>`: the catalog of
// extensions the agent can use, and a way to run one operation directly.
//
//	flynn integrations ls                       list the catalog and its status
//	flynn integrations show <extension>         show an extension's operations
//	flynn integrations call <ext> <op> [json]   run one operation and print the result
func runIntegrations(args []string, dataDir string) error {
	if len(args) == 0 {
		return errors.New("usage: flynn integrations <ls|show|call>")
	}
	ctx := context.Background()
	switch args[0] {
	case "ls", "list":
		return integrationsList(ctx, dataDir)
	case "show":
		return integrationsShow(ctx, dataDir, args[1:])
	case "call":
		return integrationsCall(ctx, dataDir, args[1:])
	default:
		return fmt.Errorf("integrations: unknown subcommand %q (want ls, show, or call)", args[0])
	}
}

// integrationRuntime bundles the resource store and the wired integration handler so
// the integration commands resolve credentials and run operation flows the same way
// the agent does.
type integrationRuntime struct {
	store  resource.Store
	creds  *credential.Store
	loader *extension.Loader
	closer func() error
}

// openIntegrationRuntime opens the durable store, syncs the official catalog into it,
// and wires the integration handler with the credential store and the vault, so an
// operation runs through the governed transport with the right credential.
func openIntegrationRuntime(ctx context.Context, dataDir string) (*integrationRuntime, error) {
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return nil, err
	}
	reg, err := missionRegistry()
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	store := durable.Resources(reg)
	if _, err := catalog.Sync(ctx, store); err != nil {
		_ = durable.Close()
		return nil, err
	}
	creds := credential.NewStore(store)
	h := integrations.NewHandler(
		integrations.WithSecrets(vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))),
		integrations.WithCredentials(creds),
	)
	ereg := extension.NewRegistry()
	if err := ereg.Register(h); err != nil {
		_ = durable.Close()
		return nil, err
	}
	return &integrationRuntime{
		store:  store,
		creds:  creds,
		loader: extension.NewLoader(ereg),
		closer: durable.Close,
	}, nil
}

// integrationsList prints the catalog: every extension, where it came from, whether
// it is ready to use, and the capabilities it provides.
func integrationsList(ctx context.Context, dataDir string) error {
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	exts, err := rt.store.List(ctx, extension.Kind, resource.Scope{}, nil)
	if err != nil {
		return err
	}
	if len(exts) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no integrations available")
		return nil
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].Name < exts[j].Name })

	_, _ = fmt.Fprintf(os.Stdout, "  %-16s %-18s %-9s %s\n", "INTEGRATION", "STATUS", "SOURCE", "CAPABILITIES")
	for _, r := range exts {
		spec, _ := extension.DecodeSpec(r)
		source := r.Labels[catalog.SourceLabel]
		if source == "" {
			source = "user"
		}
		status, err := integrationStatus(ctx, rt.creds, r.Name, spec)
		if err != nil {
			return err
		}
		caps := "-"
		if len(spec.Capabilities) > 0 {
			caps = strings.Join(spec.Capabilities, ",")
		}
		_, _ = fmt.Fprintf(os.Stdout, "  %-16s %-18s %-9s %s\n", r.Name, status, source, caps)
	}
	return nil
}

// integrationStatus reports whether an extension is ready to use: one that needs no
// credential is always ready; one that needs a credential is ready only once one is
// configured.
func integrationStatus(ctx context.Context, creds *credential.Store, name string, spec extension.Spec) (string, error) {
	if spec.Auth.Type == "" || spec.Auth.Type == "none" {
		return "ready", nil
	}
	cs, err := creds.List(ctx, name)
	if err != nil {
		return "", err
	}
	if len(cs) > 0 {
		return "configured", nil
	}
	return "needs credential", nil
}

// integrationsShow prints an extension's operations and how to configure it.
func integrationsShow(ctx context.Context, dataDir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn integrations show <extension>")
	}
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, args[0])
	if errors.Is(err, resource.ErrNotFound) {
		return fmt.Errorf("integrations: unknown integration %q (see flynn integrations ls)", args[0])
	}
	if err != nil {
		return err
	}
	if _, err := rt.loader.Load(ctx, r); err != nil {
		return err
	}
	spec, _ := extension.DecodeSpec(r)
	_, _ = fmt.Fprintf(os.Stdout, "%s  (auth: %s)\n", args[0], orNone(spec.Auth.Type))
	tools := rt.loader.Tools()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Def().Name < tools[j].Def().Name })
	for _, tool := range tools {
		def := tool.Def()
		_, _ = fmt.Fprintf(os.Stdout, "  %-22s %s\n", def.Name, def.Description)
	}
	return nil
}

// integrationsCall runs one operation and prints its result, so an integration can be
// verified end to end from the command line.
func integrationsCall(ctx context.Context, dataDir string, args []string) error {
	if len(args) < 2 {
		return errors.New(`usage: flynn integrations call <extension> <operation> [json-input]`)
	}
	extName, opName := args[0], args[1]
	input := json.RawMessage(`{}`)
	if len(args) >= 3 && args[2] != "" {
		input = json.RawMessage(args[2])
	}

	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, extName)
	if errors.Is(err, resource.ErrNotFound) {
		return fmt.Errorf("integrations: unknown integration %q (see flynn integrations ls)", extName)
	}
	if err != nil {
		return err
	}
	if _, err := rt.loader.Load(ctx, r); err != nil {
		return err
	}

	var tool mission.Tool
	for _, t := range rt.loader.Tools() {
		if t.Def().Name == opName {
			tool = t
			break
		}
	}
	if tool == nil {
		return fmt.Errorf("integrations: %q has no operation %q (see flynn integrations show %s)", extName, opName, extName)
	}
	out, err := tool.Invoke(ctx, input)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(os.Stdout, out)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
