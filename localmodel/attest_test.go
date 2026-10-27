package localmodel_test

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/localmodel"
	"github.com/ionalpha/flynn/sandbox"
)

// reopen opens a second Host over the fixture's data directory, standing in for a later,
// separate invocation that finds the server a first one started.
func reopen(t *testing.T, f *fixture, mutate func(*localmodel.Config)) *localmodel.Host {
	t.Helper()
	cfg := f.config()
	if mutate != nil {
		mutate(&cfg)
	}
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// editRecord rewrites the registry entry the running server was recorded under.
func editRecord(t *testing.T, f *fixture, edit func(rec map[string]any)) {
	t.Helper()
	path := filepath.Join(f.dataDir, "run", "servers.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	if err := json.Unmarshal(b, &recs); err != nil || len(recs) != 1 {
		t.Fatalf("registry = %s (%v); want one record", b, err)
	}
	edit(recs[0])
	out, err := json.Marshal(recs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatal(err)
	}
}

// waitStopped fails unless nothing answers at url within a few seconds.
func waitStopped(t *testing.T, url string) {
	t.Helper()
	c := &http.Client{Timeout: time.Second}
	for range 100 {
		resp, err := c.Get(url) //nolint:noctx // a short liveness poll against loopback
		if err != nil {
			return
		}
		_ = resp.Body.Close()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the server at %s is still answering", url)
}

func keyFile(f *fixture) string { return filepath.Join(f.dataDir, "run", "keys", "test_model.key") }

// An adopted server is held to the floor in force when it is found, not the one in force
// when it started.
func TestAdoptionAppliesTheFloor(t *testing.T) {
	t.Run("control: a later Host adopts a server at the floor", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{version: "b60000"})
		_, ep := serveOnce(t, f)
		got, err := reopen(t, f, nil).Reach(t.Context(), f.model.ID)
		if err != nil || !got.Reused || got.PID != ep.PID {
			t.Fatalf("Reach = %+v, %v; want the running server", got, err)
		}
	})
	t.Run("a floor raised after the server started stops it", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{version: "b60000"})
		_, ep := serveOnce(t, f)
		later := reopen(t, f, nil)
		if err := later.RaiseFloor("b70000", "TEST-ADVISORY-3"); err != nil {
			t.Fatal(err)
		}
		if _, err := later.Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
			t.Fatalf("Reach = %v; want ErrNotRunning", err)
		}
		waitStopped(t, ep.BaseURL+"/models")
		if _, err := os.Stat(keyFile(f)); !os.IsNotExist(err) {
			t.Fatalf("the key file of a stopped server is still there: %v", err)
		}
	})
	t.Run("Serve does not adopt a server below a raised floor", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{version: "b60000"})
		_, ep := serveOnce(t, f)
		later := reopen(t, f, nil)
		if err := later.RaiseFloor("b70000", "TEST-ADVISORY-4"); err != nil {
			t.Fatal(err)
		}
		if got, err := later.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); err == nil {
			t.Fatalf("Serve handed out %+v from a build below the floor", got)
		}
		waitStopped(t, ep.BaseURL+"/models")
	})
	t.Run("a record that names no build is below every floor", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{version: "b60000"})
		_, ep := serveOnce(t, f)
		editRecord(t, f, func(rec map[string]any) { delete(rec, "version") })
		if _, err := reopen(t, f, nil).Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
			t.Fatalf("Reach = %v; want ErrNotRunning", err)
		}
		waitStopped(t, ep.BaseURL+"/models")
	})
}

