package localmodel_test

import (
	"bytes"
	"errors"
	"go/parser"
	"go/token"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/localmodel"
)

// TestExternalPackageUsesOnlyThePublicAPI keeps the tests in this package honest: they
// stand in for a host outside the module, so none may import an internal package.
func TestExternalPackageUsesOnlyThePublicAPI(t *testing.T) {
	files, err := filepath.Glob("*_test.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no test files found")
	}
	for _, f := range files {
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range af.Imports {
			if strings.Contains(imp.Path.Value, "/internal/") {
				t.Errorf("%s imports %s; an outside host cannot", f, imp.Path.Value)
			}
		}
	}
}

// serveOnce serves the fixture's model and returns the host and the endpoint.
func serveOnce(t *testing.T, f *fixture) (*localmodel.Host, localmodel.Endpoint) {
	t.Helper()
	h := f.open()
	ep, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	return h, ep
}

// The headline: a catalog model is provisioned from a digest-pinned origin and served
// inside the sandbox, reachable with its token, stoppable, and gone afterwards.
func TestServeCatalogModelInSandbox(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h, ep := serveOnce(t, f)

	if ep.ModelID != f.model.ID || ep.PID == 0 || ep.Port == 0 || ep.Token.Empty() {
		t.Fatalf("incomplete endpoint: %+v", ep)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ep.BaseURL+"/chat/completions", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+ep.Token.Expose())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authenticated request: status %d", resp.StatusCode)
	}

	again, err := h.Reach(t.Context(), f.model.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.PID != ep.PID || !again.Reused {
		t.Fatalf("Reach should find the running server: %+v", again)
	}
	st, err := h.Status(t.Context())
	if err != nil || len(st) != 1 || st[0].ModelID != f.model.ID {
		t.Fatalf("status = %+v, %v", st, err)
	}
	if _, err := h.Stats(t.Context(), f.model.ID); err != nil {
		t.Fatalf("stats: %v", err)
	}

	found, err := h.Stop(f.model.ID)
	if err != nil || !found {
		t.Fatalf("stop = %v, %v", found, err)
	}
	if _, err := h.Reach(t.Context(), f.model.ID); !errors.Is(err, localmodel.ErrNotRunning) {
		t.Fatalf("after stop, Reach = %v; want ErrNotRunning", err)
	}
	if _, err := os.Stat(filepath.Join(f.dataDir, "run", "keys")); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(f.dataDir, "run", "keys")); len(ents) != 0 {
		t.Fatalf("token file left behind after stop: %v", ents)
	}
}

// Refusal 1: nothing the public API exposes can bind off loopback.
func TestNoPathBindsOffLoopback(t *testing.T) {
	// The configuration has no field that names an address, so there is nothing to set.
	for _, typ := range []reflect.Type{reflect.TypeOf(localmodel.Config{}), reflect.TypeOf(localmodel.ServeOptions{})} {
		for i := range typ.NumField() {
			n := strings.ToLower(typ.Field(i).Name)
			for _, bad := range []string{"host", "addr", "bind", "listen", "iface", "interface"} {
				if strings.Contains(n, bad) {
					t.Errorf("%s.%s can name an address", typ.Name(), typ.Field(i).Name)
				}
			}
		}
	}

	f := newFixture(t, fixtureOpts{})
	_, ep := serveOnce(t, f)
	argv := argvOf(t, ep)
	if host, ok := flagValue(argv, "--host"); !ok || host != "127.0.0.1" {
		t.Fatalf("runtime was told to bind %q (present=%v)", host, ok)
	}
	if p, _ := flagValue(argv, "--port"); p != port(ep) {
		t.Fatalf("port flag %q does not match the endpoint %d", p, ep.Port)
	}
	if ep.BaseURL != "http://127.0.0.1:"+port(ep)+"/v1" {
		t.Fatalf("BaseURL = %q", ep.BaseURL)
	}
	// And the listener really is loopback only: a non-loopback address of this machine
	// must not answer on the port.
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		ipn, ok := a.(*net.IPNet)
		if !ok || ipn.IP.IsLoopback() || ipn.IP.To4() == nil {
			continue
		}
		c, err := net.DialTimeout("tcp", net.JoinHostPort(ipn.IP.String(), port(ep)), 500*1e6)
		if err == nil {
			_ = c.Close()
			t.Fatalf("server answers on %s", ipn.IP)
		}
	}
}

