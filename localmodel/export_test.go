package localmodel

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/internal/inference/serve"
	"github.com/ionalpha/flynn/sandbox"
)

// This file is compiled into the test binary only. It is the one place a test reaches
// into the package: what a host outside the module cannot do (name a runtime build, or
// pretend the sandbox is weaker or stronger than it is) is done here, so the public API
// carries no way to do it.

// Release is a runtime build a test pins in place of the module's own.
type Release = runtimeRelease

// WithReleases returns cfg with the builds a Host will install from.
func WithReleases(cfg Config, rs ...Release) Config {
	cfg.releases = rs
	return cfg
}

// SetContainment makes the Host see the sandbox as providing level, to test a refusal on
// a platform that provides more.
func (h *Host) SetContainment(level sandbox.Containment) {
	h.containment = func() sandbox.Containment { return level }
}

// BeforeLaunchCheck runs fn just before the last digest check of a launch, with the port
// the server will be told to listen on.
func (h *Host) BeforeLaunchCheck(fn func(port int)) { h.preLaunch = fn }

// LaunchSpy records every spec the Host's sandbox launcher is asked to start.
type LaunchSpy struct {
	mu    sync.Mutex
	specs []sandbox.ServeSpec
}

// Specs returns the specs seen so far.
func (s *LaunchSpy) Specs() []sandbox.ServeSpec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sandbox.ServeSpec(nil), s.specs...)
}

type spyLauncher struct {
	next serve.Launcher
	spy  *LaunchSpy
}

func (l spyLauncher) Serve(ctx context.Context, spec sandbox.ServeSpec) (serve.Proc, error) {
	l.spy.mu.Lock()
	l.spy.specs = append(l.spy.specs, spec)
	l.spy.mu.Unlock()
	return l.next.Serve(ctx, spec)
}

// SpyOnLaunches wraps the Host's launcher so the specs it is given can be read back.
func (h *Host) SpyOnLaunches() *LaunchSpy {
	spy := &LaunchSpy{}
	h.mgr = h.newManager(spyLauncher{next: serve.SandboxLauncher{SB: h.sb}, spy: spy})
	return spy
}

// Attest runs the listener handshake against baseURL with probeKey.
func (h *Host) Attest(ctx context.Context, baseURL, probeKey string) error {
	return h.attest(ctx, baseURL, probeKey)
}

// RedactText is the redaction every error and displayed line passes through.
func RedactText(s string, secrets ...string) string { return redactText(s, secrets...) }

// Fail redacts err as a Host does before returning it.
func (h *Host) Fail(err error) error { return h.fail(err) }

// RememberToken records a token for a port, as a launch does once its listener is attributed.
func (h *Host) RememberToken(port int, token string) { h.remember(port, token) }

// HashFile hashes a file as the launch checks do.
func HashFile(path string) (string, error) { return hashFile(path) }

// RecordRuntimeBinary stores the hash of bin in sidecar.
func RecordRuntimeBinary(bin, sidecar string) error { return recordRuntimeBinary(bin, sidecar) }

// VerifyRuntimeBinary checks bin against the hash recorded beside it.
func VerifyRuntimeBinary(bin string) error { return verifyRuntimeBinary(bin) }

// VerifyWeights is the last digest check before a launch.
func VerifyWeights(w Weights) error { return verifyWeights(w) }

// VerifiedTemplate hashes and then parses the weights through one open file.
func VerifiedTemplate(w Weights) error {
	_, err := verifiedTemplate(w, "chatml")
	return err
}

// SizeCeiling is the download cap for a declared size.
func SizeCeiling(size int64) int64 { return sizeCeiling(size) }

// SmallestServable picks the quantization a model is served from when none is named.
func SmallestServable(m Model) error {
	_, err := smallestServable(m)
	return err
}
