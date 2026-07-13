package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/extension/catalog"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/integrations"
	"github.com/ionalpha/flynn/internal/ops"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
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
	var run func(context.Context, *integrationRuntime, io.Writer, []string) error
	switch args[0] {
	case "ls", "list":
		run = integrationsList
	case "show":
		run = integrationsShow
	case "call":
		run = integrationsCall
	default:
		return fmt.Errorf("integrations: unknown subcommand %q (want ls, show, or call)", args[0])
	}
	ctx := context.Background()
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()
	return run(ctx, rt, os.Stdout, args[1:])
}

// integrationRuntime bundles the resource store and the wired integration handler so
// the integration commands resolve credentials and run operation flows the same way
// the agent does.
type integrationRuntime struct {
	store  resource.Store
	creds  *credential.Store
	svc    *service.Store
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
	creds, loader, err := wireExtensions(store, dataDir)
	if err != nil {
		_ = durable.Close()
		return nil, err
	}
	return &integrationRuntime{
		store:  store,
		creds:  creds,
		svc:    service.NewStore(store),
		loader: loader,
		closer: durable.Close,
	}, nil
}

// wireExtensions assembles the extension execution stack over an existing resource
// store: the credential store, the vault-backed secret source, and a loader whose
// registry serves both the integration surface (API operations) and the ops surface
// (hosting providers) from one operation engine. It is shared by the integration
// commands and the served supervision loop so an operation runs through identical
// credential resolution, role enforcement, and egress confinement everywhere.
func wireExtensions(store resource.Store, dataDir string) (*credential.Store, *extension.Loader, error) {
	creds := credential.NewStore(store)
	opts := []integrations.Option{
		integrations.WithSecrets(vault.New(dataDir, vault.WithPassphrase(terminalPassphrase))),
		integrations.WithCredentials(creds),
	}
	ereg := extension.NewRegistry()
	if err := ereg.Register(integrations.NewHandler(opts...)); err != nil {
		return nil, nil, err
	}
	if err := ops.RegisterWith(ereg, opts...); err != nil {
		return nil, nil, err
	}
	return creds, extension.NewLoader(ereg), nil
}

// integrationsList prints the catalog: every extension, where it came from, whether
// it is ready to use, and the capabilities it provides.
func integrationsList(ctx context.Context, rt *integrationRuntime, out io.Writer, _ []string) error {
	exts, err := rt.store.List(ctx, extension.Kind, resource.Scope{}, nil)
	if err != nil {
		return err
	}
	if len(exts) == 0 {
		_, _ = fmt.Fprintln(out, "no integrations available")
		return nil
	}
	sort.Slice(exts, func(i, j int) bool { return exts[i].Name < exts[j].Name })

	_, _ = fmt.Fprintf(out, "  %-16s %-18s %-9s %s\n", "INTEGRATION", "STATUS", "SOURCE", "CAPABILITIES")
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
		_, _ = fmt.Fprintf(out, "  %-16s %-18s %-9s %s\n", r.Name, status, source, caps)
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
func integrationsShow(ctx context.Context, rt *integrationRuntime, out io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn integrations show <extension>")
	}
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
	_, _ = fmt.Fprintf(out, "%s  (auth: %s)\n", args[0], orNone(spec.Auth.Type))
	tools := rt.loader.Tools()
	sort.Slice(tools, func(i, j int) bool { return tools[i].Def().Name < tools[j].Def().Name })
	for _, tool := range tools {
		def := tool.Def()
		_, _ = fmt.Fprintf(out, "  %-22s %s\n", def.Name, def.Description)
	}
	return nil
}

// integrationsCall runs one operation and prints its result, so an integration can be
// verified end to end from the command line.
func integrationsCall(ctx context.Context, rt *integrationRuntime, out io.Writer, args []string) error {
	if len(args) < 2 {
		return errors.New(`usage: flynn integrations call <extension> <operation> [json-input]`)
	}
	extName, opName := args[0], args[1]
	input := json.RawMessage(`{}`)
	if len(args) >= 3 && args[2] != "" {
		input = json.RawMessage(args[2])
	}

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
	result, err := tool.Invoke(ctx, input)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintln(out, result)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
