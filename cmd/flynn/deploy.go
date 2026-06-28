package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/extension"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/ops"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/service"
)

// runDeploy implements `flynn deploy <extension> [flags] [json-input]`. It runs a
// hosting extension's deploy operation through the same governed transport and
// credential check every operation uses, then materializes a managed Service from the
// result so the workload is tracked rather than fire-and-forget.
//
//	flynn deploy <extension> [--name svc] [--target static-site|container|vps] [json-input]
func runDeploy(args []string, dataDir string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return errors.New("usage: flynn deploy <extension> [--name svc] [--target t] [json-input]")
	}
	extName := args[0]

	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	name := fs.String("name", "", "service name to register (defaults to the extension name)")
	target := fs.String("target", "", "what is being deployed: static-site, container, or vps")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	input := json.RawMessage(`{}`)
	if rest := fs.Args(); len(rest) > 0 {
		input = json.RawMessage(strings.Join(rest, " "))
	}

	ctx := context.Background()
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	r, err := rt.store.Get(ctx, extension.Kind, resource.Scope{}, extName)
	if errors.Is(err, resource.ErrNotFound) {
		return fmt.Errorf("deploy: unknown extension %q (see flynn integrations ls)", extName)
	}
	if err != nil {
		return err
	}
	spec, _ := extension.DecodeSpec(r)
	if _, ok := spec.Surface(extension.SurfaceOps); !ok {
		return fmt.Errorf("deploy: %q is not a hosting provider (it declares no ops surface)", extName)
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
		return fmt.Errorf("deploy: %q declares no %q operation", extName, ops.OpDeploy)
	}

	out, err := deploy.Invoke(ctx, input)
	if err != nil {
		return fmt.Errorf("deploy: the deploy operation failed: %w", err)
	}

	svcName := *name
	if svcName == "" {
		svcName = extName
	}
	tgt := service.Target(*target)
	if !tgt.Valid() {
		return fmt.Errorf("deploy: unknown --target %q (want static-site, container, or vps)", *target)
	}
	if tgt == "" {
		tgt = firstTarget(spec)
	}
	provider := spec.Provider
	if provider == "" {
		provider = extName
	}

	url, externalID := extractDeployResult(out)
	svcSpec := service.Spec{
		Provider:     provider,
		Target:       tgt,
		ExternalID:   externalID,
		URL:          url,
		DesiredState: service.StateRunning,
		Credential:   spec.Auth.CredentialRef,
	}
	status := service.Status{
		Phase:       "deployed",
		ObservedURL: url,
		LastDeploy:  clock.System{}.Now().UTC().Format(time.RFC3339),
	}
	if _, err := rt.svc.Put(ctx, svcName, svcSpec, status); err != nil {
		return fmt.Errorf("deploy: workload deployed but registering the service failed: %w", err)
	}

	_, _ = fmt.Fprintf(os.Stdout, "Deployed %s via %s", svcName, provider)
	if url != "" {
		_, _ = fmt.Fprintf(os.Stdout, " -> %s", url)
	}
	_, _ = fmt.Fprintln(os.Stdout)
	_, _ = fmt.Fprintf(os.Stdout, "Registered service %q (flynn services ls). Provider result:\n%s\n", svcName, out)
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

// extractDeployResult best-effort reads the live URL and the provider's external id
// from a deploy operation's result. Providers return varied shapes, so it scans the
// common field names; anything it cannot find is left empty, and the full result is
// always printed for the operator. The result may be a bare object or wrapped under a
// "result" key (the shape Cloudflare and others use).
func extractDeployResult(raw string) (url, id string) {
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return "", ""
	}
	m, ok := v.(map[string]any)
	if !ok {
		return "", ""
	}
	if inner, ok := m["result"].(map[string]any); ok {
		m = inner
	}
	url = firstString(m, "url", "deployment_url", "live_url", "subdomain")
	id = firstString(m, "id", "deployment_id", "external_id", "uid")
	return url, id
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
	ctx := context.Background()
	switch args[0] {
	case "ls", "list":
		return servicesList(ctx, dataDir)
	case "rm", "remove":
		return servicesRemove(ctx, dataDir, args[1:])
	default:
		return fmt.Errorf("services: unknown subcommand %q (want ls or rm)", args[0])
	}
}

func servicesList(ctx context.Context, dataDir string) error {
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()

	svcs, err := rt.svc.List(ctx)
	if err != nil {
		return err
	}
	if len(svcs) == 0 {
		_, _ = fmt.Fprintln(os.Stdout, "no services deployed")
		return nil
	}
	_, _ = fmt.Fprintf(os.Stdout, "  %-16s %-12s %-13s %-9s %s\n", "NAME", "PROVIDER", "TARGET", "STATE", "URL")
	for _, s := range svcs {
		_, _ = fmt.Fprintf(os.Stdout, "  %-16s %-12s %-13s %-9s %s\n",
			s.Name, s.Spec.Provider, orDash(string(s.Spec.Target)), orDash(string(s.Spec.DesiredState)), orDash(s.Spec.URL))
	}
	return nil
}

func servicesRemove(ctx context.Context, dataDir string, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: flynn services rm <name>")
	}
	rt, err := openIntegrationRuntime(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = rt.closer() }()
	if err := rt.svc.Delete(ctx, args[0]); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(os.Stdout, "Removed service %q from tracking.\n", args[0])
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
