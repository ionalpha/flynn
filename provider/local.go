package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/llm/openai"
	"github.com/ionalpha/flynn/secret"
)

// localKind describes a keyless model server on the operator's machine that speaks
// the OpenAI Chat Completions format. The kinds differ only in where the server
// listens, the environment variables that move it, and how it takes tools; the grant
// a run gets is built from the run's toolset and never reads the provider name, so
// reaching one server as llamacpp, ollama, lmstudio or a configured name offers the
// model the same tools.
type localKind struct {
	name           string
	baseRef        string // environment or vault reference that overrides the base URL
	visionRef      string // truthy value turns image input on
	defaultBaseURL string
	// grammar sends the tools as a decode-time grammar instead of the tools field. It
	// needs a server that honors the grammar request field (llama.cpp). Ollama and LM
	// Studio ignore that field, so a grammar-only request would leave the model with no
	// tools at all; they take the tools field.
	grammar bool
}

var localKinds = []localKind{
	{name: "llamacpp", baseRef: "LLAMACPP_BASE_URL", visionRef: "LLAMACPP_VISION", defaultBaseURL: localLlamaCPPBaseURL, grammar: true},
	{name: "ollama", baseRef: "OLLAMA_BASE_URL", visionRef: "OLLAMA_VISION", defaultBaseURL: "http://localhost:11434/v1"},
	{name: "lmstudio", baseRef: "LMSTUDIO_BASE_URL", visionRef: "LMSTUDIO_VISION", defaultBaseURL: "http://localhost:1234/v1"},
}

func localKindNamed(name string) (localKind, bool) {
	for _, k := range localKinds {
		if k.name == name {
			return k, true
		}
	}
	return localKind{}, false
}

// resolveLocal builds the client for a local kind, with the base URL taken from the
// kind's environment reference or its default.
func resolveLocal(ctx context.Context, src secret.Source, k localKind, model string) (llm.Model, error) {
	baseURL := k.defaultBaseURL
	if u, err := src.Lookup(ctx, k.baseRef); err == nil && u.Expose() != "" {
		baseURL = u.Expose()
	}
	if !llm.SafeBaseURL(baseURL) {
		return nil, fmt.Errorf("provider: %s must be https or http to localhost", k.baseRef)
	}
	vision := false
	if u, err := src.Lookup(ctx, k.visionRef); err == nil {
		vision = truthy(u.Expose())
	}
	return newLocalClient(model, baseURL, k.grammar, vision), nil
}

func newLocalClient(model, baseURL string, grammar, vision bool) llm.Model {
	opts := []openai.Option{openai.WithModel(model), openai.WithBaseURL(baseURL)}
	if grammar {
		opts = append(opts, openai.WithToolGrammar())
	}
	if vision {
		opts = append(opts, openai.WithVision())
	}
	return openai.New(secret.Text{}, opts...)
}

// ProvidersFile is the name of the providers file inside the data directory.
const ProvidersFile = "providers.json"

// Endpoint is a server the operator named once in the providers file. It is selected
// as --model <name>:<model>, so a remote or non-default server needs no *_BASE_URL
// variable.
type Endpoint struct {
	// Kind is the wire behaviour: openai (any OpenAI-compatible server, tools in the
	// tools field), or llamacpp, ollama, lmstudio.
	Kind string `json:"kind"`
	// BaseURL is the server's API root, such as http://gpu-box:11434/v1.
	BaseURL string `json:"base_url"`
	// Key names the secret that holds the server's API key, resolved through the same
	// vault-then-environment chain as a built-in provider's key. Empty means keyless.
	Key string `json:"key,omitempty"`
	// Vision marks the served model as able to read images.
	Vision bool `json:"vision,omitempty"`
}

// Endpoints is the parsed providers file.
type Endpoints struct {
	// AllowHosts are the non-loopback hosts a keyless endpoint may point at. A keyless
	// endpoint sends the conversation to its host unauthenticated, so a host the
	// operator did not list is refused rather than assumed to be theirs.
	AllowHosts []string            `json:"allow_hosts,omitempty"`
	Providers  map[string]Endpoint `json:"providers"`
}

var endpointName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// LoadEndpoints reads the providers file from dir. A missing file is no endpoints,
// not an error. A name that shadows a built-in provider is refused: an entry called
// openai with another base URL would receive the stored OpenAI key.
func LoadEndpoints(dir string) (Endpoints, error) {
	path := filepath.Join(dir, ProvidersFile)
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed filename under the operator-chosen data dir
	if errors.Is(err, os.ErrNotExist) {
		return Endpoints{}, nil
	}
	if err != nil {
		return Endpoints{}, fmt.Errorf("provider: read %s: %w", path, err)
	}
	return parseEndpoints(raw, path)
}

