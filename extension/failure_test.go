package extension

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/acquire"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/resource"
)

// TestDecodeAndEncodeRefuseMalformedContent checks the typed accessors report a resource
// they cannot read rather than handing back a zero spec that would look like a bare
// extension.
func TestDecodeAndEncodeRefuseMalformedContent(t *testing.T) {
	bad := resource.Resource{
		Spec:   json.RawMessage(`{"surfaces": 7}`),
		Status: json.RawMessage(`{"observedGeneration": "not a number"}`),
	}
	if _, err := DecodeSpec(bad); err == nil {
		t.Error("a spec whose surfaces are not an object must not decode")
	}
	if _, err := DecodeStatus(bad); err == nil {
		t.Error("a status whose generation is not a number must not decode")
	}

	// A surface block that is not valid JSON cannot be rendered back out, so encoding
	// reports it rather than writing a spec the store would refuse.
	spec := Spec{Surfaces: map[string]json.RawMessage{SurfaceProcess: json.RawMessage(`{invalid`)}}
	if _, err := spec.Encode(); err == nil {
		t.Error("a spec carrying an unrenderable surface block must not encode")
	}
	raw, err := (Status{Grade: GradeSchemaValid}).Encode()
	if err != nil {
		t.Fatalf("Status.Encode: %v", err)
	}
	st, err := DecodeStatus(resource.Resource{Status: raw})
	if err != nil {
		t.Fatalf("DecodeStatus: %v", err)
	}
	if st.Grade != GradeSchemaValid {
		t.Errorf("round-tripped grade = %q, want %q", st.Grade, GradeSchemaValid)
	}
}

// TestSpecAndStatusRoundTrip checks the ordinary path the refusals above sit next to.
func TestSpecAndStatusRoundTrip(t *testing.T) {
	spec := Spec{Surfaces: map[string]json.RawMessage{SurfaceProcess: json.RawMessage(`{"dev":{"path":"/x"}}`)}}
	raw, err := spec.Encode()
	if err != nil {
		t.Fatalf("Spec.Encode: %v", err)
	}
	got, err := DecodeSpec(resource.Resource{Spec: raw})
	if err != nil {
		t.Fatalf("DecodeSpec: %v", err)
	}
	if _, ok := got.Surface(SurfaceProcess); !ok {
		t.Error("the round-tripped spec lost its process surface")
	}
	if _, ok := got.Surface("nosuch"); ok {
		t.Error("Surface reported a key the spec does not declare")
	}
}

// TestFetcherOptionsAreApplied checks the fetcher's knobs install a value and ignore an
// empty one, so a zero option cannot silently disable a bound.
func TestFetcherOptionsAreApplied(t *testing.T) {
	f, err := NewHTTPHostFetcher(
		"https://example.com/rpc",
		WithFetchTimeout(3*time.Second),
		WithFetchContentType("application/x-custom"),
		WithMaxResponseBytes(1024),
	)
	if err != nil {
		t.Fatalf("NewHTTPHostFetcher: %v", err)
	}
	if f.contentType != "application/x-custom" {
		t.Errorf("content type = %q, want the override", f.contentType)
	}
	if f.client.Timeout != 3*time.Second {
		t.Errorf("timeout = %v, want the override", f.client.Timeout)
	}
	if f.maxResponse != 1024 {
		t.Errorf("max response = %d, want the override", f.maxResponse)
	}

	// A non-positive or empty value is ignored: the default stands rather than being
	// replaced with "no bound at all".
	d, err := NewHTTPHostFetcher("https://example.com/rpc",
		WithFetchTimeout(0), WithFetchContentType(""), WithMaxResponseBytes(0))
	if err != nil {
		t.Fatalf("NewHTTPHostFetcher: %v", err)
	}
	if d.contentType != "application/json" || d.client.Timeout != 30*time.Second || d.maxResponse != 1<<20 {
		t.Errorf("an empty option overwrote a default: type=%q timeout=%v max=%d",
			d.contentType, d.client.Timeout, d.maxResponse)
	}
}

