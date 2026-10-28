// Package localmodel runs a catalog model on this machine behind the guards the flynn
// binary applies, so a host that embeds the agent does not have to rebuild them. A host
// that assembled the same parts itself (a model server started from a download) would
// have no advisory gate, no digest pinning and no sandbox, and each of those is a way to
// run attacker-chosen code next to the agent.
//
// A [Host] walks one model from reference to running endpoint: [Host.Classify] decides how
// far a source is trusted and whether this machine isolates it enough, [Host.EnsureRuntime]
// installs a runtime build after the advisory gate and against a pinned digest,
// [Host.EnsureWeights] fetches weights against the catalog digest, [Host.Serve] starts the
// server inside the sandbox and returns an [Endpoint], and [Host.Status], [Host.Reach],
// [Host.Stats] and [Host.Stop] observe and end it. [Host.Models], [Host.Fit] and
// [Host.Hardware] answer what the catalog offers and what the machine can run.
//
// # Guarantees
//
// Each guarantee holds for every exported path, and none has a switch to turn it off:
//
//   - The server binds 127.0.0.1 only. No [Config] field or argument names an address,
//     and the port is chosen from the loopback interface.
//   - A runtime build below its advisory floor is refused before any network access, on a
//     first install and on a cached one, and a running server built below it is stopped
//     instead of adopted. [Host.RaiseFloor] can only raise a floor. A host cannot name
//     the runtime build or its version, so the version the gate reads is the one this
//     module pins.
//   - The prompt contract is the catalog's chat template. A template embedded in the
//     weights is read with a hardened parser, reported through [Endpoint.TemplateOverridden],
//     and replaced.
//   - The runtime runs in the sandbox the [Host] built. [Host.Serve] refuses to start it
//     unless the sandbox applies kernel confinement, or [Config.MinContainment] records
//     that the host accepts a process jail, and [Endpoint.Containment] reports the level
//     the server got. A source whose trust needs stronger isolation than the sandbox
//     provides is refused.
//   - Weights are fetched against a catalog digest and refused without one. The digest is
//     checked again just before launch, and the runtime binary is checked against the
//     hash recorded when it was installed, so a file swapped after provisioning is not
//     served.
//   - The endpoint demands a per-launch bearer token ([Endpoint.Token]). The token reaches
//     the runtime through a file only the owning account can read, never the command line.
//     Before the token is sent anywhere, the listener on the chosen port must prove it is
//     the server this call started: it refuses a request with no token, refuses a made-up
//     one, and accepts a second key that only the key file holds. A server that fails any
//     of these is stopped, and nothing it answered carried the token.
//   - Errors do not carry tokens, and URLs in them are reduced to scheme and host.
//
// # Limits
//
// The sandbox leaves outbound network access open to a served runtime (see
// [Host.Serve]); the loopback pin and the token are what keep the endpoint private. Calls
// for one model are serialized within a [Host]; two processes starting the same model at
// once are not, and the later one finds a server it did not start and stops it. On a
// platform without owner-only file permissions (Windows) the token file relies on the
// data directory's own access control. The runtime reads the token from a file whose path
// is on its command line; it is not handed over an inherited descriptor, because the
// sandbox cannot yet pass one. The runtime opens the weights by path, so the digest is
// checked on a descriptor before the header is parsed and again just before launch, but
// the window between that last check and the runtime's own open stays: the file is
// read-only then, so only the owning account can change it. A runtime that outlives a
// crashed host is not reaped. The handshake that attributes the listener relies on the
// runtime accepting several keys from one key file and answering an empty chat request
// with a status other than 401 or 403 when the key is valid; a build that behaves
// otherwise is refused, not trusted.
//
// This package is experimental: it may change before it is listed in the stable tier.
package localmodel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/fetch"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/internal/inference/provision"
	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
)

// Runtime is the name of the inference runtime a [Host] provisions and serves.
const Runtime = "llama.cpp"

// Model is a catalog entry: identity, provenance, license, size, capabilities and the
// quantizations that can be downloaded, each with the digest its bytes are checked
// against. It is the catalog schema, so a host reads the fields it needs from it.
type Model = catalog.ModelSpec

// Quant is one downloadable quantization of a [Model], with the digest its bytes are
// checked against. A host that supplies its own [Config.Catalog] builds these.
type Quant = catalog.Quant

// QuantFile is one file of a multi-file [Quant], with its URL and digest.
type QuantFile = catalog.QuantFile

// Source records where a [Model] came from.
type Source = catalog.Source

