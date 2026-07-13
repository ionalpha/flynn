package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/internal/ops"
	"github.com/ionalpha/flynn/internal/service"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// deployArgs is a parsed `flynn deploy` invocation: which hosting extension to drive,
// what to register the resulting workload as, and the input the provider's deploy
// operation receives.
type deployArgs struct {
	ext    string
	name   string
	target string
	input  json.RawMessage
}

// parseDeployArgs reads the deploy command line. The extension name is positional and
// must come first, so a bare flag (or nothing) is a usage error rather than a deploy of
// an extension named "--name".
func parseDeployArgs(args []string) (deployArgs, error) {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return deployArgs{}, errors.New("usage: flynn deploy <extension> [--name svc] [--target t] [json-input]")
	}
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "", "service name to register (defaults to the extension name)")
	target := fs.String("target", "", "what is being deployed: static-site, container, or vps")
	if err := fs.Parse(args[1:]); err != nil {
		return deployArgs{}, err
	}
	parsed := deployArgs{ext: args[0], name: *name, target: *target, input: json.RawMessage(`{}`)}
	if rest := fs.Args(); len(rest) > 0 {
		parsed.input = json.RawMessage(strings.Join(rest, " "))
	}
	return parsed, nil
}

// runDeploy implements `flynn deploy <extension> [flags] [json-input]`. It runs a
// hosting extension's deploy operation through the same governed transport and
// credential check every operation uses, then materializes a managed Service from the
// result so the workload is tracked rather than fire-and-forget.
//
//	flynn deploy <extension> [--name svc] [--target static-site|container|vps] [json-input]
func runDeploy(args []string, dataDir string) error {
	parsed, err := parseDeployArgs(args)
	if err != nil {
		return err
	}
	ctx := context.Background()
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()
	return deployExtension(ctx, rt, os.Stdout, clock.System{}, parsed)
}

// deployExtension runs one hosting extension's deploy operation and registers the
// resulting workload as a Service. The clock is supplied by the caller so the recorded
// deploy time comes from the agent's source of time rather than the wall clock.
func deployExtension(ctx context.Context, rt *integrationRuntime, out io.Writer, clk clock.Clock, a deployArgs) error {
	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, a.ext)
	if errors.Is(err, resource.ErrNotFound) {
		return fmt.Errorf("deploy: unknown extension %q (see flynn integrations ls)", a.ext)
	}
	if err != nil {
		return err
	}
	spec, _ := extension.DecodeSpec(r)
	if _, ok := spec.Surface(extension.SurfaceOps); !ok {
		return fmt.Errorf("deploy: %q is not a hosting provider (it declares no ops surface)", a.ext)
	}
	tgt := service.Target(a.target)
	if !tgt.Valid() {
		return fmt.Errorf("deploy: unknown --target %q (want static-site, container, or vps)", a.target)
	}
	if _, err := rt.loader.Load(ctx, r); err != nil {
		return err
	}

	var deploy mission.Tool
	for _, t := range rt.loader.Tools() {
		if t.Def().Name == ops.OpDeploy {
			deploy = t
			break
		}
	}
	if deploy == nil {
		return fmt.Errorf("deploy: %q declares no %q operation", a.ext, ops.OpDeploy)
	}

	result, err := deploy.Invoke(ctx, a.input)
	if err != nil {
		return fmt.Errorf("deploy: the deploy operation failed: %w", err)
	}

	svcName := a.name
	if svcName == "" {
		svcName = a.ext
	}
	if tgt == "" {
		tgt = firstTarget(spec)
	}
	provider := spec.Provider
	if provider == "" {
		provider = a.ext
	}

	url, externalID, address := extractDeployResult(result)
	svcSpec := service.Spec{
		Provider:     provider,
		Target:       tgt,
		ExternalID:   externalID,
		URL:          url,
		DesiredState: service.StateRunning,
		Credential:   spec.Auth.CredentialRef,
		Address:      address,
	}
	status := service.Status{
		Phase:       "deployed",
		ObservedURL: url,
		LastDeploy:  clk.Now().UTC().Format(time.RFC3339),
	}
	if _, err := rt.svc.Put(ctx, svcName, svcSpec, status); err != nil {
		return fmt.Errorf("deploy: workload deployed but registering the service failed: %w", err)
	}

	_, _ = fmt.Fprintf(out, "Deployed %s via %s", svcName, provider)
	if url != "" {
		_, _ = fmt.Fprintf(out, " -> %s", url)
	}
	_, _ = fmt.Fprintln(out)
	_, _ = fmt.Fprintf(out, "Registered service %q (flynn services ls). Provider result:\n%s\n", svcName, result)
	return nil
}

