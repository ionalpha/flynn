package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// validDigest is a well-formed sha256 image digest for building test specs.
const validDigest = "sha256:" +
	"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"

// testWeightsPath is an absolute host path for a mount, resolved for the running OS so the
// guarantee validation (which requires an absolute host path) passes on Windows and Linux.
func testWeightsPath() string {
	p, _ := filepath.Abs(filepath.Join("testdata", "weights"))
	return p
}

// servingSpec builds a valid, publishing container spec from the Untrusted constructor,
// the shape a GPU inference server runs under.
func servingSpec() ContainerSpec {
	g := Untrusted(Limits{MemMiB: 4096, VCPUs: 2, PIDs: 512},
		Mount{HostPath: testWeightsPath(), GuestPath: "/weights"})
	g.Env = map[string]string{"MODEL": "m", "API_PORT": "8000"}
	return ContainerSpec{
		Image:         ContainerImage{Ref: "vllm/vllm-openai:v1", Digest: validDigest},
		Guarantees:    g,
		GPU:           GPURequest{Enabled: true, Device: "0"},
		Network:       "flynn-egress",
		HostPort:      8123,
		ContainerPort: 8000,
	}
}

func argvHasPair(argv []string, flag, want string) bool {
	for i := range len(argv) - 1 {
		if argv[i] == flag && argv[i+1] == want {
			return true
		}
	}
	return false
}

func argvHas(argv []string, want string) bool {
	for _, a := range argv {
		if a == want {
			return true
		}
	}
	return false
}

func TestBuildContainerArgvHardenedServingCommand(t *testing.T) {
	argv := buildContainerArgv(EngineDocker, servingSpec())
	if argv[0] != "docker" || argv[1] != "run" {
		t.Fatalf("argv must start with the engine run, got %v", argv[:2])
	}
	for _, want := range []string{"--rm", "--detach", "--read-only", "--cap-drop", "ALL", "--security-opt", "no-new-privileges"} {
		if !argvHas(argv, want) {
			t.Fatalf("argv missing hardening token %q: %v", want, argv)
		}
	}
	for _, p := range [][2]string{
		{"--memory", "4096m"},
		{"--cpus", "2"},
		{"--pids-limit", "512"},
		{"--network", "flynn-egress"},
		{"--gpus", "device=0"},
		{"--tmpfs", "/tmp"},
		{"--publish", "127.0.0.1:8123:8000"},
	} {
		if !argvHasPair(argv, p[0], p[1]) {
			t.Fatalf("argv missing %s %s: %v", p[0], p[1], argv)
		}
	}
	if !argvHasPair(argv, "--mount", "type=bind,source="+testWeightsPath()+",target=/weights,readonly") {
		t.Fatalf("weights mount must be a read-only bind: %v", argv)
	}
	// Env passed explicitly, in sorted order, never inheriting the host environment.
	if !argvHasPair(argv, "--env", "API_PORT=8000") || !argvHasPair(argv, "--env", "MODEL=m") {
		t.Fatalf("env not passed explicitly: %v", argv)
	}
	// The image is referenced by its pinned digest, and it is the last argument.
	if argv[len(argv)-1] != "vllm/vllm-openai:v1@"+validDigest {
		t.Fatalf("image must be the digest-pinned final arg: %v", argv[len(argv)-1])
	}
}

func TestBuildContainerArgvIsolatedWhenNotPublishing(t *testing.T) {
	g := Untrusted(Limits{MemMiB: 1024})
	spec := ContainerSpec{Image: ContainerImage{Digest: validDigest}, Guarantees: g}
	argv := buildContainerArgv(EnginePodman, spec)
	if argv[0] != "podman" {
		t.Fatalf("engine should be podman: %v", argv[0])
	}
	if !argvHasPair(argv, "--network", "none") {
		t.Fatalf("a non-publishing container must be network-isolated: %v", argv)
	}
	if argvHas(argv, "--publish") || argvHas(argv, "--gpus") {
		t.Fatalf("an idle container must publish no port and request no gpu: %v", argv)
	}
	// A bare digest with no ref runs the digest directly.
	if argv[len(argv)-1] != validDigest {
		t.Fatalf("bare digest image expected, got %q", argv[len(argv)-1])
	}
}

func TestContainerImageValidate(t *testing.T) {
	cases := map[string]struct {
		img ContainerImage
		ok  bool
	}{
		"pinned":          {ContainerImage{Ref: "x:1", Digest: validDigest}, true},
		"bare digest":     {ContainerImage{Digest: validDigest}, true},
		"no digest":       {ContainerImage{Ref: "x:1"}, false},
		"tag not digest":  {ContainerImage{Digest: "latest"}, false},
		"short digest":    {ContainerImage{Digest: "sha256:abcd"}, false},
		"non-hex digest":  {ContainerImage{Digest: "sha256:" + strings.Repeat("z", 64)}, false},
		"wrong algorithm": {ContainerImage{Digest: "md5:" + strings.Repeat("a", 64)}, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := tc.img.validate(); (err == nil) != tc.ok {
				t.Fatalf("validate ok=%v, want %v (err=%v)", err == nil, tc.ok, err)
			}
		})
	}
}