// Capabilities is what a [Model] can do.
type Capabilities = catalog.Capabilities

// Kind says whether a [Model] is a hosted API or local weights.
type Kind = catalog.Kind

// Trust is how far a [Model] entry has been vetted.
type Trust = catalog.Trust

// Format is a weight file's encoding. Pickle is refused by every [Host] method.
type Format = catalog.Format

// Config says where a [Host] keeps its files and, for a host that curates its own
// catalog, what it may serve. The zero values of the optional fields give the catalog and
// runtime pins that ship with this module.
type Config struct {
	// DataDir is the absolute directory holding downloaded runtimes, weights, the run
	// registry and the per-launch token files. It is created owner-only. Required.
	DataDir string
	// Catalog replaces the embedded catalog when non-empty. Every entry is validated, so
	// a local model without a known chat template is rejected at [Open].
	Catalog []Model
	// MinContainment is the weakest isolation the host accepts for a model server:
	// "kernel" (the default) requires kernel-enforced confinement of the runtime, and
	// "process" accepts a process jail on a machine that cannot provide it. The runtime
	// parses untrusted weights, so a host that has not said otherwise gets a refusal.
	// Under "process" every server is reported and recorded as a process jail, even where
	// the kernel did confine it, because the sandbox does not say which launches fell back.
	// [Endpoint.Containment] reports the level of every launch.
	MinContainment string
	// HTTPClient carries every download. Nil uses a client that refuses private and
	// loopback addresses. A host that supplies its own owns that decision; the digest
	// checks apply either way.
	HTTPClient *http.Client
	// Progress, when set, receives one line for each download or install that starts. The
	// lines name models and versions and never carry a URL.
	Progress io.Writer

	// releases replaces the pinned runtime builds. Only tests set it.
	releases []runtimeRelease
}

// Host provisions, serves and observes local models under one data directory. It is
// safe for concurrent use. Servers outlive the Host that started them: [Host.Close]
// releases the sandbox, not the servers, which a later Host over the same directory finds
// through [Host.Status] and ends through [Host.Stop].
type Host struct {
	dataDir  string
	models   []Model
	releases []provision.Release
	dl       *fetch.Downloader
	sb       *sandbox.Local
	mgr      *serve.Manager
	reg      *serve.Registry
	ledger   *modelsource.Ledger
	progress io.Writer
	// minContainment is the weakest isolation Serve accepts; containment reads what the
	// sandbox provides.
	minContainment sandbox.Containment
	containment    func() sandbox.Containment
	// preLaunch, when a test sets it, runs just before the last digest check, with the port
	// chosen for the server.
	preLaunch func(port int)

	floorMu sync.RWMutex
	floor   hostFloor

	locks keyedLock

	mu     sync.Mutex
	tokens map[int]string
}

// Sentinel errors, matched with [errors.Is]. The error a call returns also carries a
// message with detail; the sentinel is the stable part.
var (
	// ErrUnpinned is returned for a download that names no digest to check it against.
	ErrUnpinned = errors.New("localmodel: no pinned digest")
	// ErrDigestMismatch is returned when a file on disk no longer hashes to the digest it
	// was installed under.
	ErrDigestMismatch = errors.New("localmodel: digest mismatch")
	// ErrNotServable is returned for a source that can be classified but not served here.
	ErrNotServable = errors.New("localmodel: source cannot be served")
	// ErrNotAdmitted is returned when the sandbox provides less isolation than the
	// source's trust requires.
	ErrNotAdmitted = errors.New("localmodel: sandbox isolation is insufficient")
	// ErrUnauthenticated is returned when a started server answered a request that
	// carried no token. The server is stopped before the error is returned.
	ErrUnauthenticated = errors.New("localmodel: server accepted an unauthenticated request")
	// ErrWrongListener is returned when the process answering on a server's port refused the
	// key written for that launch, so it is not the server that was started. The server is
	// stopped before the error is returned.
	ErrWrongListener = errors.New("localmodel: the listener is not the server that was launched")
	// ErrNotRunning is returned when no server is running for a model.
	ErrNotRunning = errors.New("localmodel: model is not running")
)