// firstTarget returns the first hosting target an extension's ops surface declares, so
// a deploy that did not pass --target records what the provider says it hosts.
func firstTarget(spec extension.Spec) service.Target {
	block, ok := spec.Surface(extension.SurfaceOps)
	if !ok {
		return ""
	}
	var opsSpec ops.Spec
	if err := json.Unmarshal(block, &opsSpec); err != nil {
		return ""
	}
	if len(opsSpec.Targets) > 0 {
		return opsSpec.Targets[0]
	}
	return ""
}

// extractDeployResult best-effort reads the live URL, the provider's external id, and
// any provider-opaque addressing from a deploy operation's result. Providers return
// varied shapes, so it scans the common field names; anything it cannot find is left
// empty, and the full result is always printed for the operator. The result may be a
// bare object or wrapped under a "result" key (the shape Cloudflare and others use).
// An "address" object, when present, is recorded verbatim so a later status or teardown
// can re-find the workload (e.g. the account id and project name a Pages deploy used).
func extractDeployResult(raw string) (url, id string, address map[string]string) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", "", nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", "", nil
	}
	if inner, ok := m["result"].(map[string]any); ok {
		m = inner
	}
	url = firstString(m, "url", "deployment_url", "live_url", "subdomain")
	id = firstString(m, "id", "deployment_id", "external_id", "uid")
	if addr, ok := m["address"].(map[string]any); ok {
		address = map[string]string{}
		for k, val := range addr {
			if s, ok := val.(string); ok && s != "" {
				address[k] = s
			}
		}
		if len(address) == 0 {
			address = nil
		}
	}
	return url, id, address
}

// firstString returns the first key in m whose value is a non-empty string.
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := m[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// runServices implements `flynn services <ls|rm>`: the deployed workloads Flynn
// tracks. Listing shows what is deployed where; rm retires a service record (the
// bookkeeping half of a teardown, after the provider's teardown operation removes the
// remote workload).
func runServices(args []string, dataDir string) error {
	if len(args) == 0 {
		args = []string{"ls"}
	}
	var run func(context.Context, *integrationRuntime, io.Writer, []string) error
	switch args[0] {
	case "ls", "list":
		run = servicesList
	case "rm", "remove":
		run = servicesRemove
	default:
		return fmt.Errorf("services: unknown subcommand %q (want ls or rm)", args[0])
	}
	ctx := context.Background()
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()
	return run(ctx, rt, os.Stdout, args[1:])
}

func servicesList(ctx context.Context, rt *integrationRuntime, out io.Writer, _ []string) error {
	svcs, err := rt.svc.List(ctx)
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		_, _ = fmt.Fprintln(out, "no services deployed")
		return nil
	}
	_, _ = fmt.Fprintf(out, "  %-16s %-12s %-13s %-9s %s\n", "NAME", "PROVIDER", "TARGET", "STATE", "URL")
	for _, s := range svcs {
		_, _ = fmt.Fprintf(out, "  %-16s %-12s %-13s %-9s %s\n",
			s.Name, s.Spec.Provider, orDash(string(s.Spec.Target)), orDash(string(s.Spec.DesiredState)), orDash(s.Spec.URL))
	}
	return nil
}

func servicesRemove(ctx context.Context, rt *integrationRuntime, out io.Writer, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn services rm <name>")
	}
	if err := rt.svc.Delete(ctx, args[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Removed service %q from tracking.\n", args[0])
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
