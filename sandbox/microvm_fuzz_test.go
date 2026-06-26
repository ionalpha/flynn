package sandbox

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzBuildManifest hammers the manifest builder, the one place the untrusted posture is
// rendered for the runtime, with arbitrary images, caps, commands, and mounts. The
// load-bearing invariants must hold for every input that the builder accepts: egress is
// always denied, every mount is read-only, the memory cap carries through, the command is
// preserved as argv (never flattened into a shell string), and the builder never panics.
// A regression that let any of these slip would weaken the boundary silently; here it fails.
func FuzzBuildManifest(f *testing.F) {
	f.Add("/k", "/r", "echo hi", 256, "/weights", true)
	f.Add("", "", "", 0, "", false)
	f.Add("rel/k", "/r", "rm -rf /", -5, "rel/mount", true)
	f.Add(`C:\k`, `C:\r`, "serve --port 9", 1024, `C:\w`, false)

	f.Fuzz(func(t *testing.T, kernel, rootfs, cmd string, mem int, mountHost string, ro bool) {
		spec := Spec{
			Root:  filepath.Join(t.TempDir(), "root"),
			Image: Image{Kernel: kernel, RootFS: rootfs},
			Guarantees: Guarantees{
				// Deliberately feed a possibly-open egress and a possibly-writable mount: the
				// builder must clamp them to the untrusted posture regardless of the input.
				EgressDenied: false,
				Limits:       Limits{MemMiB: mem, VCPUs: -1},
				Mounts:       []Mount{{HostPath: mountHost, GuestPath: "/m", ReadOnly: ro}},
			},
		}
		argv := []string{"/bin/sh", "-c", cmd}
		man, err := buildManifest(spec, argv, false, "/tmp/result")
		if err != nil {
			// The only accepted reason to refuse is a non-absolute image path.
			if filepath.IsAbs(kernel) && filepath.IsAbs(rootfs) {
				t.Fatalf("builder refused an absolute-image spec: %v", err)
			}
			return
		}
		if man.Egress {
			t.Fatal("INVARIANT VIOLATED: manifest left egress open for an untrusted guest")
		}
		for _, m := range man.Mounts {
			if !m.ReadOnly {
				t.Fatalf("INVARIANT VIOLATED: manifest mount %q is writable", m.HostPath)
			}
		}
		if man.MemMiB != mem {
			t.Fatalf("memory cap not carried through: got %d want %d", man.MemMiB, mem)
		}
		if man.VCPUs < 1 {
			t.Fatalf("vCPU count must default to at least 1, got %d", man.VCPUs)
		}
		if len(man.Command) != len(argv) {
			t.Fatalf("command not preserved as argv: got %v", man.Command)
		}
		for i := range argv {
			if man.Command[i] != argv[i] {
				t.Fatalf("command argv altered at %d: %q != %q", i, man.Command[i], argv[i])
			}
		}
	})
}

// FuzzMicroVMConfine proves the Sandbox-port file boundary: for any caller-supplied path,
// the microVM either denies the operation or hands the guest a clean, root-relative path
// that cannot escape (never absolute, never starting with "..", never containing a "../"
// segment). A path that reached the guest as an escape would breach the working-area
// confinement that backs the tier's default-deny rule.
func FuzzMicroVMConfine(f *testing.F) {
	for _, s := range []string{"ok.txt", "../escape", "/etc/passwd", "a/../../b", "", ".", "..", `..\win`, "a/b/c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		fm := newFakeMachine()
		d := &fakeDriver{name: "kvm", av: Availability{OK: true}, mach: fm}
		restore := swapDrivers(d)
		defer restore()

		vm, err := BootMicroVM(context.Background(), untrustedSpec(t.TempDir()))
		if err != nil {
			t.Fatalf("boot: %v", err)
		}
		defer func() { _ = vm.Close() }()

		err = vm.WriteFile(context.Background(), p, []byte("x"))
		if err != nil {
			return // denied: the safe outcome
		}
		// Accepted: the path the guest received must be confined.
		fm.mu.Lock()
		defer fm.mu.Unlock()
		for got := range fm.written {
			if filepath.IsAbs(got) || got == ".." || strings.HasPrefix(got, "../") || strings.Contains(got, "/../") {
				t.Fatalf("INVARIANT VIOLATED: escaping path reached the guest: %q (from %q)", got, p)
			}
		}
	})
}
