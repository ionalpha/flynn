//go:build linux

package sandbox

import (
	"os/exec"
	"slices"
	"syscall"
	"testing"
)

// TestProcMaskedNamesTheMountsTheKernelRefusesToShadow holds the predictor to the mount
// tables it has to tell apart. Getting it wrong in one direction refuses every confined
// launch on a container host; in the other it silently gives up the private /proc on a
// host that could have had it.
func TestProcMaskedNamesTheMountsTheKernelRefusesToShadow(t *testing.T) {
	tests := []struct {
		name string
		mps  []string
		want bool
	}{
		{"a plain host", []string{"/", "/proc", "/sys", "/dev", "/run", "/tmp"}, false},
		{"systemd's binfmt_misc below /proc is empty", []string{"/", "/proc", "/proc/sys/fs/binfmt_misc"}, false},
		{"a masked file, as a container runtime does", []string{"/", "/proc", "/proc/kcore"}, true},
		{"a masked directory", []string{"/", "/proc", "/proc/acpi"}, true},
		{"a read-only bind of /proc/sys", []string{"/", "/proc", "/proc/sys"}, true},
		{"binfmt_misc does not excuse a mask beside it", []string{"/proc/sys/fs/binfmt_misc", "/proc/scsi"}, true},
		{"a path that only starts like /proc", []string{"/", "/proc", "/procfs", "/proc2/x"}, false},
		{"nothing mounted", nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := procMasked(tc.mps); got != tc.want {
				t.Fatalf("procMasked(%v) = %v, want %v", tc.mps, got, tc.want)
			}
		})
	}
}

// TestPrivateProcIsRequestedExactlyWhenAvailable holds the link between the prediction and
// the launch: the launcher mounts a /proc only when the parent put the command in a PID
// namespace, and the parent does so only when the mount table allows it. A launcher told to
// mount without the namespace would fail every launch; a namespace without the mount would
// show the wrong process numbers.
func TestPrivateProcIsRequestedExactlyWhenAvailable(t *testing.T) {
	l, err := NewLocal(t.TempDir(), WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("true")
	if err := l.confine(cmd); err != nil {
		t.Skipf("this host cannot confine commands: %v", err)
	}
	inNamespace := cmd.SysProcAttr.Cloneflags&syscall.CLONE_NEWPID != 0
	requested := slices.Contains(cmd.Env, envPrivateProc+"=1")
	if inNamespace != requested {
		t.Fatalf("PID namespace = %v but the launcher is told to mount /proc = %v", inNamespace, requested)
	}
	if inNamespace != privateProcAvailable() {
		t.Fatalf("PID namespace = %v, privateProcAvailable = %v", inNamespace, privateProcAvailable())
	}
}