// A running server is adopted only if it can still be vouched for.
func TestAdoptionRefusesWhatItCannotVouchFor(t *testing.T) {
	t.Run("isolation below what the Host asks for", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		_, ep := serveOnce(t, f)
		editRecord(t, f, func(rec map[string]any) { rec["containment"] = sandbox.ContainmentNone.String() })

		kernel := reopen(t, f, func(c *localmodel.Config) { c.MinContainment = "kernel" })
		if _, err := kernel.Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
			t.Fatalf("Reach = %v; want ErrNotRunning for a jailed server under a kernel requirement", err)
		}
		waitStopped(t, ep.BaseURL+"/models")
	})
	t.Run("a server a process-jail Host started", func(t *testing.T) {
		// The sandbox does not say which launches fell back to the floor, so a Host that
		// accepted a jail records every server as one, and a Host that requires the kernel
		// never adopts it on a guess.
		f := newFixture(t, fixtureOpts{})
		_, ep := serveOnce(t, f)
		if ep.Containment != sandbox.ContainmentNone.String() {
			t.Fatalf("Containment = %q; want %q from a Host that accepted a jail", ep.Containment, sandbox.ContainmentNone)
		}
		kernel := reopen(t, f, func(c *localmodel.Config) { c.MinContainment = "kernel" })
		if _, err := kernel.Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
			t.Fatalf("Reach = %v; want ErrNotRunning for a server recorded as a jail", err)
		}
		waitStopped(t, ep.BaseURL+"/models")
	})
	t.Run("isolation recorded at the level asked for", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		serveOnce(t, f)
		editRecord(t, f, func(rec map[string]any) { rec["containment"] = sandbox.ContainmentKernel.String() })

		kernel := reopen(t, f, func(c *localmodel.Config) { c.MinContainment = "kernel" })
		got, err := kernel.Reach(t.Context(), f.model.ID)
		if err != nil || got.Containment != sandbox.ContainmentKernel.String() {
			t.Fatalf("Reach = %+v, %v; want the server adopted at the recorded level", got, err)
		}
	})
	t.Run("a key file from before the probe key existed", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		h, ep := serveOnce(t, f)
		if err := os.WriteFile(keyFile(f), []byte(ep.Token.Expose()+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
			t.Fatalf("Reach = %v; want ErrNotRunning", err)
		}
		waitStopped(t, ep.BaseURL+"/models")
	})
	t.Run("a listener that refuses the launch's probe key", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		h, ep := serveOnce(t, f)
		if err := os.WriteFile(keyFile(f), []byte(ep.Token.Expose()+"\nnot-the-probe-key\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := h.Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrWrongListener) {
			t.Fatalf("Reach = %v; want ErrWrongListener", err)
		}
		waitStopped(t, ep.BaseURL+"/models")
	})
}

// squatter is a listener that took the port a server was about to be given.
type squatter struct {
	port   int
	token  string
	mu     sync.Mutex
	hits   int
	leaked bool
}

func (s *squatter) seen() (hits int, leaked bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hits, s.leaked
}

// squat listens on port and answers with answer. Each request is compared with the first
// line of the launch's key file, which is the session token, so a request that carries it
// is recorded as a leak.
func squat(t *testing.T, key string, port int, answer http.HandlerFunc) *squatter {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		t.Fatal(err)
	}
	s := &squatter{port: port}
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.hits++
		if s.token == "" {
			if b, err := os.ReadFile(key); err == nil {
				if keys := strings.Fields(string(b)); len(keys) > 0 {
					s.token = keys[0]
				}
			}
		}
		if s.token != "" && r.Header.Get("Authorization") == "Bearer "+s.token {
			s.leaked = true
		}
		s.mu.Unlock()
		answer(w, r)
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return s
}

func isModelsProbe(r *http.Request) bool { return strings.HasSuffix(r.URL.Path, "/models") }