func TestGPURequestFlag(t *testing.T) {
	cases := []struct {
		req  GPURequest
		want string
	}{
		{GPURequest{}, ""},
		{GPURequest{Enabled: true}, "all"},
		{GPURequest{Enabled: true, Device: "0,1"}, "device=0,1"},
		{GPURequest{Device: "0"}, ""}, // device without Enabled grants nothing
	}
	for _, tc := range cases {
		if got := tc.req.flag(); got != tc.want {
			t.Fatalf("flag(%+v) = %q, want %q", tc.req, got, tc.want)
		}
	}
}

func TestContainerSpecValidateRefuses(t *testing.T) {
	cases := map[string]func(*ContainerSpec){
		"egress open":    func(s *ContainerSpec) { s.Guarantees.EgressDenied = false },
		"no memory cap":  func(s *ContainerSpec) { s.Guarantees.Limits.MemMiB = 0 },
		"writable mount": func(s *ContainerSpec) { s.Guarantees.Mounts = []Mount{{HostPath: "/h", GuestPath: "/g"}} },
		"relative mount": func(s *ContainerSpec) {
			s.Guarantees.Mounts = []Mount{{HostPath: "rel", GuestPath: "/g", ReadOnly: true}}
		},
		"unpinned image":     func(s *ContainerSpec) { s.Image.Digest = "" },
		"publish no network": func(s *ContainerSpec) { s.Network = "" },
		"bad host port":      func(s *ContainerSpec) { s.HostPort = 0 },
	}
	for name, mut := range cases {
		t.Run(name, func(t *testing.T) {
			spec := servingSpec()
			mut(&spec)
			if err := spec.validate(); err == nil {
				t.Fatalf("expected %s to be refused", name)
			}
		})
	}
}

func TestContainerSpecValidateRefusesIdleWithNetwork(t *testing.T) {
	g := Untrusted(Limits{MemMiB: 1024})
	spec := ContainerSpec{Image: ContainerImage{Digest: validDigest}, Guarantees: g, Network: "x"}
	if err := spec.validate(); err == nil {
		t.Fatal("a non-publishing container that names a network must be refused")
	}
}

func TestContainerSpecValidateAcceptsServing(t *testing.T) {
	if err := servingSpec().validate(); err != nil {
		t.Fatalf("the serving spec should be valid: %v", err)
	}
}

// fakeContainerServing is a minimal Serving for driver tests.
type fakeContainerServing struct{ addr string }

func (f *fakeContainerServing) Addr() string          { return f.addr }
func (f *fakeContainerServing) Running() bool         { return true }
func (f *fakeContainerServing) Output() string        { return "" }
func (f *fakeContainerServing) Done() <-chan struct{} { return make(chan struct{}) }
func (f *fakeContainerServing) Stop() error           { return nil }

// fakeContainerDriver records the spec it was asked to run and reports a fixed
// availability, so the Run/validate/select paths are testable without a real engine.
type fakeContainerDriver struct {
	name string
	av   Availability
	ran  *ContainerSpec
}

func (d *fakeContainerDriver) Name() string         { return d.name }
func (d *fakeContainerDriver) Detect() Availability { return d.av }
func (d *fakeContainerDriver) Run(_ context.Context, spec ContainerSpec) (Serving, error) {
	d.ran = &spec
	return &fakeContainerServing{addr: "127.0.0.1:8123"}, nil
}

func TestRunContainerWithValidatesBeforeRunning(t *testing.T) {
	d := &fakeContainerDriver{name: "fake", av: Availability{OK: true}}
	// A weakened spec must be refused before the driver is ever asked to run it.
	bad := servingSpec()
	bad.Guarantees.EgressDenied = false
	if _, err := RunContainerWith(context.Background(), d, bad); err == nil {
		t.Fatal("a weakened spec must be refused")
	}
	if d.ran != nil {
		t.Fatal("the driver must not run a spec that failed validation")
	}
	// A valid spec runs and returns the handle.
	s, err := RunContainerWith(context.Background(), d, servingSpec())
	if err != nil {
		t.Fatal(err)
	}
	if s.Addr() != "127.0.0.1:8123" || d.ran == nil {
		t.Fatalf("the driver should have run the valid spec, got addr=%q ran=%v", s.Addr(), d.ran)
	}
}

func TestRunContainerWithRefusesUnavailableDriver(t *testing.T) {
	d := &fakeContainerDriver{name: "fake", av: Availability{OK: false, Detail: "no daemon"}}
	if _, err := RunContainerWith(context.Background(), d, servingSpec()); err == nil {
		t.Fatal("an unavailable engine must be refused, never downgraded")
	}
}

func TestSelectContainerDriver(t *testing.T) {
	restore := swapContainerDrivers(
		&fakeContainerDriver{name: "docker", av: Availability{OK: false, Detail: "no daemon"}},
		&fakeContainerDriver{name: "podman", av: Availability{OK: true}},
	)
	defer restore()
	d, err := SelectContainerDriver()
	if err != nil || d.Name() != "podman" {
		t.Fatalf("should select the first available engine, got %v err=%v", d, err)
	}

	restore2 := swapContainerDrivers(&fakeContainerDriver{name: "docker", av: Availability{OK: false, Detail: "absent"}})
	defer restore2()
	if _, err := SelectContainerDriver(); err == nil {
		t.Fatal("no available engine should be a refusal")
	}
}

func TestRunContainerNoEngine(t *testing.T) {
	restore := swapContainerDrivers()
	defer restore()
	if _, err := RunContainer(context.Background(), servingSpec()); err == nil {
		t.Fatal("running with no registered engine must error")
	}
}