// Refusal 2: the advisory gate runs before any byte is fetched, on first install and on
// a cached one, and the only way to move the floor is up.
func TestAdvisoryGateCannotBeSkipped(t *testing.T) {
	// The floor is process-wide and cannot be lowered, so this test raises it to a band
	// of its own: b9000 is above the compiled-in floor and below b50000, and the default
	// fixture build, b99999, is above everything raised here, so test order does not matter.
	h := newFixture(t, fixtureOpts{}).open()
	if err := h.RaiseFloor("b50000", "TEST-ADVISORY-1"); err != nil {
		t.Fatal(err)
	}
	old := newFixture(t, fixtureOpts{version: "b9000"})
	oh := old.open()

	if _, err := oh.EnsureRuntime(t.Context()); err == nil {
		t.Fatal("a build below the raised floor was installed")
	}
	if n := old.hits.Load(); n != 0 {
		t.Fatalf("%d requests reached the origin before the gate refused", n)
	}
	if _, err := oh.Serve(t.Context(), old.model.ID, localmodel.ServeOptions{}); err == nil {
		t.Fatal("Serve ran a build below the floor")
	}
	if n := old.hits.Load(); n != 0 {
		t.Fatalf("Serve reached the origin %d times before the gate refused", n)
	}

	// A build installed before an advisory is held to it afterwards. Fresh data
	// directory per fixture, so install by hand with a high build and check the same
	// directory refuses once the release set names a low one.
	cached := newFixture(t, fixtureOpts{version: "b60000"})
	ch := cached.open()
	if _, err := ch.EnsureRuntime(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := ch.RaiseFloor("b70000", "TEST-ADVISORY-2"); err != nil {
		t.Fatal(err)
	}
	before := cached.hits.Load()
	if _, err := ch.EnsureRuntime(t.Context()); err == nil {
		t.Fatal("cached build below a later advisory was reused")
	}
	if cached.hits.Load() != before {
		t.Fatal("gate refused only after touching the network")
	}
	if _, err := ch.Serve(t.Context(), cached.model.ID, localmodel.ServeOptions{}); err == nil {
		t.Fatal("Serve ran a cached build below the floor")
	}
}

// Refusal 3: a floor never goes down, and a build below the compiled-in floor is refused
// without any option to lower it.
func TestFloorNeverLowers(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	before := h.Floor()
	if before == "" {
		t.Fatal("the runtime has no floor")
	}
	if err := h.RaiseFloor("b1", "TEST-LOWER"); err != nil {
		t.Fatal(err)
	}
	if after := h.Floor(); after != before {
		t.Fatalf("floor moved from %s to %s after a lower raise", before, after)
	}
	if err := h.RaiseFloor("not a version", ""); err == nil {
		t.Fatal("a version with no number was accepted")
	}
	if h.Floor() != before {
		t.Fatal("floor changed after a rejected raise")
	}

	// A release set naming a build under the compiled-in floor is refused at install.
	low := newFixture(t, fixtureOpts{version: "b1000"})
	if _, err := low.open().EnsureRuntime(t.Context()); err == nil {
		t.Fatal("build below the compiled-in floor was installed")
	}
	if low.hits.Load() != 0 {
		t.Fatal("origin was contacted for a refused build")
	}

	// The API has no method that could lower the floor.
	for i := range reflect.TypeOf(h).NumMethod() {
		n := strings.ToLower(reflect.TypeOf(h).Method(i).Name)
		for _, bad := range []string{"lower", "setfloor", "resetfloor", "skipgate", "disablegate", "unsafe", "insecure"} {
			if strings.Contains(n, bad) {
				t.Errorf("Host.%s suggests a way around the gate", reflect.TypeOf(h).Method(i).Name)
			}
		}
	}
}

// Refusal 4: a chat template embedded in the weights never reaches the runtime.
func TestUntrustedChatTemplateNeverPassedThrough(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, ep := serveOnce(t, f)
	argv := argvOf(t, ep)
	if got, ok := flagValue(argv, "--chat-template"); !ok || got != "chatml" {
		t.Fatalf("--chat-template = %q (present=%v); want the catalog's chatml", got, ok)
	}
	joined := strings.Join(argv, " ")
	if strings.Contains(joined, "EVIL_TEMPLATE_MARKER") || strings.Contains(joined, "__class__") {
		t.Fatalf("the embedded template reached the command line: %s", joined)
	}
	for _, a := range argv {
		if a == "--jinja" {
			t.Fatal("the runtime was asked to render model-supplied jinja")
		}
	}
	if !ep.TemplateOverridden {
		t.Fatal("the override was not reported")
	}

	clean := newFixture(t, fixtureOpts{id: "test/clean", weights: ggufWithTemplate("")})
	_, ep2 := serveOnce(t, clean)
	if ep2.TemplateOverridden {
		t.Fatal("a model with no embedded template was reported as overridden")
	}

	// A catalog entry whose template is not one of the trusted names is rejected when the
	// host opens, so it can never be served.
	bad := newFixture(t, fixtureOpts{template: "{{ evil }}"})
	h, err := localmodel.Open(bad.config())
	if err == nil {
		_, err = h.Serve(t.Context(), bad.model.ID, localmodel.ServeOptions{})
		_ = h.Close()
	}
	if err == nil {
		t.Fatal("an unrecognized template was served")
	}
	if bad.hits.Load() > 2 {
		t.Fatalf("unexpected traffic: %d", bad.hits.Load())
	}
	missing := newFixture(t, fixtureOpts{})
	missing.model.ChatTemplate = ""
	if _, err := localmodel.Open(missing.config()); err == nil {
		t.Fatal("a local model with no template was accepted")
	}
}

// The digest is checked before the weights are parsed and again before launch.
func TestDigestCheckedBeforeParseAndBeforeLaunch(t *testing.T) {
	t.Run("corrupt download is refused before any parse", func(t *testing.T) {
		// The origin serves garbage that is not a GGUF; the digest mismatch must be what
		// the caller hears, not a parser complaint.
		f := newFixture(t, fixtureOpts{})
		f.weights = []byte("not a gguf at all, and not the pinned bytes")
		h := f.open()
		_, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		if err == nil {
			t.Fatal("served unverified weights")
		}
		if strings.Contains(err.Error(), "gguf") || strings.Contains(strings.ToLower(err.Error()), "magic") {
			t.Fatalf("the file was parsed before it was verified: %v", err)
		}
	})
	t.Run("tampered file on disk is replaced, not served", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		h := f.open()
		w, err := h.EnsureWeights(t.Context(), f.model.ID, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(w.Path, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(w.Path, []byte("tampered"), 0o600); err != nil {
			t.Fatal(err)
		}
		ep, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		_ = ep
		got, err := os.ReadFile(w.Path)
		if err != nil || !bytes.Equal(got, f.weights) {
			t.Fatalf("the tampered file was not replaced by the verified bytes (err=%v)", err)
		}
	})
	t.Run("runtime binary swapped after install", func(t *testing.T) {
		f := newFixture(t, fixtureOpts{})
		h := f.open()
		rt, err := h.EnsureRuntime(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(rt.BinPath, 0o700); err != nil {
			t.Fatal(err)
		}
		fh, err := os.OpenFile(rt.BinPath, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = fh.WriteString("backdoor")
		_ = fh.Close()
		_, err = h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
		if !errors.Is(err, localmodel.ErrDigestMismatch) {
			t.Fatalf("Serve = %v; want ErrDigestMismatch", err)
		}
		if st, _ := h.Status(t.Context()); len(st) != 0 {
			t.Fatalf("a server was started from a modified binary: %+v", st)
		}
	})
}

// Nothing is fetched on trust: a missing digest is refused, for weights and for runtimes.
func TestUnpinnedIsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{digest: "none"})
	h := f.open()
	if _, err := h.EnsureWeights(t.Context(), f.model.ID, ""); !errors.Is(err, localmodel.ErrUnpinned) {
		t.Fatalf("EnsureWeights = %v; want ErrUnpinned", err)
	}
	if _, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{}); !errors.Is(err, localmodel.ErrUnpinned) {
		t.Fatalf("Serve = %v; want ErrUnpinned", err)
	}
	if f.hits.Load() > 1 {
		t.Fatalf("weights were requested without a digest: %d hits", f.hits.Load())
	}

	g := newFixture(t, fixtureOpts{})
	g.release.SHA256 = ""
	if _, err := localmodel.Open(g.config()); !errors.Is(err, localmodel.ErrUnpinned) {
		t.Fatalf("Open with an unpinned release = %v; want ErrUnpinned", err)
	}
}

// A server that does not demand the token is not handed out.
func TestServerWithoutAuthIsStopped(t *testing.T) {
	f := newFixture(t, fixtureOpts{id: "test/noauth"})
	h := f.open()
	_, err := h.Serve(t.Context(), f.model.ID, localmodel.ServeOptions{})
	if !errors.Is(err, localmodel.ErrUnauthenticated) {
		t.Fatalf("Serve = %v; want ErrUnauthenticated", err)
	}
	if st, _ := h.Status(t.Context()); len(st) != 0 {
		t.Fatalf("the open server was left running: %+v", st)
	}
}

// The token reaches the runtime through a file, never the command line, and no one
// without it gets in.
func TestTokenIsPrivateToTheLaunch(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, ep := serveOnce(t, f)
	tok := ep.Token.Expose()
	if len(tok) < 32 {
		t.Fatalf("token is too short to resist guessing: %d", len(tok))
	}
	if strings.Contains(strings.Join(argvOf(t, ep), " "), tok) {
		t.Fatal("the token is on the command line")
	}
	for _, shown := range []string{ep.Token.String(), (&ep).Token.String()} {
		if strings.Contains(shown, tok) {
			t.Fatal("the token prints in the clear")
		}
	}
	for _, hdr := range []string{"", "Bearer wrong", "Bearer " + tok[:len(tok)-1]} {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, ep.BaseURL+"/chat/completions", strings.NewReader("{}"))
		if hdr != "" {
			req.Header.Set("Authorization", hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("credential %q got status %d", hdr, resp.StatusCode)
		}
	}
	// Two launches never share a token.
	g := newFixture(t, fixtureOpts{})
	_, ep2 := serveOnce(t, g)
	if ep2.Token.Expose() == tok {
		t.Fatal("two launches got the same token")
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(filepath.Join(f.dataDir, "run", "keys", "test_model.key"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("token file is readable by others: %v", info.Mode())
		}
	}
}

// Client carries the token for a server another host started, found from the run
// directory, and cannot be aimed off loopback.
func TestClientAttachesTokenAndStaysOnLoopback(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, ep := serveOnce(t, f)

	other, err := localmodel.Open(f.config())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close() })

	post := func(c *http.Client, url string) (int, error) {
		req, _ := http.NewRequestWithContext(t.Context(), http.MethodPost, url, strings.NewReader("{}"))
		resp, err := c.Do(req)
		if err != nil {
			return 0, err
		}
		_ = resp.Body.Close()
		return resp.StatusCode, nil
	}
	if code, err := post(other.Client(), ep.BaseURL+"/chat/completions"); err != nil || code != http.StatusOK {
		t.Fatalf("a second host's client should present the token: %d, %v", code, err)
	}
	if code, err := post(http.DefaultClient, ep.BaseURL+"/chat/completions"); err != nil || code != http.StatusUnauthorized {
		t.Fatalf("a plain client should be refused: %d, %v", code, err)
	}
	if _, err := post(other.Client(), "http://localhost:"+port(ep)+"/v1/chat/completions"); err == nil {
		t.Fatal("the client sent a request to a name other than 127.0.0.1")
	}
	if _, err := post(other.Client(), "https://example.test/v1/chat/completions"); err == nil {
		t.Fatal("the client sent a request off loopback")
	}
}

