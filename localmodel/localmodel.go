// Package localmodel runs a catalog model on this machine behind the guards the flynn
// binary applies, so an embedding host need not rebuild them. A model server started
// from a bare download has no advisory gate, digest pinning or sandbox, and each gap lets
// attacker-chosen code run next to the agent.
//
// A [Host] takes a model from reference to endpoint: [Host.Classify] decides trust and
// whether the sandbox isolates it enough, [Host.EnsureRuntime] installs a gated, pinned
// runtime build, [Host.EnsureWeights] fetches weights against the catalog digest,
// [Host.Serve] starts the server and returns an [Endpoint], and [Host.Status],
// [Host.Reach], [Host.Stats] and [Host.Stop] observe and end it.
//
// # Guarantees
//
// Each holds on every exported path, with no switch to turn it off:
//
//   - The server binds 127.0.0.1 only; nothing in [Config] names an address.
//   - A runtime build below its advisory floor is refused before any network access, and
//     a running server below it is stopped instead of adopted. [Host.RaiseFloor] only
//     raises, and the gate reads the version this module pins.
//   - The prompt contract is the catalog's chat template. A template in the weights is
//     read with a hardened parser, reported through [Endpoint.TemplateOverridden], and
//     replaced.
//   - [Host.Serve] requires kernel confinement unless [Config.MinContainment] accepts a
//     process jail, and refuses a source whose trust needs more isolation than the
//     sandbox gives. [Endpoint.Containment] reports the level.
//   - Weights need a catalog digest, checked again just before launch, and the runtime
//     binary is checked against its install-time hash, so a swapped file is not served.
//   - The endpoint demands a per-launch token ([Endpoint.Token]) passed in an owner-only
//     file, never argv. Before the token is sent, the listener must refuse no token and a
//     made-up one and accept a key only the key file holds, or it is stopped.
//   - Errors carry no tokens, and URLs in them are reduced to scheme and host.
//
// # Limits
//
// A served runtime keeps outbound network access. Two processes starting one model race,
// and the later stops the server it did not start. On Windows the token file relies on
// the data directory's ACL. The runtime opens weights by path after the last digest
// check, while the file is read-only to its owner. A runtime outliving a crashed host is
// not reaped. A runtime that cannot pass the listener handshake is refused. This package
// is experimental and may change.
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
	"github.com/ionalpha/flynn/internal/hardware"
	"github.com/ionalpha/flynn/internal/inference/gpu"
	"github.com/ionalpha/flynn/internal/inference/modelsource"
	"github.com/ionalpha/flynn/internal/inference/provision"
	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
)

// Runtime is the name of the inference runtime a [Host] provisions and serves.
const Runtime = "llama.cpp"

// Model is a catalog entry: identity, provenance, license, size, capabilities and the
// downloadable quantizations, each with the digest its bytes are checked against.
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

// The Trust values. Only [TrustBlessed] makes a [Config.Catalog] entry a pinned catalog
// source; the others are classified by the origin the entry names.
const (
	TrustBlessed    = catalog.TrustBlessed
	TrustDiscovered = catalog.TrustDiscovered
	TrustLocal      = catalog.TrustLocal
)

// Format is a weight file's encoding. Pickle is refused by every [Host] method.
type Format = catalog.Format

// Arch is the network shape of a [Model]: the layer and head counts that size its
// key/value cache, and for a mixture-of-experts model the expert counts a GPU and RAM
// split is planned from.
type Arch = catalog.Arch

// Machine is the free memory a [Model] is planned against: VRAM not already in use, and
// the system RAM a split may spill into.
type Machine = catalog.Machine

// FitSettings chooses the context length and cache precision [Model.PlanFit] sizes for.
type FitSettings = catalog.FitSettings

// FitPlan is [Model.PlanFit]'s verdict with the memory arithmetic behind it.
type FitPlan = catalog.FitPlan

// KVType is the precision the key/value cache is stored in.
type KVType = catalog.KVType

// The KVType values.
const (
	KVF16 = catalog.KVF16
	KVQ8  = catalog.KVQ8
)

// Placement is where [Model.PlanFit] runs a model: on the GPU, split across GPU and
// system RAM, or on the CPU.
type Placement = catalog.Placement

// The Placement values.
const (
	PlacementAPI   = catalog.PlacementAPI
	PlacementGPU   = catalog.PlacementGPU
	PlacementSplit = catalog.PlacementSplit
	PlacementCPU   = catalog.PlacementCPU
	PlacementNone  = catalog.PlacementNone
)

// Config says where a [Host] keeps its files and, for a host that curates its own
// catalog, what it may serve. The zero values of the optional fields give the catalog and
// runtime pins that ship with this module.
type Config struct {
	// DataDir is the absolute directory holding downloaded runtimes, weights, the run
	// registry and the per-launch token files. It is created owner-only. Required.
	DataDir string
	// Catalog replaces the embedded catalog when non-empty. Every entry is validated, so
	// a local model without a known chat template is rejected at [Open]. Only an entry
	// whose Trust is [TrustBlessed] is reported by [Host.Classify] as a pinned catalog
	// model. An entry at any other tier (including the zero value) is classified by the
	// origin it names, as a file, a URL or a Hub repository, so a catalog merged from a
	// user's own entries cannot carry them into the trusted class. [Host.Serve] runs both
	// kinds, but the second only where the host's sandbox gives the isolation that origin
	// needs.
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

	// detectGPU, when a test sets it, stands in for the machine's GPU probe. gpuOnce keeps
	// the probe to one run per Host.
	detectGPU func(context.Context) gpu.Info
	gpuOnce   sync.Once
	gpuInfo   gpu.Info

	// detectBox is the read of the machine behind [Host.Machine]. A test sets it to describe
	// a machine other than the one it runs on.
	detectBox func(context.Context) hardware.Box

	// provided reads the isolation the sandbox gives, which [Host.Classify] and [Host.Serve]
	// admit a source against. A test sets it to stand a weaker or stronger host in.
	provided func() sandbox.Containment

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
	// ErrRuntimeUnrecorded is returned when an installed runtime binary has no hash
	// record beside it, as with Flynn v0.1.3 installs. Its provenance is unknown, so the
	// build is fetched again; it is not a mismatch.
	ErrRuntimeUnrecorded = errors.New("localmodel: runtime binary has no recorded hash")
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
		// The sandbox refuses a launch it cannot confine, so a process-jail opt-in must
		// reach the sandbox too. Such a launch may fall back without the reported tier
		// changing, so every server this Host starts is recorded at the floor, and a
		// later Host that requires kernel confinement refuses to adopt it.
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
		detectBox:      hardware.Detect,
		provided:       func() sandbox.Containment { return sandbox.ContainmentOf(sb) },
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
