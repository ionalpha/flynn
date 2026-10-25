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
//     first install and on a cached one. [Host.RaiseFloor] can only raise a floor.
//   - The prompt contract is the catalog's chat template. A template embedded in the
//     weights is read with a hardened parser, reported through [Endpoint.TemplateOverridden],
//     and replaced.
//   - The runtime runs in the sandbox the [Host] built, with the platform's kernel
//     confinement where it can be applied. A source whose trust needs stronger isolation
//     than the sandbox provides is refused.
//   - Weights are fetched against a catalog digest and refused without one. The digest is
//     checked again just before launch, and the runtime binary is checked against the
//     hash recorded when it was installed, so a file swapped after provisioning is not
//     served.
//   - The endpoint demands a per-launch bearer token ([Endpoint.Token]). The token reaches
//     the runtime through a file only the owning account can read, never the command line,
//     and the server is stopped if it answers an unauthenticated request.
//   - Errors do not carry tokens, and URLs in them are reduced to scheme and host.
//
// # Limits
//
// The sandbox leaves outbound network access open to a served runtime (see
// [Host.Serve]); the loopback pin and the token are what keep the endpoint private. On a
// platform without owner-only file permissions (Windows) the token file relies on the
// data directory's own access control.
//
// This package is experimental: it may change before it is listed in the stable tier.
package localmodel

import (
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
	"github.com/ionalpha/flynn/internal/inference"
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
	// Releases replaces the runtime builds this module pins when non-empty. A release
	// still goes through the advisory gate and must carry a digest.
	Releases []Release
	// HTTPClient carries every download. Nil uses a client that refuses private and
	// loopback addresses. A host that supplies its own owns that decision; the digest
	// checks apply either way.
	HTTPClient *http.Client
	// Progress, when set, receives one line for each download or install that starts. The
	// lines name models and versions and never carry a URL.
	Progress io.Writer
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
	// ErrNotRunning is returned when no server is running for a model.
	ErrNotRunning = errors.New("localmodel: model is not running")
)

// Open builds a Host over cfg.DataDir. It creates the directory tree owner-only, builds
// the confining sandbox the servers run in, and validates a supplied catalog.
func Open(cfg Config) (*Host, error) {
	if !filepath.IsAbs(cfg.DataDir) {
		return nil, errors.New("localmodel: DataDir must be an absolute path")
	}
	models := cfg.Catalog
	if len(models) == 0 {
		c, err := catalog.Load()
		if err != nil {
			return nil, fmt.Errorf("localmodel: load catalog: %w", err)
		}
		models = c.Models
	} else if err := (catalog.Catalog{Models: models}).Validate(); err != nil {
		return nil, fmt.Errorf("localmodel: %w", err)
	}
	releases, err := pinnedReleases(cfg.Releases)
	if err != nil {
		return nil, err
	}
	runDir := filepath.Join(cfg.DataDir, "run")
	for _, d := range []string{cfg.DataDir, runDir, filepath.Join(runDir, keyDirName), filepath.Join(cfg.DataDir, "models"), filepath.Join(cfg.DataDir, "runtimes")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("localmodel: create %s: %w", filepath.Base(d), err)
		}
	}
	sb, err := sandbox.NewLocal(runDir, sandbox.WithDefaultConfinement())
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
	}
	h.mgr = serve.NewManager(
		serve.SandboxLauncher{SB: sb},
		serve.HTTPProbe(h.loopbackClient()),
		serve.OSKiller,
		h.reg,
		serve.WithStatsSource(Runtime, serve.LlamaCppStatsSource(h.loopbackClient())),
	)
	return h, nil
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

// floorOf reports the advisory floor for a runtime as text, empty when it has none.
func floorOf(name string) string {
	v, ok, _ := inference.FloorFor(name)
	if !ok {
		return ""
	}
	return v.String()
}
