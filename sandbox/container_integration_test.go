//go:build oci_integration

// This file drives a real OCI engine to prove the concrete container backend runs, adopts,
// and tears down an actual container, not just a scripted one. It is build-tagged off by
// default because it needs a working docker or podman daemon and pulls a small public image,
// neither of which every CI runner has. On a leg with a container engine the tag is set and
// these run for real; everywhere else they never compile in, so the suite stays portable.
//
// Run: go test -tags oci_integration ./sandbox -run TestOCIReal

package sandbox

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// realDriver returns a docker driver, skipping cleanly when no engine is usable so the leg
// is a no-op rather than a failure off a container host.
func realDriver(t *testing.T) ContainerDriver {
	t.Helper()
	d := NewContainerDriver(EngineDocker, nil)
	if av := d.Detect(); !av.OK {
		t.Skipf("no usable docker engine: %s", av.Detail)
	}
	return d
}

// pinnedBusybox pulls busybox and returns its digest-pinned image, the small public image
// the lifecycle test runs.
func pinnedBusybox(t *testing.T) ContainerImage {
	t.Helper()
	if out, err := exec.Command("docker", "pull", "busybox:latest").CombinedOutput(); err != nil {
		t.Skipf("cannot pull busybox (no network?): %v: %s", err, out)
	}
	out, err := exec.Command("docker", "inspect", "--format", "{{index .RepoDigests 0}}", "busybox:latest").Output()
	if err != nil {
		t.Fatalf("inspect busybox digest: %v", err)
	}
	ref := strings.TrimSpace(string(out)) // e.g. "busybox@sha256:..."
	at := strings.LastIndex(ref, "@")
	if at < 0 {
		t.Fatalf("unexpected repo digest %q", ref)
	}
	return ContainerImage{Ref: ref[:at], Digest: ref[at+1:]}
}

func TestOCIRealRunAdoptStop(t *testing.T) {
	d := realDriver(t)
	img := pinnedBusybox(t)

	// A non-publishing, network-isolated container under the untrusted posture, kept alive
	// by a sleep so the lifecycle is observable.
	spec := ContainerSpec{
		Image:      img,
		Guarantees: Untrusted(Limits{MemMiB: 64, VCPUs: 1, PIDs: 64}),
		Command:    []string{"sleep", "120"},
	}
	if err := spec.validate(); err != nil {
		t.Fatalf("the spec should be valid: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := d.Run(ctx, spec)
	if err != nil {
		t.Fatalf("run real container: %v", err)
	}
	t.Cleanup(func() { _ = s.Stop() })

	if !s.Running() {
		t.Fatal("the container should be running right after start")
	}
	if s.Addr() != "" {
		t.Fatalf("a non-publishing container has no address, got %q", s.Addr())
	}

	// Stop tears it down and the reaper observes the exit.
	if err := s.Stop(); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-s.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("the container did not report done after stop")
	}
	if s.Running() {
		t.Fatal("a stopped container must not report running")
	}
}

func TestOCIRealRefusesUnpinnedImage(t *testing.T) {
	d := realDriver(t)
	// A tag, not a digest: the tier refuses it before the engine is ever invoked.
	spec := ContainerSpec{
		Image:      ContainerImage{Ref: "busybox", Digest: "latest"},
		Guarantees: Untrusted(Limits{MemMiB: 64}),
		Command:    []string{"true"},
	}
	if _, err := RunContainerWith(context.Background(), d, spec); err == nil {
		t.Fatal("an unpinned image must be refused")
	}
}
