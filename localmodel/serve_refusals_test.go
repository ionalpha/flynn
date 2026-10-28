package localmodel_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/localmodel"
)

// A source outside the catalog is classified, and when the host admits it, Serve still
// refuses it by name: only catalog models carry a pinned digest to serve from.
func TestServeRefusesAnAdmittedSourceOutsideTheCatalog(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	const ref = "hf:tester/repo/model.gguf"
	a, err := h.Classify(ref)
	if err != nil {
		t.Fatal(err)
	}
	_, err = h.Serve(t.Context(), ref, localmodel.ServeOptions{})
	switch {
	case a.Admitted && !errors.Is(err, localmodel.ErrNotServable):
		t.Fatalf("Serve of an admitted non-catalog source = %v; want ErrNotServable", err)
	case !a.Admitted && !errors.Is(err, localmodel.ErrNotAdmitted):
		t.Fatalf("Serve of a refused source = %v; want ErrNotAdmitted", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("a refused source caused downloads")
	}
}

// A catalog model with no quantization this package can serve fails before the runtime or
// any weights are fetched.
func TestServeNeedsAServableQuantization(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.model.Quants[0].Format = "pickle"
	h := f.open()
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); !errors.Is(err, localmodel.ErrNotServable) {
		t.Fatalf("Serve = %v; want ErrNotServable", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("a model with nothing servable caused downloads")
	}
}

// Serve calls for one model run one at a time, and a caller waiting its turn gives up when
// its context ends instead of queueing behind a launch without bound.
func TestServeWaitsForItsTurnAndHonoursItsContext(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	held, release := make(chan struct{}), make(chan struct{})
	h.BeforeLaunchCheck(func(int) {
		close(held)
		<-release
	})
	first := make(chan error, 1)
	go func() {
		_, err := h.Serve(context.Background(), f.model.ID, localmodel.ServeOptions{})
		first <- err
	}()
	select {
	case <-held:
	case <-time.After(60 * time.Second):
		t.Fatal("the first launch never reached its last check")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := h.Serve(ctx, f.model.ID, localmodel.ServeOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second Serve = %v; want the context's deadline", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("first Serve: %v", err)
	}
}

// A key file that cannot be written stops the launch before any server starts.
func TestServeStopsWhenItCannotWriteTheKeyFile(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	if err := os.MkdirAll(filepath.Join(keyFile(f), "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	spy := h.SpyOnLaunches()
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); err == nil || !strings.Contains(err.Error(), "token file") {
		t.Fatalf("Serve = %v; want a token file error", err)
	}
	if n := len(spy.Specs()); n != 0 {
		t.Fatalf("%d servers were started without a key file", n)
	}
}

// Servers recorded for another model are none of a launch's business, and adopting one's
// neighbour must not hand out the wrong endpoint.
func TestServeLeavesAnotherModelsServerAlone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	other := f.model
	other.ID = "test/other"
	other.Quants = append([]localmodel.Quant(nil), f.model.Quants...)
	cfg := f.config()
	cfg.Catalog = append(cfg.Catalog, other)
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, id := range []string{f.model.ID, other.ID} {
			_, _ = h.Stop(id)
		}
		_ = h.Close()
	})

	a, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := h.Serve(t.Context(), other.ID, localmodel.ServeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Port == b.Port || b.Reused || b.ModelID != other.ID {
		t.Fatalf("second model got %+v after %+v", b, a)
	}
	if _, err := h.Reach(t.Context(), f.model.ID); err != nil {
		t.Fatalf("the first model's server was disturbed: %v", err)
	}
}

// Weights that no longer match their digest are replaced, and when the path cannot be
// cleared the failure is the caller's to see, not a silent serve of the old file.
func TestEnsureWeightsReportsStaleWeightsItCannotRemove(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	w, err := h.EnsureWeights(t.Context(), f.model.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(w.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(w.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(w.Path, "occupied"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The host narrows the path's mode before it tries the removal, which on a directory
	// takes away the search bit the temporary directory's cleanup needs.
	t.Cleanup(func() { _ = os.Chmod(w.Path, 0o700) })
	if _, err := h.EnsureWeights(t.Context(), f.model.ID, ""); err == nil || !strings.Contains(err.Error(), "remove stale weights") {
		t.Fatalf("EnsureWeights = %v; want a stale weights removal error", err)
	}
}

// A run directory whose registry cannot be read is reported by every call that lists
// servers, and the client falls back to presenting no token.
func TestUnreadableRegistryIsReported(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	if err := os.MkdirAll(filepath.Join(f.dataDir, "run", "servers.json"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Status(t.Context()); err == nil {
		t.Fatal("Status read an unreadable registry")
	}
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); err == nil {
		t.Fatal("Serve launched over an unreadable registry")
	}

	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
	}))
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := h.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if auth != "" {
		t.Fatalf("the client presented %q to a port no server of this host holds", auth)
	}
}

// A listener that cannot even be asked is not attributed: a base URL no request can be
// built for fails the handshake as an unauthenticated listener.
func TestAttestRefusesAnUnaskableListener(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	if err := h.Attest(t.Context(), "http://127.0.0.1:1/\x7f", "probe"); !errors.Is(err, localmodel.ErrUnauthenticated) {
		t.Fatalf("Attest = %v; want ErrUnauthenticated", err)
	}
}

// A second host that finds a server whose key file is gone has no token to present for it,
// so its client sends none rather than guessing.
func TestClientPresentsNoTokenWhenTheKeyFileIsGone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	ep, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second := reopen(t, f, nil)
	if err := os.Remove(keyFile(f)); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, ep.BaseURL+"/chat/completions", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := second.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d; want the server's refusal of a request with no token", resp.StatusCode)
	}
}