// TestFetchReportsWhatItCannotDeliver checks each failure the host fetcher can hit: a
// transport that never lands, a body over the cap, and a non-2xx status. None of them may
// return a body: an endpoint's error page must not become a channel into the extension.
func TestFetchReportsWhatItCannotDeliver(t *testing.T) {
	t.Run("transport failure", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		url := srv.URL
		srv.Close() // nothing is listening any more

		f, err := NewHTTPHostFetcher(url, WithPrivateEndpoint())
		if err != nil {
			t.Fatalf("NewHTTPHostFetcher: %v", err)
		}
		if _, err := f.Fetch(context.Background(), []byte(`{}`)); err == nil {
			t.Fatal("a fetch to a dead endpoint must fail")
		}
	})

	t.Run("over-sized response", func(t *testing.T) {
		f := serveFetcher(t, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("x", 64)))
		}, WithMaxResponseBytes(8))
		_, err := f.Fetch(context.Background(), []byte(`{}`))
		if err == nil || !strings.Contains(err.Error(), "exceeded") {
			t.Fatalf("Fetch error = %v, want an over-size refusal", err)
		}
	})
}

// serveFetcher stands a fake endpoint up on loopback and points a fetcher at it. Loopback
// needs the explicit private-endpoint opt-in, which is the same decision an operator makes
// for a local node.
func serveFetcher(t *testing.T, h http.HandlerFunc, opts ...FetcherOption) *HTTPHostFetcher {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	f, err := NewHTTPHostFetcher(srv.URL, append([]FetcherOption{WithPrivateEndpoint()}, opts...)...)
	if err != nil {
		t.Fatalf("NewHTTPHostFetcher: %v", err)
	}
	return f
}

// TestProcessHandlerRefusesAnUnusableMount checks the load path fails closed on every input
// it cannot act on, and that nothing is mounted afterwards.
func TestProcessHandlerRefusesAnUnusableMount(t *testing.T) {
	m := Mount{ID: "ext-1", Name: "token", Surface: SurfaceProcess, Block: json.RawMessage(`{}`)}

	t.Run("no launcher or resolver", func(t *testing.T) {
		h := NewProcessHandler(nil, nil)
		if err := h.OnLoad(context.Background(), m); err == nil {
			t.Fatal("a handler with no launcher or resolver must refuse to load")
		}
	})

	t.Run("block is not JSON", func(t *testing.T) {
		h := NewProcessHandler(&fakeLauncher{conn: newFakeConn(echoStub())}, okResolver{})
		bad := m
		bad.Block = json.RawMessage(`{not json`)
		if err := h.OnLoad(context.Background(), bad); err == nil {
			t.Fatal("a process block that is not JSON must refuse to load")
		}
		if h.Tools(m.ID) != nil {
			t.Error("a refused load left tools mounted")
		}
	})

	t.Run("the binary cannot be resolved", func(t *testing.T) {
		h := NewProcessHandler(&fakeLauncher{conn: newFakeConn(echoStub())},
			errResolver{err: errors.New("no such release")})
		if err := h.OnLoad(context.Background(), m); err == nil {
			t.Fatal("an unresolvable binary must refuse to load")
		}
	})

	t.Run("the launch fails", func(t *testing.T) {
		h := NewProcessHandler(&fakeLauncher{launchErr: errors.New("sandbox unavailable")}, okResolver{})
		if err := h.OnLoad(context.Background(), m); err == nil {
			t.Fatal("a launch failure must refuse to load")
		}
	})
}

// errResolver fails every resolution, standing in for a release that cannot be verified.
type errResolver struct{ err error }

func (r errResolver) Resolve(context.Context, string, ProcessBlock) (string, []string, error) {
	return "", nil, r.err
}