func parseEndpoints(raw []byte, path string) (Endpoints, error) {
	var eps Endpoints
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&eps); err != nil {
		return Endpoints{}, fmt.Errorf("provider: parse %s: %w", path, err)
	}
	for name, ep := range eps.Providers {
		if err := validateEndpoint(name, ep); err != nil {
			return Endpoints{}, fmt.Errorf("provider: %s: %w", path, err)
		}
	}
	return eps, nil
}

func validateEndpoint(name string, ep Endpoint) error {
	if !endpointName.MatchString(name) {
		return fmt.Errorf("provider name %q must be lowercase letters, digits, - or _, starting with a letter", name)
	}
	for _, p := range Providers() {
		if p == name {
			return fmt.Errorf("provider name %q is a built-in provider; choose another name", name)
		}
	}
	if ep.Kind != "openai" {
		if _, ok := localKindNamed(ep.Kind); !ok {
			return fmt.Errorf("provider %q: kind %q is not one of openai, llamacpp, ollama, lmstudio", name, ep.Kind)
		}
	}
	if ep.BaseURL == "" {
		return fmt.Errorf("provider %q: base_url is required", name)
	}
	if u, err := url.Parse(ep.BaseURL); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("provider %q: base_url %q must be an http or https URL", name, ep.BaseURL)
	}
	return nil
}

// endpointCarrier is implemented by the Source WithEndpoints returns.
type endpointCarrier interface{ endpoints() Endpoints }

type endpointSource struct {
	secret.Source
	eps Endpoints
}

func (s endpointSource) endpoints() Endpoints { return s.eps }

// WithEndpoints returns a Source that resolves exactly as src does and also carries
// the named endpoints, so ResolveWith can select one by name without a new
// parameter on it.
func WithEndpoints(src secret.Source, eps Endpoints) secret.Source {
	return endpointSource{Source: src, eps: eps}
}

func endpointsOf(src secret.Source) Endpoints {
	if c, ok := src.(endpointCarrier); ok {
		return c.endpoints()
	}
	return Endpoints{}
}

// EndpointNames lists the endpoint names a Source carries, sorted.
func EndpointNames(src secret.Source) []string {
	eps := endpointsOf(src)
	names := make([]string, 0, len(eps.Providers))
	for n := range eps.Providers {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// resolveEndpoint builds the client for a configured endpoint.
func resolveEndpoint(ctx context.Context, src secret.Source, eps Endpoints, name, model string) (llm.Model, error) {
	ep := eps.Providers[name]
	if ep.Key == "" {
		if err := checkKeylessHost(ep.BaseURL, eps.AllowHosts); err != nil {
			return nil, fmt.Errorf("provider %q: %w", name, err)
		}
	} else if !llm.SafeBaseURL(ep.BaseURL) {
		return nil, fmt.Errorf("provider %q: base_url must be https (or http to localhost); refusing to send the API key over an unencrypted transport", name)
	}
	if ep.Key != "" {
		key, err := src.Lookup(ctx, ep.Key)
		if errors.Is(err, secret.ErrNotFound) {
			return nil, fmt.Errorf("%w (%s)", ErrCredentialNotSet, ep.Key)
		}
		if err != nil {
			return nil, fmt.Errorf("provider: resolve %s: %w", ep.Key, err)
		}
		opts := []openai.Option{openai.WithModel(model), openai.WithBaseURL(ep.BaseURL)}
		if ep.Vision {
			opts = append(opts, openai.WithVision())
		}
		return openai.New(key, opts...), nil
	}
	grammar := false
	if k, ok := localKindNamed(ep.Kind); ok {
		grammar = k.grammar
	}
	return newLocalClient(model, ep.BaseURL, grammar, ep.Vision), nil
}

// checkKeylessHost refuses a keyless endpoint whose host is neither loopback nor
// listed in allow.
func checkKeylessHost(baseURL string, allow []string) error {
	u, err := url.Parse(baseURL)
	if err != nil {
		return err
	}
	host := u.Hostname()
	if host == "localhost" {
		return nil
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		return nil
	}
	for _, a := range allow {
		if strings.EqualFold(a, host) {
			return nil
		}
	}
	return fmt.Errorf("%s is not a loopback host and the server needs no key, so its conversation would leave this machine unauthenticated; add %q to allow_hosts in %s to allow it", host, host, ProvidersFile)
}

// BaseURLEnvVars lists every environment variable (or vault reference) that moves a
// built-in provider's base URL, so a help text and the README can name them all
// without a hand-kept copy drifting from what [ResolveWith] reads.
func BaseURLEnvVars() []string {
	vars := []string{"ANTHROPIC_BASE_URL", "OPENAI_BASE_URL", "DEEPSEEK_BASE_URL", "GEMINI_BASE_URL"}
	for _, k := range localKinds {
		vars = append(vars, k.baseRef)
	}
	return vars
}

// VisionEnvVars lists the variables that turn image input on for a local server,
// which is off by default because a local server can serve a model that cannot see.
func VisionEnvVars() []string {
	vars := make([]string, 0, len(localKinds))
	for _, k := range localKinds {
		vars = append(vars, k.visionRef)
	}
	return vars
}