// Error text never carries a credential, whether it came from a URL or from a token.
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.model.Quants[0].URL = "https://user:hunter2@127.0.0.1:1/w.gguf?sig=SECRET-SIGNATURE"
	h, err := localmodel.Open(f.config())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	_, err = h.EnsureWeights(t.Context(), f.model.ID, "")
	if err == nil {
		t.Fatal("fetch from a dead address succeeded")
	}
	for _, leak := range []string{"hunter2", "SECRET-SIGNATURE", "user:"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("error leaks %q: %v", leak, err)
		}
	}

	a, err := h.Classify("https://user:hunter2@example.test/m.gguf?token=SECRET-SIGNATURE")
	if err == nil {
		for _, s := range append([]string{a.Ref, a.Reason, a.Refusal}, a.Risk...) {
			if strings.Contains(s, "hunter2") || strings.Contains(s, "SECRET-SIGNATURE") {
				t.Fatalf("assessment leaks a credential: %q", s)
			}
		}
	} else if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "SECRET-SIGNATURE") {
		t.Fatalf("classify error leaks a credential: %v", err)
	}
}

// Only catalog models serve; any other source is classified and then refused.
func TestNonCatalogSourcesAreNotServed(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	h := f.open()
	for _, ref := range []string{
		"https://example.test/model.gguf",
		"hf:someone/repo/model.gguf",
		filepath.Join(f.dataDir, "local.gguf"),
	} {
		a, err := h.Classify(ref)
		if err != nil {
			continue
		}
		if a.Catalog {
			t.Fatalf("%s classified as a catalog model", ref)
		}
		if _, err := h.Serve(t.Context(), ref, localmodel.ServeOptions{}); err == nil {
			t.Fatalf("%s was served", ref)
		} else if !errors.Is(err, localmodel.ErrNotAdmitted) && !errors.Is(err, localmodel.ErrNotServable) {
			t.Fatalf("%s: %v; want ErrNotAdmitted or ErrNotServable", ref, err)
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("a refused source caused downloads")
	}
	if _, err := h.Classify("pickled.pt"); err == nil {
		t.Fatal("a code-executing weight format was accepted")
	}
	a, err := h.Classify(f.model.ID)
	if err != nil || !a.Catalog || !a.Admitted {
		t.Fatalf("catalog ref = %+v, %v", a, err)
	}
}

func TestPickleWeightsRefused(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	f.model.Quants[0].Format = "pickle"
	h := f.open()
	if _, err := h.EnsureWeights(t.Context(), f.model.ID, ""); !errors.Is(err, localmodel.ErrNotServable) {
		t.Fatalf("EnsureWeights = %v; want ErrNotServable", err)
	}
	if f.hits.Load() != 0 {
		t.Fatal("pickle weights were downloaded")
	}
}

func TestEmbeddedCatalogAndFit(t *testing.T) {
	h, err := localmodel.Open(localmodel.Config{DataDir: filepath.Join(t.TempDir(), "d")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = h.Close() }()
	ms := h.Models(localmodel.ModelQuery{})
	if len(ms) == 0 {
		t.Fatal("the embedded catalog has no local models")
	}
	m, err := h.Model(ms[0].ID)
	if err != nil || m.ID != ms[0].ID {
		t.Fatalf("Model = %+v, %v", m, err)
	}
	if _, err := h.Model("no/such-model"); err == nil {
		t.Fatal("unknown model found")
	}
	fit, err := h.Fit(t.Context(), m.ID)
	if err != nil {
		t.Fatal(err)
	}
	switch fit {
	case localmodel.FitFeasible, localmodel.FitTight, localmodel.FitOverBudget, localmodel.FitUnknown:
	default:
		t.Fatalf("fit = %q", fit)
	}
	_ = h.Hardware(t.Context())
	// With no release set the built-in pins apply, and they are above their own floor.
	if h.Floor() == "" {
		t.Fatal("no floor for the pinned runtime")
	}
}

func TestOpenRejectsRelativeDataDir(t *testing.T) {
	if _, err := localmodel.Open(localmodel.Config{DataDir: "relative/dir"}); err == nil {
		t.Fatal("a relative data directory was accepted")
	}
}

var _ = io.Discard