// Another process can take the chosen port in the moment between choosing it and the
// runtime binding it. Whatever answers there must be told apart from the server, and must
// never be sent the token.
func TestSquatterNeverSeesTheToken(t *testing.T) {
	cases := []struct {
		name   string
		answer http.HandlerFunc
		want   error
	}{
		{"answers 200 to everything", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"data":[]}`) }, localmodel.ErrUnauthenticated},
		{"accepts any bearer token", func(w http.ResponseWriter, r *http.Request) {
			if isModelsProbe(r) || strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				_, _ = io.WriteString(w, `{"data":[]}`)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}, localmodel.ErrUnauthenticated},
		{"refuses every credential", func(w http.ResponseWriter, r *http.Request) {
			if isModelsProbe(r) {
				_, _ = io.WriteString(w, `{"data":[]}`)
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}, localmodel.ErrWrongListener},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{})
			h := f.open()
			var sq *squatter
			h.BeforeLaunchCheck(func(port int) { sq = squat(t, keyFile(f), port, tc.answer) })

			_, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
			if !errors.Is(err, tc.want) {
				t.Fatalf("Serve = %v; want %v", err, tc.want)
			}
			hits, leaked := sq.seen()
			if hits == 0 {
				t.Fatal("the squatter saw no request, so the test proved nothing")
			}
			if leaked {
				t.Fatal("the session token was sent to a listener that was never shown to be the server")
			}
			if st, _ := h.Status(t.Context()); len(st) != 0 {
				t.Fatalf("a server was recorded: %+v", st)
			}
			if _, err := os.Stat(keyFile(f)); !os.IsNotExist(err) {
				t.Fatalf("the key file of a refused launch is still there: %v", err)
			}

			// Nothing about the failed launch leaves the client carrying a token to that port.
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(sq.port)+"/v1/chat/completions", strings.NewReader("{}"))
			if resp, err := h.Client().Do(req); err == nil {
				_ = resp.Body.Close()
			}
			if _, leaked := sq.seen(); leaked {
				t.Fatal("the client sent the token to the squatted port after the refused launch")
			}
		})
	}
}

// Two callers asking for one model at once get the same server: none is started twice,
// none is killed, and none fails because it lost a race.
func TestConcurrentServeStartsOneServer(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	spy := h.SpyOnLaunches()

	const callers = 6
	type result struct {
		ep  localmodel.Endpoint
		err error
	}
	results := make([]result, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i].ep, results[i].err = h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		}()
	}
	wg.Wait()

	reused := 0
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("caller %d: %v", i, r.err)
		}
		if r.ep.PID != results[0].ep.PID || r.ep.Port != results[0].ep.Port {
			t.Fatalf("caller %d got a different server: %+v vs %+v", i, r.ep, results[0].ep)
		}
		if r.ep.Reused {
			reused++
		}
	}
	if reused != callers-1 {
		t.Fatalf("%d callers adopted the server; want %d and exactly one launch", reused, callers-1)
	}
	if n := len(spy.Specs()); n != 1 {
		t.Fatalf("the server was launched %d times", n)
	}
	if st, err := h.Status(t.Context()); err != nil || len(st) != 1 {
		t.Fatalf("status = %+v, %v; want the one server still up", st, err)
	}
}

// A caller queued behind a launch stops waiting when its own context ends, and the launch
// it waited for is not disturbed.
func TestServeWaitsForTheLockAndHonoursItsContext(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.BeforeLaunchCheck(func(int) {
		once.Do(func() { close(entered) })
		<-release
	})

	first := make(chan error, 1)
	go func() {
		_, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		first <- err
	}()
	<-entered

	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	if _, err := h.Serve(ctx, f.model.ID, localmodel.ServeOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a queued Serve = %v; want its context's deadline", err)
	}
	if _, err := h.Reach(ctx, f.model.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a queued Reach = %v; want its context's deadline", err)
	}

	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the launch that held the lock failed: %v", err)
	}
}

// The server is launched confined, and the isolation reported is the one the sandbox
// provides. A Host refuses to launch when that is less than it was told to require.
func TestServeReportsAndEnforcesConfinement(t *testing.T) {
	t.Run("the launch asks for confinement and reports what it got", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		h := f.open()
		spy := h.SpyOnLaunches()
		h.SetContainment(sandbox.ContainmentKernel)
		ep, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		specs := spy.Specs()
		if len(specs) != 1 || !specs[0].Confine {
			t.Fatalf("launch specs = %+v; want one with Confine set", specs)
		}
		if ep.Containment != sandbox.ContainmentKernel.String() {
			t.Fatalf("Containment = %q; want %q", ep.Containment, sandbox.ContainmentKernel)
		}
	})
	t.Run("a jail is refused unless the Host accepted one", func(t *testing.T) {
		for _, floor := range []string{"", "kernel"} {
			f := newFixture(t, fixtureOpts{})
			h := reopen(t, f, func(c *localmodel.Config) { c.MinContainment = floor })
			spy := h.SpyOnLaunches()
			h.SetContainment(sandbox.ContainmentNone)
			_, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
			if !errors.Is(err, localmodel.ErrNotAdmitted) {
				t.Fatalf("MinContainment %q: Serve = %v; want ErrNotAdmitted", floor, err)
			}
			if !strings.Contains(err.Error(), sandbox.ContainmentNone.String()) {
				t.Fatalf("the refusal does not say what the host provides: %v", err)
			}
			if n := len(spy.Specs()); n != 0 {
				t.Fatalf("a server was launched anyway (%d)", n)
			}
			if f.hits.Load() != 0 {
				t.Fatalf("the origin was contacted %d times before the refusal", f.hits.Load())
			}
		}
	})
	t.Run("a jail is served when the Host accepts one, and is reported as a jail", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		h := f.open()
		h.SetContainment(sandbox.ContainmentNone)
		ep, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if ep.Containment != sandbox.ContainmentNone.String() {
			t.Fatalf("Containment = %q; want %q", ep.Containment, sandbox.ContainmentNone)
		}
	})
	t.Run("an unknown level is refused at Open", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		cfg := f.config()
		cfg.MinContainment = "none"
		if _, err := localmodel.Open(cfg); err == nil {
			t.Fatal("Open accepted a MinContainment it does not define")
		}
	})
}

// The weights are checked once more just before launch, so a file replaced after the
// earlier checks never reaches the runtime.
func TestWeightsSwappedBeforeLaunchAreRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	spy := h.SpyOnLaunches()
	w, err := h.EnsureWeights(t.Context(), f.model.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	h.BeforeLaunchCheck(func(int) {
		if err := os.Chmod(w.Path, 0o600); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(w.Path, []byte("swapped after the earlier checks"), 0o600); err != nil {
			t.Error(err)
		}
	})
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); !errors.Is(err, localmodel.ErrDigestMismatch) {
		t.Fatalf("Serve = %v; want ErrDigestMismatch", err)
	}
	if n := len(spy.Specs()); n != 0 {
		t.Fatalf("the runtime was launched on swapped weights (%d launches)", n)
	}
}

// A cached runtime whose recorded hash is gone is refused, and the refusal does not write
// a new record: a hash taken from the file as it stands would vouch for anything there.
func TestCachedRuntimeWithoutRecordIsRefusedNotRecorded(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	rt, err := h.EnsureRuntime(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sidecar := rt.BinPath + ".sha256"
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := h.EnsureRuntime(t.Context()); err == nil {
			t.Fatal("a cached runtime with no recorded hash was accepted")
		}
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Fatalf("the record was written back: %v", err)
		}
	}
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); err == nil {
		t.Fatal("Serve ran a runtime with no recorded hash")
	}
}

// A host outside the module cannot name the runtime build it runs: the version is a claim
// about a binary this package never executes, and the advisory gate compares against it.
func TestHostCannotSupplyARuntimeBuild(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for id := range af.Scope.Objects {
			if ast.IsExported(id) && strings.Contains(id, "Release") {
				t.Errorf("%s declares %s; a release set must not be part of the public API", name, id)
			}
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if fld, ok := n.(*ast.Field); ok {
				for _, nm := range fld.Names {
					if ast.IsExported(nm.Name) && strings.Contains(nm.Name, "Release") {
						t.Errorf("%s has an exported field %s that can name a runtime build", name, nm.Name)
					}
				}
			}
			return true
		})
	}
}

// The models a Host returns are copies: editing one cannot change what the Host fetches
// or trusts, and neither can editing the catalog the caller opened it with.
func TestReturnedModelsCannotEditTheCatalog(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	cfg := f.config()
	h, err := localmodel.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	digest := f.model.Quants[0].Digest

	listed := h.Models(localmodel.ModelQuery{})
	if len(listed) != 1 {
		t.Fatalf("Models = %d entries; want the fixture's one", len(listed))
	}
	listed[0].Quants[0].Digest = "sha256:" + strings.Repeat("0", 64)
	listed[0].Quants[0].URL = "https://evil.example/weights"

	one, err := h.Model(f.model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if one.Quants[0].Digest != digest || strings.Contains(one.Quants[0].URL, "evil") {
		t.Fatalf("an edit through Models reached the host's catalog: %+v", one.Quants[0])
	}
	one.Quants[0].Digest = "sha256:" + strings.Repeat("1", 64)
	if again, _ := h.Model(f.model.ID); again.Quants[0].Digest != digest {
		t.Fatal("an edit through Model reached the host's catalog")
	}

	cfg.Catalog[0].Quants[0].Digest = "sha256:" + strings.Repeat("2", 64)
	if later, _ := h.Model(f.model.ID); later.Quants[0].Digest != digest {
		t.Fatal("an edit to the catalog slice passed to Open reached the host")
	}
}

// A listener that cannot be asked is not attributed, whichever of the three questions
// fails to get an answer.
func TestAttestFailsClosedWhenTheListenerCannotBeAsked(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()

	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL + "/v1"
	dead.Close()
	if err := h.Attest(t.Context(), deadURL, "probe"); !errors.Is(err, localmodel.ErrUnauthenticated) {
		t.Fatalf("a closed port: %v; want ErrUnauthenticated", err)
	}

	// dropAt closes the connection on the nth request and answers the rest as a server
	// that enforces a credential would.
	dropAt := func(n int) string {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if int(calls.Add(1)) == n {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					_ = conn.Close()
				}
				return
			}
			if r.Header.Get("Authorization") == "Bearer probe" {
				return
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/v1"
	}
	if err := h.Attest(t.Context(), dropAt(2), "probe"); !errors.Is(err, localmodel.ErrUnauthenticated) {
		t.Fatalf("a listener that drops the made-up credential: %v; want ErrUnauthenticated", err)
	}
	if err := h.Attest(t.Context(), dropAt(3), "probe"); !errors.Is(err, localmodel.ErrWrongListener) {
		t.Fatalf("a listener that drops the probe key: %v; want ErrWrongListener", err)
	}
	if err := h.Attest(t.Context(), dropAt(99), "probe"); err != nil {
		t.Fatalf("a listener that behaves as the server does: %v", err)
	}
	if err := h.Attest(t.Context(), dropAt(99), "other"); !errors.Is(err, localmodel.ErrWrongListener) {
		t.Fatalf("a listener that refuses the launch's key: %v; want ErrWrongListener", err)
	}
}

// Calls that name no running or servable model fail without starting anything.
func TestNothingRunningIsReportedAsSuch(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	spy := h.SpyOnLaunches()

	if _, err := h.Serve(t.Context(), "no/such-model", localmodel.ServeOptions{}); err == nil {
		t.Fatal("Serve accepted a model that is not in the catalog")
	}
	if _, err := h.Stats(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
		t.Fatalf("Stats = %v; want ErrNotRunning", err)
	}
	if found, err := h.Stop(f.model.ID); err != nil || found {
		t.Fatalf("Stop = %v, %v; want nothing found", found, err)
	}
	if n := len(spy.Specs()); n != 0 {
		t.Fatalf("%d servers were launched", n)
	}
}
