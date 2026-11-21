package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"

	"github.com/ionalpha/flynn/governed"

	"github.com/ionalpha/flynn/archetypes/review"
	budgetpkg "github.com/ionalpha/flynn/budget"
	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/internal/credential"
	"github.com/ionalpha/flynn/internal/vault"
	"github.com/ionalpha/flynn/netguard"
	"github.com/ionalpha/flynn/secret"
	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/tools/github"
)

// errChangesRequested marks a successful review whose verdict was REQUEST_CHANGES, so
// main exits non-zero and a CI step can gate a merge on it.
var errChangesRequested = errors.New("review requested changes")

// exitChangesRequested is distinct from 1 (command failed) and 2 (usage), so a pipeline
// can tell a refusal from a broken reviewer.
const exitChangesRequested = 3

// GitHub credential references, resolved vault first and then environment like the
// model keys, so a stored value wins and a CI job's ambient environment still works.
const (
	refToken           = "GITHUB_TOKEN" //nolint:gosec // G101: a credential reference, not a credential
	refAppIssuer       = "FLYNN_GITHUB_APP_ISSUER"
	refAppInstallation = "FLYNN_GITHUB_APP_INSTALLATION"
	refAppKey          = "FLYNN_GITHUB_APP_KEY"      // the App private key, PEM content
	refAppKeyFile      = "FLYNN_GITHUB_APP_KEY_FILE" // a path to the PEM, for hosts that mount key files
)

// githubIntegration is the integration whose stored credentials (`flynn auth add
// github`) authenticate a review when no App is configured.
const githubIntegration = "github"

// envAPIBase names the REST API root. It is not a credential, so it comes from the
// environment. GitHub Actions sets it in every job, to github.com's root or the
// Enterprise appliance's, so a workflow that sets nothing reviews the API it runs under.
const envAPIBase = "GITHUB_API_URL"

// reviewFlags is the pull request to review, the authority granted, and the run's
// budgets.
type reviewFlags struct {
	owner, repo string
	number      int
	approve     bool
	selfLogin   string
	maxLines    int
	oversize    bool
	maxCost     float64
	maxTokens   int64
	credRef     string
	apiBase     string
}

// parseReviewFlags refuses an APPROVE grant without a self-identity, because approval
// that cannot recognise its own pull request cannot refuse self-approval.
func parseReviewFlags(args []string) (reviewFlags, error) {
	fs := flag.NewFlagSet("review", flag.ContinueOnError)
	var (
		repo      = fs.String("repo", "", "repository as owner/name; unneeded when the pull request is given as a URL")
		approve   = fs.Bool("approve", false, "permit an APPROVE verdict; off by default so installing the reviewer never silently adds a merge gate. Requires --as.")
		selfLogin = fs.String("as", "", "the reviewer's own login (for example my-bot[bot]); an APPROVE on its own pull request is refused")
		maxLines  = fs.Int("max-changed-lines", 0, "review budget: refuse a pull request whose changed lines exceed this; 0 selects the default, negative disables")
		oversize  = fs.Bool("oversize", false, "review a pull request that exceeds the budget anyway (approval stays refused on a truncated diff)")
		maxCost   = fs.Float64("max-cost", 0, "cap the review's total model spend; 0 is unlimited")
		maxTokens = fs.Int64("max-tokens", 0, "cap the review's total metered tokens; 0 is unlimited")
		credRef   = fs.String("credential", "", "the stored github credential to authenticate with (github/<name>); default is the integration's default credential")
		apiBase   = fs.String("api-base", os.Getenv(envAPIBase), "the GitHub REST API root; a GitHub Enterprise host points this at its own API. Defaults to $"+envAPIBase+", else "+github.DefaultAPIBase)
	)
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: flynn review <pull-request> [flags]")
		fmt.Fprintln(os.Stderr, "  <pull-request> is a URL (https://github.com/OWNER/REPO/pull/N) or OWNER/REPO#N")
		fmt.Fprintln(os.Stderr, "  auth: a GitHub App ("+refAppIssuer+" + "+refAppInstallation+" + "+refAppKey+" or "+refAppKeyFile+"),")
		fmt.Fprintln(os.Stderr, "        else a stored credential (flynn auth add github), else "+refToken+";")
		fmt.Fprintln(os.Stderr, "        every reference resolves vault first, environment second")
		fmt.Fprintln(os.Stderr, "  host: "+github.DefaultAPIBase+" unless --api-base or $"+envAPIBase+" names another")
		fmt.Fprintf(os.Stderr, "  exit: 0 clean, %d changes requested, 1 failure\n", exitChangesRequested)
		fs.PrintDefaults()
	}
	// The flag package stops at the first non-flag, so "<pull-request> --approve" would
	// leave --approve unparsed. Positionals are collected and parsing continues after them.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return reviewFlags{}, err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fs.Usage()
		return reviewFlags{}, errors.New("expected exactly one pull request")
	}

	owner, repoName, number, err := parsePRRef(positional[0], *repo)
	if err != nil {
		return reviewFlags{}, err
	}
	if *approve && *selfLogin == "" {
		return reviewFlags{}, errors.New("--approve requires --as <reviewer-login>: approval without a self-identity cannot refuse self-approval")
	}
	return reviewFlags{
		owner: owner, repo: repoName, number: number,
		approve: *approve, selfLogin: *selfLogin,
		maxLines: *maxLines, oversize: *oversize,
		maxCost: *maxCost, maxTokens: *maxTokens,
		credRef: *credRef, apiBase: *apiBase,
	}, nil
}

