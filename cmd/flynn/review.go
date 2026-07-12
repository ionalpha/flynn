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

// errChangesRequested is returned by runReview when the reviewer submitted a
// REQUEST_CHANGES verdict. The review itself succeeded; the distinct error lets
// main map the verdict to a non-zero exit so a CI step can gate a merge on it.
var errChangesRequested = errors.New("review requested changes")

// exitChangesRequested is the exit code for a completed review whose verdict was
// REQUEST_CHANGES. It is distinct from 1 (the command failed) and 2 (usage), so a
// pipeline can tell "the reviewer said no" from "the reviewer broke".
const exitChangesRequested = 3

// The references the review command resolves its GitHub credentials by. Each is
// looked up through the same vault-then-environment chain the model keys use, so
// a value stored once (`flynn auth`) wins over an exported variable, and a CI
// workflow with only its ambient environment still works.
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

// envAPIBase names the REST API root to review against. It is not a credential, so
// it is read from the environment rather than the vault. GitHub Actions exports it
// into every job, holding github.com's API root on the hosted service and the
// appliance's own root on GitHub Enterprise, so a workflow that sets nothing still
// reviews against the API it is running under.
const envAPIBase = "GITHUB_API_URL"

// runReview reviews one pull request with the reviewer archetype and submits a
// formal verdict. The archetype supplies the standing instruction and the exact
// authority (package review); this command only assembles: parse the target, build
// the repository-bound toolset from the environment's credentials, bind the grant,
// drive the loop, and map the submitted verdict to an exit code.
func runReview(args []string, modelSpec, dataDir string, verbose bool) error {
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
	// The flag package stops parsing at the first argument that is not a flag, so a
	// single Parse of "<pull-request> --approve" would take --approve for a second
	// pull request. The usage line puts the pull request first because that is how the
	// command is written, so collect positionals and keep parsing what follows them.
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		args = fs.Args()[1:]
	}
	if len(positional) != 1 {
		fs.Usage()
		return errors.New("expected exactly one pull request")
	}

	owner, repoName, number, err := parsePRRef(positional[0], *repo)
	if err != nil {
		return err
	}
	if *approve && *selfLogin == "" {
		return errors.New("--approve requires --as <reviewer-login>: approval without a self-identity cannot refuse self-approval")
	}

	// The reviewer's loop is Flynn's own: governance rides the dispatch waist, and
	// an external harness's inner reasoning is unobserved, which a review verdict
	// cannot afford.
	if name, _, ok := externalAgentSpec(modelSpec); ok {
		return fmt.Errorf("review runs the native loop; the %s external agent backend is not supported here", name)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	model, plan, _, err := resolveModelOrOnboard(ctx, modelSpec, modelSpecExplicit, dataDir)
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

	reg, err := missionRegistry()
	if err != nil {
		return err
	}

	// Credentials resolve vault first, environment second: the chain model keys
	// already use, plus the github integration's stored credentials. The raw token
	// variable is dropped from the process environment either way.
	creds := credential.NewStore(store.Resources(reg))
	cfg, err := resolveReviewCredentials(ctx, credentialSource(dataDir),
		storedGitHubToken(creds, vault.New(dataDir, vault.WithPassphrase(terminalPassphrase)), *credRef), os.ReadFile)
	_ = os.Unsetenv(refToken)
	if err != nil {
		return err
	}
	cfg.Owner, cfg.Repo, cfg.Number = owner, repoName, number
	cfg.SelfLogin = *selfLogin
	cfg.AllowApprove = *approve
	cfg.MaxChangedLines = *maxLines
	cfg.ReviewOversize = *oversize
	if err := configureAPIBase(ctx, &cfg, *apiBase, resolveHost); err != nil {
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
	objective := fmt.Sprintf("Review pull request #%d.", number)
	_, source, _, err := drive(
		ctx, os.Stdout, model, plan, "", objective, review.SystemPrompt,
		store.Resources(reg), store.Jobs(), log, verbose, "", nil,
		withToolset(&boundToolset{tools: toolset, grant: review.Grant()}),
		withEventObserver(verdict.observe),
		withBudget(budgetpkg.Limits{Tokens: *maxTokens, Cost: *maxCost}),
	)
	if err != nil {
		return err
	}

	// Seal the run before acting on its outcome, so the verdict the command reports
	// is one a third party can verify from the record alone.
	if rec != nil {
		if serr := sealRun(ctx, store, rec, source, signer); serr != nil {
			_, _ = fmt.Fprintf(os.Stdout, "  (run not sealed: %v)\n", serr)
		} else {
			_, _ = fmt.Fprintf(os.Stdout, "  run sealed; verify with: flynn spine verify %s\n", source)
		}
	}

	return verdict.outcome(os.Stdout)
}

// verdictTracker reads the submitted review verdict off the run's own event
// stream: the github_submit_review tool call whose result did not error. Reading
// the recorded events, rather than a side channel, means the verdict the command
// exits on is the one the sealed record shows.
type verdictTracker struct {
	pending map[string]string // tool-use id -> requested verdict
	final   string
}

func newVerdictTracker() *verdictTracker {
	return &verdictTracker{pending: make(map[string]string)}
}

// observe watches the session stream for a submitted review. A call registers the
// verdict it asks for; the matching non-error result confirms it. A refused
// submission (an ungated APPROVE, a truncated diff) errors and confirms nothing,
// so a later successful submission is the one that counts.
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

// outcome reports the review's verdict and maps it to the command's result: nil
// for a clean verdict, errChangesRequested for a blocking one, and an error when
// the run converged without submitting any review, which is a reviewer that did
// not do its job and must not read as a pass.
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

// parsePRRef resolves the command's pull-request argument: a full URL
// (https://github.com/OWNER/REPO/pull/N) or OWNER/REPO#N, with --repo allowed to
// carry the OWNER/REPO half when the argument is only #N.
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

// hostResolver maps a host name to the addresses it answers on. resolveHost is the
// real one; a test supplies its own to fix what a name resolves to.
type hostResolver func(ctx context.Context, host string) ([]netip.Addr, error)

// resolveHost resolves a host name, or parses it when it is already an address
// literal, which the resolver would otherwise have to be trusted to pass through.
func resolveHost(ctx context.Context, host string) ([]netip.Addr, error) {
	if addr, err := netip.ParseAddr(host); err == nil {
		return []netip.Addr{addr}, nil
	}
	return net.DefaultResolver.LookupNetIP(ctx, "ip", host)
}

// configureAPIBase points the toolset at an API root other than github.com's, and
// widens the egress policy just far enough to reach it.
//
// The default toolset dials through a public-only policy, which refuses private,
// loopback, and cloud-metadata addresses. That is the right default against a public
// API and the wrong one against a GitHub Enterprise appliance, which commonly answers
// on an internal address the policy would refuse. An empty base, or github.com's own,
// changes nothing and keeps that default.
//
// A configured base widens the policy by address rather than by class: the policy
// permits exactly the addresses this host resolves to, and nothing else, so naming an
// internal appliance does not also open the run to the rest of the private network or
// to the metadata endpoint. Pagination is separately pinned to the API origin by the
// client, so a response cannot redirect the credential elsewhere.
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

// apiEgressPolicy builds the outbound policy for reaching base, and refuses a base
// that would carry the credential unsafely.
//
// Every request to the API root attaches the reviewer's token, so the scheme is not a
// preference. Plain http is permitted only when every address the host resolves to is
// loopback, where the request never leaves the machine; against any other address it
// would put the token on the wire in the clear, and an internal network is not a
// trusted one.
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

// tokenLookup resolves a stored GitHub token, returning secret.ErrNotFound when
// none is configured so the caller can continue down the chain.
type tokenLookup func(ctx context.Context) (secret.Text, error)

// storedGitHubToken resolves the github integration's stored credential: the one
// named by ref ("github/<name>"), or the integration's default when ref is empty.
// A named reference that does not exist is an error, because the operator asked
// for that credential specifically; an absent default is ErrNotFound, because
// falling through to the environment is then the intended path.
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

// resolveReviewCredentials builds the toolset's credentials, strongest identity
// first and vault before environment at every step:
//
//  1. A GitHub App: issuer + installation id + private key (PEM content, or a
//     file path), each reference resolved through src. The App is the identity a
//     formal approval wants, so when it is fully configured it wins. A partial
//     App configuration is an error rather than a silent fall-through: the
//     operator set it up and would not want a quiet downgrade to a weaker path.
//  2. A stored github credential (`flynn auth add github`), via stored.
//  3. The GITHUB_TOKEN reference through src (vault entry, else the ambient
//     environment variable a CI workflow provides).
//
// Every value is a secret.Text from the moment it is read, so none survives to a
// log or a crash dump.
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