// Open builds a Host over cfg.DataDir. It creates the directory tree owner-only, builds
// the confining sandbox the servers run in, and validates a supplied catalog.
func Open(cfg Config) (*Host, error) {
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("localmodel: DataDir must be an absolute path")
	}
	// The catalog is copied so nothing the caller keeps a reference to can change what the
	// host trusts after it was validated.
	models := cloneModels(cfg.Catalog)
	if len(models) == 0 {
		c, err := catalog.Load()
		if err != nil {
			return nil, fmt.Errorf("localmodel: load catalog: %w", err)
		}
		models = cloneModels(c.Models)
	} else if err := (catalog.Catalog{Models: models}).Validate(); err != nil {
		return nil, fmt.Errorf("localmodel: %w", err)
	}
	minContainment, err := parseMinContainment(cfg.MinContainment)
	if err != nil {
		return nil, err
	}
	releases, err := pinnedReleases(cfg.releases)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(cfg.DataDir, "run")
	for _, d := range []string{cfg.DataDir, runDir, filepath.Join(runDir, keyDirName), filepath.Join(cfg.DataDir, "models"), filepath.Join(cfg.DataDir, "runtimes")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("localmodel: create %s: %w", filepath.Base(d), err)
		}
	}
	var sb *sandbox.Local
	confinement := sandbox.WithDefaultConfinement()
	containment := func() sandbox.Containment { return sandbox.ContainmentOf(sb) }
	if minContainment < sandbox.ContainmentKernel {
		// The sandbox refuses a launch it cannot confine, so a host that accepts a process
		// jail has to say so to the sandbox too. A launch under the opt-in may run at the
		// floor without the sandbox's reported tier changing, so every server this Host
		// starts is recorded at the floor: a later Host that requires kernel confinement
		// then refuses to adopt it, where a kernel record could hand it an unconfined server.
		confinement = sandbox.WithBestEffortConfinement(func(error) {})
		containment = func() sandbox.Containment { return sandbox.ContainmentNone }
	}
	sb, err = sandbox.NewLocal(runDir, confinement)
	if err != nil {
		return nil, fmt.Errorf("localmodel: sandbox: %w", err)
	}
	h := &Host{
		dataDir:  cfg.DataDir,
		models:   models,
		releases: releases,
		dl:       fetch.New(fetch.WithHTTPClient(cfg.HTTPClient)),
		sb:       sb,
		ledger:   modelsource.NewLedger(filepath.Join(cfg.DataDir, "models")),
		reg:      serve.NewRegistry(runDir),
		progress: cfg.Progress,
		tokens:   map[int]string{},
		locks:    newKeyedLock(),

		minContainment: minContainment,
		containment:    containment,
	}
	h.mgr = h.newManager(serve.SandboxLauncher{SB: sb})
	return h, nil
}

// newManager builds the server manager over l. The readiness probe carries no credential:
// it runs before the listener is known to be the server this host started.
func (h *Host) newManager(l serve.Launcher) *serve.Manager {
	return serve.NewManager(
		l,
		serve.HTTPProbe(h.client(false)),
		serve.OSKiller,
		h.reg,
		serve.WithStatsSource(Runtime, serve.LlamaCppStatsSource(h.loopbackClient())),
	)
}

// Close releases the sandbox. Servers already started keep running.
func (h *Host) Close() error { return h.sb.Close() }

// say writes a progress line when the host was given a [Config.Progress] writer.
func (h *Host) say(format string, args ...any) {
	if h.progress != nil {
		_, _ = fmt.Fprintf(h.progress, format+"\n", args...)
	}
}

// platform returns the operating system and architecture a runtime build is chosen for.
func platform() (goos, goarch string) { return runtime.GOOS, runtime.GOARCH }

// parseMinContainment reads [Config.MinContainment].
func parseMinContainment(v string) (sandbox.Containment, error) {
	switch v {
	case "", "kernel":
		return sandbox.ContainmentKernel, nil
	case "process":
		return sandbox.ContainmentNone, nil
	}
	return 0, fmt.Errorf("localmodel: MinContainment %q is not \"kernel\" or \"process\"", v)
}

// keyedLock serializes work per key and gives up when the caller's context ends.
type keyedLock struct {
	mu sync.Mutex
	ch map[string]chan struct{}
}

func newKeyedLock() keyedLock { return keyedLock{ch: map[string]chan struct{}{}} }

// acquire waits for the key with no deadline and returns the function that releases it.
func (k *keyedLock) acquire(key string) func() {
	unlock, _ := k.lock(context.Background(), key) // a background context is never done
	return unlock
}

// lock waits for the key and returns the function that releases it.
func (k *keyedLock) lock(ctx context.Context, key string) (func(), error) {
	k.mu.Lock()
	c, ok := k.ch[key]
	if !ok {
		c = make(chan struct{}, 1)
		k.ch[key] = c
	}
	k.mu.Unlock()
	select {
	case c <- struct{}{}:
		return func() { <-c }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
