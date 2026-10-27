package deploy

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"slices"
	"strings"
	"testing"
)

const (
	// seccompProfile is the profile an operator passes to the container runtime
	// (docker run --security-opt seccomp=deploy/seccomp/flynn.json).
	seccompProfile = "seccomp/flynn.json"
	// mobyDefaultProfile is Docker's default profile, copied unchanged from moby/profiles
	// at the commit dev/container-profiles pins, which also checks the copy against it.
	mobyDefaultProfile = "testdata/moby-default-seccomp.json"
)

// updateProfiles rewrites the shipped container profiles from the pinned originals instead
// of checking them. dev/container-profiles passes it after refreshing the upstream copies.
var updateProfiles = flag.Bool("update-profiles", false, "rewrite the shipped container profiles from the pinned Docker originals")

// sandboxSyscalls are the calls Docker's default profile allows only to CAP_SYS_ADMIN and
// the Linux sandbox needs without it: clone with namespace flags (the launcher's user,
// network and mount namespaces), unshare, and mount (the read-only host view, made inside
// the sandbox's own user namespace, where the kernel permits it to an unprivileged
// process). Measured on Docker 29: dropping mount, or allowing unshare, mount, umount2 and
// pivot_root without clone, still refuses.
var sandboxSyscalls = []string{"clone", "mount", "unshare"}

// sandboxRule is the one rule the shipped profile adds to Docker's default. It allows the
// calls with no argument or capability condition; the kernel still refuses a namespace or
// a mount that the container's user does not own.
const sandboxRule = `		{
			"names": [
				"clone",
				"mount",
				"unshare"
			],
			"action": "SCMP_ACT_ALLOW",
			"comment": "Flynn sandbox: user, network and mount namespaces and the confined mount view, without CAP_SYS_ADMIN"
		}`

// generateSeccomp appends sandboxRule to the default profile's syscalls list as text, so
// the shipped file keeps upstream's layout and a diff against upstream is the one rule.
func generateSeccomp(t *testing.T, base []byte) []byte {
	t.Helper()
	const tail = "\n\t]\n}"
	text := strings.TrimRight(string(base), "\n")
	if !strings.HasSuffix(text, tail) {
		t.Fatalf("%s does not end with the syscalls list; upstream changed its layout, so update generateSeccomp", mobyDefaultProfile)
	}
	return []byte(strings.TrimSuffix(text, tail) + ",\n" + sandboxRule + tail + "\n")
}

// profile is the part of a seccomp profile the comparison reads. Everything else is kept
// as raw JSON and compared whole.
type profile struct {
	Syscalls []json.RawMessage `json:"syscalls"`
}

type syscallRule struct {
	Names    []string        `json:"names"`
	Action   string          `json:"action"`
	Args     json.RawMessage `json:"args"`
	Includes json.RawMessage `json:"includes"`
	Excludes json.RawMessage `json:"excludes"`
}

func decodeProfile(t *testing.T, b []byte) (profile, map[string]json.RawMessage) {
	t.Helper()
	var p profile
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	delete(top, "syscalls")
	return p, top
}

// TestSeccompProfileIsDockerDefaultPlusSandbox holds the shipped profile to Docker's
// default plus one rule. The file must be exactly what the generator produces from the
// pinned default, so a hand edit that widens it fails here; and the comparison below
// checks the result independently of the generator, so a generator bug that drops or
// rewrites an upstream rule fails too.
func TestSeccompProfileIsDockerDefaultPlusSandbox(t *testing.T) {
	base := []byte(readFile(t, mobyDefaultProfile))
	want := generateSeccomp(t, base)
	if *updateProfiles {
		if err := os.WriteFile(seccompProfile, want, 0o644); err != nil { //nolint:gosec // a profile the operator reads and passes to the runtime; nothing secret
			t.Fatal(err)
		}
	}
	got := []byte(readFile(t, seccompProfile))
	if !bytes.Equal(got, want) {
		t.Fatalf("%s is not the generated profile; run dev/container-profiles to regenerate it from the pinned Docker default", seccompProfile)
	}

	baseProfile, baseTop := decodeProfile(t, base)
	gotProfile, gotTop := decodeProfile(t, got)
	if len(gotTop) != len(baseTop) {
		t.Fatalf("top-level keys differ from the default: got %d, want %d", len(gotTop), len(baseTop))
	}
	for k, v := range baseTop {
		if !bytes.Equal(gotTop[k], v) {
			t.Errorf("top-level %q differs from the default", k)
		}
	}
	n := len(baseProfile.Syscalls)
	if len(gotProfile.Syscalls) != n+1 {
		t.Fatalf("profile has %d syscall rules, want the default's %d plus one", len(gotProfile.Syscalls), n)
	}
	for i := range n {
		if !bytes.Equal(gotProfile.Syscalls[i], baseProfile.Syscalls[i]) {
			t.Errorf("syscall rule %d differs from the default", i)
		}
	}
	var added syscallRule
	if err := json.Unmarshal(gotProfile.Syscalls[n], &added); err != nil {
		t.Fatalf("decode the added rule: %v", err)
	}
	if !slices.Equal(added.Names, sandboxSyscalls) {
		t.Errorf("added rule names %q, want exactly %q", added.Names, sandboxSyscalls)
	}
	if added.Action != "SCMP_ACT_ALLOW" {
		t.Errorf("added rule action = %q, want SCMP_ACT_ALLOW", added.Action)
	}
	if added.Args != nil || added.Includes != nil || added.Excludes != nil {
		t.Error("added rule carries conditions; it is meant to be the plain allow measured on Docker")
	}
}

// TestSeccompProfileNeedsTheRule checks the premise of the added rule against the pinned
// default: each call the sandbox needs is granted there only behind CAP_SYS_ADMIN. When
// upstream starts allowing one of them to every container, this fails, and the rule
// should shrink to what is still missing.
func TestSeccompProfileNeedsTheRule(t *testing.T) {
	p, _ := decodeProfile(t, []byte(readFile(t, mobyDefaultProfile)))
	for _, name := range sandboxSyscalls {
		for i, raw := range p.Syscalls {
			var r syscallRule
			if err := json.Unmarshal(raw, &r); err != nil {
				t.Fatalf("decode rule %d: %v", i, err)
			}
			if r.Action != "SCMP_ACT_ALLOW" || !slices.Contains(r.Names, name) {
				continue
			}
			if r.Includes == nil || !strings.Contains(string(r.Includes), "CAP_SYS_ADMIN") {
				if name == "clone" && r.Args != nil {
					// The default allows clone without namespace flags to everyone; the
					// sandbox needs the flags, which only the CAP_SYS_ADMIN rule permits.
					continue
				}
				t.Errorf("the default already allows %s to every container (rule %d); drop it from the added rule", name, i)
			}
		}
	}
}