// runReview reviews one pull request with the reviewer archetype, which supplies the
// instruction and the exact authority (package review), and maps the submitted verdict
// to an exit code.
func runReview(args []string, modelSpec, dataDir string, verbose bool) error {
	fl, err := parseReviewFlags(args)
	if err != nil {
		return err
	}

	// Governance rides Flynn's own dispatch waist, and a review verdict cannot rest on an
	// external harness's unobserved reasoning.
	if name, _, ok := externalAgentSpec(modelSpec); ok {
		return fmt.Errorf("review runs the native loop; the %s external agent backend is not supported here", name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, plan, choice, err := resolveModelOrOnboard(ctx, modelSpec, currentModelOrigin(), dataDir)
	if err != nil {
		return err
	}

	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	reg, err := governed.Registry()
	if err != nil {
		return err
	}

	// Vault before environment, plus the github integration's stored credentials. The raw
	// token variable is removed from the process environment either way.
	creds := credential.NewStore(store.Resources(reg))
	cfg, err := resolveReviewCredentials(ctx, credentialSource(dataDir),
		storedGitHubToken(creds, vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), fl.credRef), os.ReadFile)
	_ = os.Unsetenv(refToken)
	if err != nil {
		return err
	}
	cfg.Owner, cfg.Repo, cfg.Number = fl.owner, fl.repo, fl.number
	cfg.SelfLogin = fl.selfLogin
	cfg.AllowApprove = fl.approve
	cfg.MaxChangedLines = fl.maxLines
	cfg.ReviewOversize = fl.oversize
	if err := configureAPIBase(ctx, &cfg, fl.apiBase, resolveHost); err != nil {
		return err
	}

	log := store.Log()
	var rec *chain.RecordingLog
	if signer != nil {
		rec = chain.NewRecordingLog(log, nil)
		log = rec
	}

	toolset, err := review.Tools(cfg)
	if err != nil {
		return err
	}

	verdict := newVerdictTracker()
	objective := fmt.Sprintf("Review pull request #%d.", fl.number)
	_, source, _, err := driveRequest{
		out:       os.Stdout,
		model:     model,
		plan:      plan,
		objective: objective,
		system:    review.SystemPrompt,
		rstore:    store.Resources(reg),
		jq:        store.Jobs(),
		log:       log,
		verbose:   verbose,
	}.withOptions(
		withModelChoice(choice),
		withToolset(&boundToolset{tools: toolset, grant: review.Grant()}),
		withEventObserver(verdict.observe),
		withBudget(budgetpkg.Limits{Tokens: fl.maxTokens, Cost: fl.maxCost}),
	).execute(ctx)
	if err != nil {
		// A stalled or over-budget review is the one a maintainer asks about, so it is sealed
		// with its ending before the error returns.
		if rec != nil && source != "" {
			sealStopped(ctx, os.Stdout, store, rec, source, signer, err)
		}
		return err
	}

	// Sealed before the outcome is acted on, so the reported verdict is verifiable from the
	// record alone.
	if rec != nil {
		sealReported(ctx, os.Stdout, store, rec, source, signer)
	}

	return verdict.outcome(os.Stdout)
}

// verdictTracker reads the verdict from the run's events (the github_submit_review call
// whose result did not error), so the exit follows what the sealed record shows.
type verdictTracker struct {
	pending map[string]string // tool-use id -> requested verdict
	final   string
}

func newVerdictTracker() *verdictTracker {
	return &verdictTracker{pending: make(map[string]string)}
}

// observe pairs a submit call with its non-error result. A refused submission (an
// ungated APPROVE, a truncated diff) confirms nothing, so a later success counts.
func (v *verdictTracker) observe(ev session.Event) {
	switch ev.Kind {
	case session.KindToolCall:
		if ev.Tool != "github_submit_review" {
			return
		}
		var in struct {
			Event string `json:"event"`
		}
		if err := json.Unmarshal(ev.Input, &in); err != nil {
			return
		}
		v.pending[ev.ToolUseID] = strings.ToUpper(strings.TrimSpace(in.Event))
	case session.KindToolResult:
		if want, ok := v.pending[ev.ToolUseID]; ok && !ev.IsError {
			v.final = want
		}
	default:
	}
}

// outcome returns nil for a clean verdict, errChangesRequested for a blocking one, and
// an error when the run converged without submitting a review, which must not read as a
// pass.
func (v *verdictTracker) outcome(out io.Writer) error {
	switch v.final {
	case "":
		return errors.New("the run submitted no review verdict")
	case "REQUEST_CHANGES":
		_, _ = fmt.Fprintln(out, "  verdict: changes requested")
		return errChangesRequested
	default:
		_, _ = fmt.Fprintf(out, "  verdict: %s\n", strings.ToLower(v.final))
		return nil
	}
}

// parsePRRef accepts https://github.com/OWNER/REPO/pull/N or OWNER/REPO#N, with --repo
// supplying OWNER/REPO for a bare #N.
func parsePRRef(arg, repoFlag string) (owner, repo string, number int, err error) {
	var coords, numPart string
	switch {
	case strings.Contains(arg, "://"):
		trimmed := strings.TrimPrefix(arg, "https://")
		trimmed = strings.TrimPrefix(trimmed, "http://")
		parts := strings.Split(strings.TrimSuffix(trimmed, "/"), "/")
		// host/OWNER/REPO/pull/N
		if len(parts) != 5 || parts[3] != "pull" {
			return "", "", 0, fmt.Errorf("unrecognised pull request URL %q", arg)
		}
		coords = parts[1] + "/" + parts[2]
		numPart = parts[4]
	case strings.Contains(arg, "#"):
		var ok bool
		coords, numPart, ok = strings.Cut(arg, "#")
		if !ok || coords == "" {
			return "", "", 0, fmt.Errorf("unrecognised pull request %q; want OWNER/REPO#N", arg)
		}
	default:
		coords = repoFlag
		numPart = arg
	}
	if coords == "" {
		return "", "", 0, fmt.Errorf("no repository for pull request %q; pass a URL, OWNER/REPO#N, or --repo", arg)
	}
	owner, repo, ok := strings.Cut(coords, "/")
	if !ok || owner == "" || repo == "" || strings.Contains(repo, "/") {
		return "", "", 0, fmt.Errorf("repository %q is not owner/name", coords)
	}
	number, err = strconv.Atoi(numPart)
	if err != nil || number <= 0 {
		return "", "", 0, fmt.Errorf("pull request number %q is not a positive integer", numPart)
	}
	return owner, repo, number, nil
}

// hostResolver maps a host name to its addresses; tests supply one to fix the answer.
type hostResolver func(ctx context.Context, host string) ([]netip.Addr, error)

// resolveHost parses an address literal itself, so the resolver need not be trusted to
// pass it through.
func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// configureAPIBase points the toolset at a non-github.com API root and widens egress
// just enough to reach it.
//
// The default public-only policy refuses private, loopback and metadata addresses, but
// a GitHub Enterprise appliance often answers on an internal one. An empty base or
// github.com's keeps the default.
//
// The widened policy permits exactly the addresses the host resolves to, so naming an
// appliance opens neither the rest of the private network nor the metadata endpoint.
// The client pins pagination to the API origin, so a response cannot redirect the
// credential.
func configureAPIBase(ctx context.Context, cfg *github.Config, base string, resolve hostResolver) error {
	base = strings.TrimSuffix(strings.TrimSpace(base), "/")
	if base == "" || base == github.DefaultAPIBase {
		return nil
	}
	policy, err := apiEgressPolicy(ctx, base, resolve)
	if err != nil {
		return err
	}
	cfg.APIBase = base
	cfg.HTTPClient = netguard.Client(policy)
	return nil
}

// apiEgressPolicy builds the policy for base and refuses a base that would expose the
// credential.
//
// Every API request carries the token, so plain http is allowed only when every
// resolved address is loopback. Anywhere else it puts the token on the wire in the
// clear, and an internal network is not a trusted one.
func apiEgressPolicy(ctx context.Context, base string, resolve hostResolver) (netguard.Policy, error) {
	u, err := url.Parse(base)
	if err != nil {
		return netguard.Policy{}, fmt.Errorf("api base %q is not a URL: %w", base, err)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return netguard.Policy{}, fmt.Errorf("api base %q must be an http or https URL", base)
	}
	host := u.Hostname()
	if host == "" {
		return netguard.Policy{}, fmt.Errorf("api base %q names no host", base)
	}
	addrs, err := resolve(ctx, host)
	if err != nil {
		return netguard.Policy{}, fmt.Errorf("resolve api base host %q: %w", host, err)
	}
	if len(addrs) == 0 {
		return netguard.Policy{}, fmt.Errorf("api base host %q resolves to no address", host)
	}

	policy := netguard.Policy{Allow: make([]netip.Prefix, 0, len(addrs))}
	for _, addr := range addrs {
		addr = addr.Unmap()
		if u.Scheme == "http" && !addr.IsLoopback() {
			return netguard.Policy{}, fmt.Errorf(
				"api base %q is http and %q is not loopback: a GitHub credential would cross the network in the clear; use https",
				base, addr,
			)
		}
		policy.Allow = append(policy.Allow, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return policy, nil
}

// tokenLookup returns secret.ErrNotFound when no token is stored, so the caller tries
// the next source.
type tokenLookup func(ctx context.Context) (secret.Text, error)

// storedGitHubToken resolves the credential named by ref ("github/<name>"), or the
// integration default when ref is empty. A missing named credential is an error, since
// the operator asked for it; a missing default is ErrNotFound, falling through to the
// environment.
func storedGitHubToken(creds *credential.Store, vaultStore *vault.Store, ref string) tokenLookup {
	return func(ctx context.Context) (secret.Text, error) {
		var (
			cred credential.Credential
			err  error
		)
		if ref != "" {
			cred, err = creds.Resolve(ctx, ref)
			if err != nil {
				return secret.Text{}, fmt.Errorf("credential %q: %w", ref, err)
			}
		} else {
			cred, err = creds.Default(ctx, githubIntegration)
			if errors.Is(err, credential.ErrNotFound) {
				return secret.Text{}, secret.ErrNotFound
			}
			if err != nil {
				return secret.Text{}, err
			}
		}
		return vaultStore.Lookup(ctx, cred.Ref())
	}
}

// resolveReviewCredentials picks the strongest identity first, vault before environment
// at each step:
//
//  1. A GitHub App: issuer, installation id and private key (PEM content or a file
//     path), each resolved through src. Formal approval wants this identity. A partial
//     App configuration is an error, so the operator never gets a silent downgrade.
//  2. A stored github credential (`flynn auth add github`), via stored.
//  3. GITHUB_TOKEN through src (vault entry, else a CI workflow's environment).
//
// Every value is a secret.Text from the moment it is read, so none reaches a log or a
// crash dump.
func resolveReviewCredentials(ctx context.Context, src secret.Source, stored tokenLookup, readFile func(string) ([]byte, error)) (github.Config, error) {
	var cfg github.Config

	lookup := func(ref string) (secret.Text, error) {
		v, err := src.Lookup(ctx, ref)
		if errors.Is(err, secret.ErrNotFound) {
			return secret.Text{}, nil
		}
		return v, err
	}
	issuer, err := lookup(refAppIssuer)
	if err != nil {
		return cfg, err
	}
	installation, err := lookup(refAppInstallation)
	if err != nil {
		return cfg, err
	}
	keyPEM, err := lookup(refAppKey)
	if err != nil {
		return cfg, err
	}
	keyFile, err := lookup(refAppKeyFile)
	if err != nil {
		return cfg, err
	}
	if keyPEM.Empty() && !keyFile.Empty() {
		pemBytes, rerr := readFile(keyFile.Expose())
		if rerr != nil {
			return cfg, fmt.Errorf("read App private key: %w", rerr)
		}
		keyPEM = secret.New(string(pemBytes))
	}

	if !issuer.Empty() || !installation.Empty() || !keyPEM.Empty() {
		if issuer.Empty() || installation.Empty() || keyPEM.Empty() {
			return cfg, fmt.Errorf("app auth needs all of %s, %s, and %s (or %s)",
				refAppIssuer, refAppInstallation, refAppKey, refAppKeyFile)
		}
		id, perr := strconv.ParseInt(installation.Expose(), 10, 64)
		if perr != nil {
			return cfg, fmt.Errorf("%s is not an installation id", refAppInstallation)
		}
		key, kerr := github.ParsePrivateKey([]byte(keyPEM.Expose()))
		if kerr != nil {
			return cfg, fmt.Errorf("parse App private key: %w", kerr)
		}
		cfg.App = github.App{Issuer: issuer.Expose(), InstallationID: id, PrivateKey: key}
		return cfg, nil
	}

	token, err := stored(ctx)
	switch {
	case err == nil:
		cfg.Token = token
		return cfg, nil
	case !errors.Is(err, secret.ErrNotFound):
		return cfg, err
	}

	token, err = src.Lookup(ctx, refToken)
	switch {
	case err == nil:
		cfg.Token = token
		return cfg, nil
	case errors.Is(err, secret.ErrNotFound):
		return cfg, fmt.Errorf("no GitHub credentials: configure a GitHub App (%s...), store a token (flynn auth add github), or set %s",
			refAppIssuer, refToken)
	default:
		return cfg, err
	}
}
