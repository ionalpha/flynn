package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/provider"
	"github.com/ionalpha/flynn/secret"
)

// localDiscovery asks the local model servers that are running which models they
// hold. It reads only the process environment, never the vault: a listing must not
// stop to ask for a passphrase, and a server's address is configuration, not a
// secret. It is a variable so a test can stand a fake in for the machine.
var localDiscovery = discoverEnvLocal

func discoverEnvLocal(ctx context.Context) []provider.LocalModel {
	return provider.DiscoverLocal(ctx, secret.EnvSource{}, nil)
}

// withProviderEndpoints wraps base so a --model spec can name a server declared in
// the data directory's providers file. A missing file leaves base as it is.
func withProviderEndpoints(base secret.Source, dataDir string) (secret.Source, error) {
	eps, err := provider.LoadEndpoints(dataDir)
	if err != nil {
		return nil, err
	}
	if len(eps.Providers) == 0 {
		return base, nil
	}
	return provider.WithEndpoints(base, eps), nil
}

// renderDiscovered lists the models running local servers reported. They are labelled
// unverified because the catalog has not seen them: the size and the tool capability
// are the server's word.
func renderDiscovered(out io.Writer, models []provider.LocalModel) {
	if len(models) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "\nrunning on this machine (local, unverified: reported by the server, not in the catalog):")
	tw := tabwriter.NewWriter(out, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "MODEL\tKIND\tSIZE\tTOOLS")
	for _, m := range models {
		size := strings.TrimSpace(m.Params + " " + m.Quant)
		if size == "" {
			size = "-"
		}
		tools := "unknown"
		if m.Tools {
			tools = "yes"
		}
		_, _ = fmt.Fprintf(tw, "%s\tlocal\t%s\t%s\n", m.Spec(), size, tools)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(out, "run one with `flynn goal --model %s \"<objective>\"`\n", models[0].Spec())
}

// providerEndpointHelp is the part of `flynn models --help` about where models are
// served from: the base-URL variables and the providers file. It is built from the
// provider package's own lists so it cannot name a variable that nothing reads.
func providerEndpointHelp() string {
	return "\nServer addresses (environment variables):\n  " +
		strings.Join(provider.BaseURLEnvVars(), "\n  ") +
		"\nMove a provider to another address with its variable. A server that needs no key (llamacpp, ollama,\n" +
		"lmstudio) listens on localhost by default; " + strings.Join(provider.VisionEnvVars(), ", ") +
		" turn image input on.\n" +
		"\nNamed servers (" + provider.ProvidersFile + " in the data directory), used as --model <name>:<model>:\n" +
		"  {\"allow_hosts\": [\"gpu-box\"],\n" +
		"   \"providers\": {\"box\": {\"kind\": \"ollama\", \"base_url\": \"http://gpu-box:11434/v1\"},\n" +
		"                 \"gw\":  {\"kind\": \"openai\", \"base_url\": \"https://gw.example.com/v1\", \"key\": \"GW_API_KEY\"}}}\n" +
		"kind is openai, llamacpp, ollama or lmstudio. key names a secret in the vault or environment; a server\n" +
		"with no key is refused unless its host is localhost or listed in allow_hosts.\n"
}
