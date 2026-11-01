package localmodel_test

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ionalpha/flynn/localmodel"
)

// maliciousTemplate stands in for a chat template a hostile weights file carries. The
// fake runtime never renders it; the tests look for it in the argument vector.
const maliciousTemplate = "{{ ''.__class__.__mro__[1].__subclasses__() }} EVIL_TEMPLATE_MARKER"

// fakeRuntimeFlag is the argument that marks a re-executed test binary as the stand-in
// for llama-server. The sandbox scrubs the environment, so the marker is on the command
// line, where the real runtime's token flag is too.
const fakeRuntimeFlag = "--api-key-file"

// TestMain turns the test binary into a fake llama-server when the sandbox launches it
// with the runtime's flags.
func TestMain(m *testing.M) {
	for _, a := range os.Args[1:] {
		if a == fakeRuntimeFlag {
			runFakeRuntime(os.Args[1:])
			return
		}
	}
	os.Exit(m.Run())
}

// runFakeRuntime serves what the facade expects of llama-server: an unauthenticated
// /v1/models, bearer-protected completions, and a /v1/_argv endpoint that echoes the
// command line so tests can inspect it. A model whose file name contains "noauth"
// skips the token check, to prove the facade refuses a server that does not enforce it.
func runFakeRuntime(args []string) {
	flags := map[string]string{}
	for i := 0; i+1 < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			flags[args[i]] = args[i+1]
		}
	}
	// The key file holds one key per line, and the server accepts any of them.
	var keys []string
	if b, err := os.ReadFile(flags[fakeRuntimeFlag]); err == nil {
		keys = strings.Fields(string(b))
	}
	noAuth := strings.Contains(flags["--model"], "noauth")
	authed := func(r *http.Request) bool {
		if noAuth {
			return true
		}
		for _, k := range keys {
			if r.Header.Get("Authorization") == "Bearer "+k {
				return true
			}
		}
		return false
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"data":[]}`) })
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	mux.HandleFunc("/v1/_argv", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(args)
	})
	mux.HandleFunc("/v1/_env", func(w http.ResponseWriter, r *http.Request) {
		if !authed(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(os.Environ())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "") })
	ln, err := net.Listen("tcp", net.JoinHostPort(flags["--host"], flags["--port"]))
	if err != nil {
		// Lost the bind race. Staying up a moment keeps the launch from seeing the exit
		// before it has asked the listener who it is.
		time.Sleep(3 * time.Second)
		os.Exit(3)
	}
	_ = http.Serve(ln, mux)
}

// ggufWithTemplate encodes a minimal GGUF header carrying one string metadata entry, and
// a payload so the file has a body to hash.
func ggufWithTemplate(template string) []byte {
	var b bytes.Buffer
	w32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w64 := func(v uint64) { _ = binary.Write(&b, binary.LittleEndian, v) }
	wstr := func(s string) {
		w64(uint64(len(s)))
		b.WriteString(s)
	}
	w32(0x46554747)
	w32(3)
	w64(0)
	if template == "" {
		w64(0)
	} else {
		w64(1)
		wstr("tokenizer.chat_template")
		w32(8)
		wstr(template)
	}
	b.WriteString(strings.Repeat("weights", 64))
	return b.Bytes()
}

func sum(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// fixture is a TLS origin serving a runtime archive and one weights file, a catalog and a
// release set that pin them, and a count of the requests the origin saw.
type fixture struct {
	t       *testing.T
	srv     *httptest.Server
	hits    atomic.Int64
	weights []byte
	archive []byte
	binName string
	model   localmodel.Model
	release localmodel.Release
	dataDir string
}

type fixtureOpts struct {
	id       string
	version  string
	weights  []byte
	digest   string // overrides the catalog digest; "none" omits it
	template string // the catalog's chat template; empty means chatml
}

func newFixture(t *testing.T, o fixtureOpts) *fixture {
	t.Helper()
	if o.id == "" {
		o.id = "test/model"
	}
	if o.version == "" {
		o.version = "b99999"
	}
	if o.weights == nil {
		o.weights = ggufWithTemplate(maliciousTemplate)
	}
	if o.template == "" {
		o.template = "chatml"
	}
	f := &fixture{t: t, weights: o.weights, dataDir: filepath.Join(t.TempDir(), "data")}
	f.binName = "llama-server"
	if runtime.GOOS == "windows" {
		f.binName += ".exe"
	}
	f.archive = runtimeArchive(t, f.binName)

	mux := http.NewServeMux()
	mux.HandleFunc("/runtime.tar.gz", func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write(f.archive)
	})
	mux.HandleFunc("/weights.gguf", func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write(f.weights)
	})
	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)

	digest := "sha256:" + sum(o.weights)
	switch o.digest {
	case "":
	case "none":
		digest = ""
	default:
		digest = o.digest
	}
	f.model = localmodel.Model{
		ID: o.id, Name: "Test model", License: "mit", Kind: "local",
		Trust: localmodel.TrustBlessed, ChatTemplate: o.template,
		Quants: []localmodel.Quant{{
			Name: "Q4_K_M", Format: "gguf", SizeBytes: int64(len(o.weights)),
			URL: f.srv.URL + "/weights.gguf?sig=SECRET-SIGNATURE", Digest: digest,
		}},
	}
	f.model.Source.Publisher = "tester"
	f.model.Source.URL = "https://example.test/model"
	f.release = localmodel.Release{
		Runtime: localmodel.Runtime, Version: o.version, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		URL: f.srv.URL + "/runtime.tar.gz", SHA256: sum(f.archive), SizeBytes: int64(len(f.archive)), BinName: f.binName,
	}
	return f
}

func (f *fixture) config() localmodel.Config {
	// The jail is accepted so the tests run where the platform cannot confine a process
	// further; the tests that exercise the refusal ask for the kernel level themselves.
	return localmodel.WithReleases(localmodel.Config{
		DataDir:        f.dataDir,
		Catalog:        []localmodel.Model{f.model},
		HTTPClient:     f.srv.Client(),
		MinContainment: "process",
	}, f.release)
}

func (f *fixture) open() *localmodel.Host {
	f.t.Helper()
	h, err := localmodel.Open(f.config())
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() {
		recs, _ := h.Status(f.t.Context())
		for _, r := range recs {
			_, _ = h.Stop(r.ModelID)
		}
		_ = h.Close()
	})
	return h
}

var (
	archiveOnce sync.Once
	archiveData []byte
	errArchive  error
)

// runtimeArchive is the test binary packed as the runtime archive. The binary is large, so
// it is read and compressed once per process; every fixture serves the same bytes.
func runtimeArchive(t *testing.T, name string) []byte {
	t.Helper()
	archiveOnce.Do(func() {
		var self string
		if self, errArchive = os.Executable(); errArchive != nil {
			return
		}
		var body []byte
		if body, errArchive = os.ReadFile(self); errArchive != nil {
			return
		}
		archiveData = tarGz(t, name, body)
	})
	if errArchive != nil {
		t.Fatal(errArchive)
	}
	return archiveData
}

func tarGz(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// argvOf asks a running fake server for its command line.
func argvOf(t *testing.T, ep localmodel.Endpoint) []string {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ep.BaseURL+"/_argv", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+ep.Token.Expose())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var argv []string
	if err := json.NewDecoder(resp.Body).Decode(&argv); err != nil {
		t.Fatal(err)
	}
	return argv
}

func flagValue(argv []string, name string) (string, bool) {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == name {
			return argv[i+1], true
		}
	}
	return "", false
}

func port(ep localmodel.Endpoint) string { return strconv.Itoa(ep.Port) }