// TestProcessHandlerSurfaceAndOptions checks the handler names the surface it serves and
// that the dial-timeout knob installs a value and ignores a non-positive one.
func TestProcessHandlerSurfaceAndOptions(t *testing.T) {
	h := NewProcessHandler(nil, nil, WithDialTimeout(2*time.Second))
	if h.Capability() != SurfaceProcess {
		t.Errorf("Capability = %q, want %q", h.Capability(), SurfaceProcess)
	}
	if h.dialTimeout != 2*time.Second {
		t.Errorf("dial timeout = %v, want the override", h.dialTimeout)
	}
	def := NewProcessHandler(nil, nil, WithDialTimeout(0))
	if def.dialTimeout != 30*time.Second {
		t.Errorf("dial timeout = %v, want the default to stand", def.dialTimeout)
	}
	if got := h.Tools("never-mounted"); got != nil {
		t.Errorf("Tools of an unmounted id = %v, want nil", got)
	}
}

// TestHostCallsAreRefusedWhenUngrantedOrMalformed checks the borrowed-authority path: a
// tool that asks for an authority it was not granted, hands over something that is not a
// payload, or asks for two authorities in one message is refused, and the refusal reaches
// the caller as an error rather than a result.
func TestHostCallsAreRefusedWhenUngrantedOrMalformed(t *testing.T) {
	signer := testSigner(t)
	signMsg := `{"session":"s1","sign":{"message":"` + base64.StdEncoding.EncodeToString([]byte("payload")) + `"}}`

	cases := []struct {
		name    string
		message string
		opts    []ProcessOption
		want    string
	}{
		{
			name:    "signing without a granted key",
			message: signMsg,
			want:    "granted no key",
		},
		{
			// The host holds no parser, so a signer that does not judge its own payloads
			// leaves NOBODY judging them. That is blind signing, and it is refused rather
			// than warned about.
			name:    "a signer that does not judge what it signs",
			message: signMsg,
			opts: []ProcessOption{
				WithHostSigner(func(string, string) HostSigner { return blindSigner{pub: signer.Public()} }),
			},
			want: "does not judge what it signs",
		},
		{
			name:    "the signing payload is not base64",
			message: `{"session":"s1","sign":{"message":"!!!not base64!!!"}}`,
			opts: []ProcessOption{
				WithHostSigner(func(string, string) HostSigner { return signer }),
			},
			want: "not base64",
		},
		{
			name:    "the signing payload is over-sized",
			message: `{"session":"s1","sign":{"message":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", (64<<10)+1))) + `"}}`,
			opts: []ProcessOption{
				WithHostSigner(func(string, string) HostSigner { return signer }),
			},
			want: "over-sized payload",
		},
		{
			name:    "the fetch body is not base64",
			message: `{"session":"s1","fetch":{"body":"!!!not base64!!!"}}`,
			opts:    []ProcessOption{WithHostFetcher(func(string, string) HostFetcher { return nullFetcher{} })},
			want:    "not base64",
		},
		{
			name:    "the fetch body is over-sized",
			message: `{"session":"s1","fetch":{"body":"` + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", (256<<10)+1))) + `"}}`,
			opts: []ProcessOption{
				WithHostFetcher(func(string, string) HostFetcher { return nullFetcher{} }),
			},
			want: "over-sized request",
		},
		{
			name:    "signing and fetching in one message",
			message: `{"session":"s1","sign":{"message":"aGk"},"fetch":{"body":"aGk"}}`,
			opts: []ProcessOption{
				WithHostSigner(func(string, string) HostSigner { return signer }),
				WithHostFetcher(func(string, string) HostFetcher { return nullFetcher{} }),
			},
			want: "sign and fetch in one message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := stubTool{
				name: "op", desc: "d",
				invoke: func(context.Context, json.RawMessage) (string, error) { return tc.message, nil },
			}
			h, _, m := mountStub(t, []mission.Tool{stub}, tc.opts...)
			_, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("invoke error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// nullFetcher answers every fetch with a fixed body, so a fetch that is refused before it is
// sent is distinguishable from one that goes out.
type nullFetcher struct{}

func (nullFetcher) Fetch(context.Context, []byte) ([]byte, error) { return []byte(`{"ok":true}`), nil }

// TestIntersectHostsIsTheNoEscalationRule checks the egress intersection: an empty grant
// permits nothing, a spec can only narrow the grant, blanks and duplicates are dropped, and
// the comparison ignores case and surrounding space.
func TestIntersectHostsIsTheNoEscalationRule(t *testing.T) {
	cases := []struct {
		name      string
		requested []string
		grant     []string
		want      []string
	}{
		{"no grant permits nothing", []string{"api.example.com"}, nil, nil},
		{"no request asks for nothing", nil, []string{"api.example.com"}, nil},
		{"a spec cannot widen the grant", []string{"evil.example.com"}, []string{"api.example.com"}, nil},
		{
			"case and space are not part of a host name",
			[]string{" API.Example.com "},
			[]string{"api.example.com"},
			[]string{" API.Example.com "},
		},
		{
			"blanks and duplicates are dropped",
			[]string{"a.example.com", "", "a.example.com", "b.example.com"},
			[]string{"a.example.com", "b.example.com"},
			[]string{"a.example.com", "b.example.com"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := intersectHosts(tc.requested, tc.grant)
			if len(got) != len(tc.want) {
				t.Fatalf("intersectHosts = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("intersectHosts = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// TestBoundTextFallsBackToADefaultLimit checks a non-positive limit is a default rather than
// "no bound", so an untrusted result can never be unbounded by a misconfigured cap.
func TestBoundTextFallsBackToADefaultLimit(t *testing.T) {
	long := strings.Repeat("a", (64<<10)+100)
	got := boundText(long, 0)
	if !strings.HasPrefix(got, strings.Repeat("a", 64<<10)) {
		t.Error("boundText with no limit did not keep the first 64 KiB")
	}
	if !strings.Contains(got, "truncated 100 bytes") {
		t.Errorf("boundText with no limit did not fall back to the default cap: %q", got[len(got)-40:])
	}
	if strings.Contains(boundText("a\x00b\x07c", 100), "\x00") {
		t.Error("control characters must be stripped from untrusted text")
	}
}

// TestInjectHostKeyRefusesAnInputThatIsNotAnObject checks the key injection: a caller's
// argument that is not a JSON object cannot carry the host key, and is refused rather than
// silently replaced with one that can.
func TestInjectHostKeyRefusesAnInputThatIsNotAnObject(t *testing.T) {
	pub := testSigner(t).Public()
	if _, err := injectHostKey(json.RawMessage(`["a"]`), pub); err == nil {
		t.Fatal("a non-object input must be refused")
	}
	// A null or empty input starts from an empty object rather than panicking on the
	// assignment.
	for _, in := range []json.RawMessage{nil, json.RawMessage(` null `), json.RawMessage(`{}`)} {
		out, err := injectHostKey(in, pub)
		if err != nil {
			t.Fatalf("injectHostKey(%q): %v", in, err)
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(out, &obj); err != nil {
			t.Fatalf("injected input is not an object: %v", err)
		}
		if _, ok := obj[hostKeyField]; !ok {
			t.Errorf("injectHostKey(%q) did not inject the key", in)
		}
	}
}

// TestSandboxLaunchRefusesWhatItCannotConfine checks the sandbox launcher fails before it
// starts anything when there is no binary to run, or nowhere to put its scratch directory.
func TestSandboxLaunchRefusesWhatItCannotConfine(t *testing.T) {
	l := NewSandboxLauncher(t.TempDir())
	if _, err := l.Launch(context.Background(), LaunchRequest{}); err == nil {
		t.Fatal("a launch with no binary path must be refused")
	}

	// A work root that is a file, not a directory: the scratch directory cannot be made.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	blocked := NewSandboxLauncher(file)
	if _, err := blocked.Launch(context.Background(), LaunchRequest{Path: "/verified/bin"}); err == nil {
		t.Fatal("a launch with no usable scratch root must be refused")
	}
}

// TestDevResolverRefusesAPathItCannotRun checks the dev path is checked before it is run: a
// path that is not there, and one that is a directory, are both refused.
func TestDevResolverRefusesAPathItCannotRun(t *testing.T) {
	dir := t.TempDir()
	r := DevResolver{Enabled: true}

	absent := filepath.Join(dir, "nope")
	if _, _, err := r.Resolve(context.Background(), "token",
		ProcessBlock{Dev: &DevSource{Path: absent}}); err == nil {
		t.Error("a dev path that is not there must be refused")
	}
	if _, _, err := r.Resolve(context.Background(), "token",
		ProcessBlock{Dev: &DevSource{Path: dir}}); err == nil {
		t.Error("a dev path that is a directory must be refused")
	}
	if _, _, err := r.Resolve(context.Background(), "token", ProcessBlock{}); err == nil {
		t.Error("a block with no dev source must be refused")
	}
}

// TestSourceResolverPrefersTheSignedRelease checks the routing rule: a release always wins
// over a dev block, so a stray dev source cannot substitute an unsigned local binary for the
// signed release the operator asked for. A block declaring neither is refused.
func TestSourceResolverPrefersTheSignedRelease(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // a test fixture binary
		t.Fatalf("write dev binary: %v", err)
	}
	s := SourceResolver{
		Release: ReleaseResolver{}, // unconfigured: it refuses, which is what the test reads
		Dev:     DevResolver{Enabled: true},
	}

	// Both declared: the release resolver is the one consulted, and it refuses because no
	// origin is pinned. The dev binary is never reached.
	_, _, err := s.Resolve(context.Background(), "token", ProcessBlock{
		Release: &ReleaseSource{Asset: "token", Version: "v0.1.0"},
		Dev:     &DevSource{Path: bin},
	})
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatalf("resolve error = %v, want the release resolver's refusal", err)
	}

	// Dev alone: routed to the dev resolver.
	path, _, err := s.Resolve(context.Background(), "token", ProcessBlock{Dev: &DevSource{Path: bin}})
	if err != nil {
		t.Fatalf("resolve a dev source: %v", err)
	}
	if path != bin {
		t.Errorf("resolved path = %q, want the dev binary", path)
	}

	// Neither: there is nothing to run.
	if _, _, err := s.Resolve(context.Background(), "token", ProcessBlock{}); err == nil ||
		!strings.Contains(err.Error(), "neither") {
		t.Fatalf("resolve error = %v, want a no-source refusal", err)
	}
}

// TestReleaseResolverRefusesAnIncompleteRequest checks each precondition of a verified
// install is checked before anything is downloaded.
func TestReleaseResolverRefusesAnIncompleteRequest(t *testing.T) {
	rel := buildRelease(t, "binary body")
	r, _ := rel.serve(t)

	cases := []struct {
		name  string
		mut   func(*ReleaseResolver)
		block ProcessBlock
		want  string
	}{
		{"no release source", nil, ProcessBlock{Dev: &DevSource{Path: "/x"}}, "declares no released source"},
		{
			name:  "no version",
			block: ProcessBlock{Release: &ReleaseSource{Asset: "token"}},
			want:  "asset and a version",
		},
		{
			name:  "no downloader",
			mut:   func(r *ReleaseResolver) { r.Downloader = nil },
			block: releaseBlock(),
			want:  "no downloader",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := r
			if tc.mut != nil {
				tc.mut(&res)
			}
			_, _, err := res.Resolve(context.Background(), "token", tc.block)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Resolve error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// TestReleaseResolverReportsAMissingArchive checks a release whose metadata verifies but
// whose archive is not served is an install failure, not a silent success.
func TestReleaseResolverReportsAMissingArchive(t *testing.T) {
	rel := buildRelease(t, "binary body")
	delete(rel.assets, "token_v0.1.0_linux_amd64.tar.gz")
	r, _ := rel.serve(t)

	if _, _, err := r.Resolve(context.Background(), "token", releaseBlock()); err == nil {
		t.Fatal("an archive that is not served must fail the install")
	}
}

// TestPlatformDefaultsToTheHost checks the resolver's platform falls back to the running
// host when it is not pinned, and that the archive and binary naming follow the platform.
func TestPlatformDefaultsToTheHost(t *testing.T) {
	got := ReleaseResolver{}.platform()
	if got.goos != runtime.GOOS || got.goarch != runtime.GOARCH {
		t.Errorf("platform = %+v, want the host's %s/%s", got, runtime.GOOS, runtime.GOARCH)
	}
	pinned := ReleaseResolver{GOOS: "windows", GOARCH: "arm64"}.platform()
	if pinned.goos != "windows" || pinned.goarch != "arm64" {
		t.Errorf("platform = %+v, want the pinned one", pinned)
	}

	win := target{goos: "windows", goarch: "amd64"}
	if win.binaryName("token") != "token.exe" {
		t.Errorf("windows binary name = %q, want token.exe", win.binaryName("token"))
	}
	if win.archiveKind() != acquire.ArchiveZip {
		t.Error("a windows release is a zip")
	}
	if name := archiveName("token", "v0.1.0", win); name != "token_v0.1.0_windows_amd64.zip" {
		t.Errorf("archive name = %q, want the published contract", name)
	}

	nix := target{goos: "linux", goarch: "amd64"}
	if nix.binaryName("token") != "token" {
		t.Errorf("linux binary name = %q, want token", nix.binaryName("token"))
	}
	if nix.archiveKind() != acquire.ArchiveTarGz {
		t.Error("a non-windows release is a tar.gz")
	}
}

// TestHashFileReportsAMissingFile checks the receipt hash refuses to pass over a binary it
// cannot read, which is what would let an unverified file be recorded as verified.
func TestHashFileReportsAMissingFile(t *testing.T) {
	if _, err := hashFile(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("hashing a file that is not there must fail")
	}
}

// TestJoinSortedNamesAnEmptyRegistry checks the error text a resolve against an empty
// registry produces names the absence rather than trailing off.
func TestJoinSortedNamesAnEmptyRegistry(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Resolve(SurfaceProcess)
	if err == nil || !strings.Contains(err.Error(), "(none)") {
		t.Fatalf("Resolve error = %v, want it to report that nothing is registered", err)
	}
}

// TestUnloadReportsAFailingSurface checks an unmount collects the failures of the surfaces
// it tore down rather than reporting a clean unload, and still records the extension as
// gone: an unload always makes progress.
func TestUnloadReportsAFailingSurface(t *testing.T) {
	reg := NewRegistry()
	tool := &recordHandler{capability: SurfaceTool, unloadErr: errors.New("surface stuck")}
	if err := reg.Register(tool); err != nil {
		t.Fatalf("register: %v", err)
	}
	l := NewLoader(reg)
	if _, err := l.Load(context.Background(), res("ext-1", "x", map[string]json.RawMessage{
		SurfaceTool: block(),
	})); err != nil {
		t.Fatalf("load: %v", err)
	}

	err := l.Unload(context.Background(), "ext-1")
	if err == nil || !strings.Contains(err.Error(), "surface stuck") {
		t.Fatalf("Unload error = %v, want the surface's failure", err)
	}
	if got := l.Mounted("ext-1"); len(got) != 0 {
		t.Errorf("Mounted after unload = %v, want none", got)
	}
	if tool.unloadCount() != 1 {
		t.Errorf("OnUnload calls = %d, want 1", tool.unloadCount())
	}
}

// TestLoadRefusesAResourceItCannotRead checks the loader refuses a resource with no id and
// one whose spec will not decode, so a half-read extension is never mounted.
func TestLoadRefusesAResourceItCannotRead(t *testing.T) {
	l := NewLoader(NewRegistry())
	if _, err := l.Load(context.Background(), resource.Resource{APIVersion: GroupVersion, Kind: Kind}); err == nil {
		t.Error("a resource with no id must not load")
	}
	bad := resource.Resource{APIVersion: GroupVersion, Kind: Kind, ID: "ext-1", Spec: json.RawMessage(`{"surfaces": 7}`)}
	if _, err := l.Load(context.Background(), bad); err == nil {
		t.Error("a resource whose spec will not decode must not load")
	}
}

// TestAToolErrorResultIsReturnedNotRaised checks an extension reporting a tool error hands
// the message back to the caller as a bounded result, so the model can read what went wrong
// instead of the run failing on the extension's word.
func TestAToolErrorResultIsReturnedNotRaised(t *testing.T) {
	stub := stubTool{
		name: "op", desc: "d",
		invoke: func(context.Context, json.RawMessage) (string, error) {
			return "", errors.New("the mint refused")
		},
	}
	h, _, m := mountStub(t, []mission.Tool{stub})
	out, err := h.Tools(m.ID)[0].Invoke(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(out, "extension tool error") || !strings.Contains(out, "the mint refused") {
		t.Errorf("result = %q, want the extension's error reported as a result", out)
	}
}

// TestDigestForSkipsWhatIsNotAChecksumLine checks the manifest reader ignores a line that is
// not a checksum, and reports no digest for an archive the signed file does not cover.
func TestDigestForSkipsWhatIsNotAChecksumLine(t *testing.T) {
	checksums := []byte("# a comment\n\nabc123  token_v0.1.0_linux_amd64.tar.gz\n")
	digest, ok := digestFor(checksums, "token_v0.1.0_linux_amd64.tar.gz")
	if !ok || digest != "abc123" {
		t.Errorf("digestFor = %q, %v, want abc123", digest, ok)
	}
	if _, ok := digestFor(checksums, "other.zip"); ok {
		t.Error("an archive the manifest does not cover must have no digest")
	}
}

// TestReleaseResolverReportsMissingMetadata checks a release whose signed checksum file is
// not served is refused: the digest can only come from a file the pinned identity signed, so
// there is nothing to fall back to.
func TestReleaseResolverReportsMissingMetadata(t *testing.T) {
	rel := buildRelease(t, "binary body")
	delete(rel.assets, "checksums.txt")
	r, _ := rel.serve(t)

	_, _, err := r.Resolve(context.Background(), "token", releaseBlock())
	if err == nil || !strings.Contains(err.Error(), "metadata") {
		t.Fatalf("Resolve error = %v, want a metadata failure", err)
	}
}

// TestACacheFromAnotherOriginIsNotReused checks the receipt is re-proven against the pinned
// origin: an install made under a different pinned workflow is not trusted, it is verified
// again.
func TestACacheFromAnotherOriginIsNotReused(t *testing.T) {
	rel := buildRelease(t, "binary body")
	r, _ := rel.serve(t)

	if _, _, err := r.Resolve(context.Background(), "token", releaseBlock()); err != nil {
		t.Fatalf("first install: %v", err)
	}
	before := rel.hits["checksums.txt"]

	// The same on-disk install, now under a different pinned identity: the receipt proves
	// nothing about this origin, so the release is downloaded and verified again.
	moved := r
	moved.Origin.Identity.Workflow = testWorkflow + "-other"
	_, _, err := moved.Resolve(context.Background(), "token", releaseBlock())
	if err == nil {
		t.Fatal("an install proven under another origin must not be reused")
	}
	if rel.hits["checksums.txt"] <= before {
		t.Error("the release was not re-verified against the new origin")
	}
}
