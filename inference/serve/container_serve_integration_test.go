//go:build oci_integration

// This file proves the container serve path end to end against a real engine: it runs an
// actual published container through EnsureContainer, waits for its loopback endpoint to
// answer the health probe, confirms it is recorded with its container identity, and stops it
// through the engine the same way `models stop` would. It is build-tagged off by default
// because it needs a working docker or podman daemon and pulls a small public image.
//
// Run: go test -tags oci_integration ./inference/serve -run TestContainerServeReal

package serve

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/bindguard"
	"github.com/ionalpha/flynn/sandbox"
)

// a tiny image that answers 200 on any path, so the OpenAI-style /models health probe
// succeeds without a real model server.
const probeImage = "traefik/whoami:latest"

func dockerAvailable(t *testing.T) {
	t.Helper()
	if av := sandbox.NewContainerDriver(sandbox.EngineDocker, nil).Detect(); !av.OK {
		t.Skipf("no usable docker engine: %s", av.Detail)
	}
}

func pinnedProbeImage(t *testing.T) sandbox.ContainerImage {
	t.Helper()
	if out, err := exec.Command("docker", "pull", probeImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s (no network?): %v: %s", probeImage, err, out)
	}
	out, err := exec.Command("docker", "inspect", "--format", "{{index .RepoDigests 0}}", probeImage).Output()
	if err != nil {
		t.Fatalf("inspect digest: %v", err)
	}
	ref := strings.TrimSpace(string(out))
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		t.Fatalf("unexpected repo digest %q", ref)
	}
	return sandbox.ContainerImage{Ref: ref[:at], Digest: ref[at+1:]}
}

func TestContainerServeRealLifecycle(t *testing.T) {
	dockerAvailable(t)
	img := pinnedProbeImage(t)

	// A user-defined network is required for a published container; create one and clean up.
	const net = "flynn-it-egress"
	_ = exec.Command("docker", "network", "rm", net).Run()
	if out, err := exec.Command("docker", "network", "create", net).CombinedOutput(); err != nil {
		t.Skipf("cannot create test network: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "network", "rm", net).Run() })

	port, err := bindguard.FreeLoopbackPort()
	if err != nil {
		t.Fatalf("free port: %v", err)
	}

	reg := NewRegistry(t.TempDir())
	m := NewManager(
		SandboxLauncher{}, HTTPProbe(nil), OSKiller, reg,
		WithContainerStopper(EngineStopper),
		WithReadyTimeout(30*time.Second),
		WithPollInterval(250*time.Millisecond),
	)

	cfg := ContainerEnsureConfig{
		ModelID: "vllm:probe",
		Runtime: "vllm",
		Spec: sandbox.ContainerSpec{
			Image:         img,
			Guarantees:    sandbox.Untrusted(sandbox.Limits{MemMiB: 64, VCPUs: 1, PIDs: 128}),
			Network:       net,
			HostPort:      port,
			ContainerPort: 80, // whoami listens on 80
		},
		BaseURL: fmt.Sprintf("http://127.0.0.1:%d/v1", port),
		Port:    port,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ep, err := m.EnsureContainer(ctx, cfg)
	if err != nil {
		t.Fatalf("EnsureContainer against a real engine: %v", err)
	}
	t.Cleanup(func() { _, _ = m.Stop("vllm:probe") })

	if ep.Reused {
		t.Fatal("a freshly started container must not be reused")
	}
	rec, ok, err := reg.Get("vllm:probe")
	if err != nil || !ok || rec.ContainerID == "" || rec.Engine != "docker" {
		t.Fatalf("the container must be recorded with its identity, got rec=%+v ok=%v err=%v", rec, ok, err)
	}

	// Status sees it live, then a stop through the engine tears it down and prunes the record.
	if live, err := m.Status(ctx); err != nil || len(live) != 1 {
		t.Fatalf("status should report one live container: live=%d err=%v", len(live), err)
	}
	if ok, err := m.Stop("vllm:probe"); err != nil || !ok {
		t.Fatalf("stop: ok=%v err=%v", ok, err)
	}
	if _, present, _ := reg.Get("vllm:probe"); present {
		t.Fatal("the record must be removed after stop")
	}
}
