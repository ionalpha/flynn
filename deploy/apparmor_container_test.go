package deploy

import (
	"os"
	"slices"
	"strings"
	"testing"
	"text/template"
)

const (
	// containerAppArmorProfile is the profile an operator loads on an AppArmor host and
	// names with docker run --security-opt apparmor=flynn-container.
	containerAppArmorProfile = "apparmor/flynn-container"
	// mobyAppArmorTemplate is moby/profiles' apparmor/template.go, copied unchanged at the
	// commit dev/container-profiles pins. Docker renders docker-default from it.
	mobyAppArmorTemplate = "testdata/moby-apparmor-template.go.txt"
	// containerProfileName is the name the profile loads under.
	containerProfileName = "flynn-container"
)

// dockerDenyMount is the one docker-default rule the container profile replaces. It
// denies every mount, including the ones the sandbox launcher makes inside the command's
// own user and mount namespaces, where the kernel already confines them.
const dockerDenyMount = "  deny mount,\n"

// sandboxMounts replaces dockerDenyMount: one rule per kind of mount the launcher makes
// (sandbox/confine_linux.go and sandbox/readdeny_linux.go). Making the root rprivate stops
// mounts propagating out of the namespace; bind and rbind build the writable and denied
// paths; remount makes them read-only; tmpfs gives a scratch directory and hides denied
// directories. Nothing else docker-default denies is lifted.
//
// The remount rule lists its flags because a remount rule without options does not cover
// a bind remount: AppArmor refused the launcher's "ro, remount, bind" of / with "failed
// flags match", the remount failed silently, and the sandbox check's canary caught the
// writable host. The list is the read-only bit and the restrictive flags the launcher
// carries over (keepRestrictive in sandbox/confine_linux.go).
var sandboxMounts = []string{
	"mount options=(rprivate) -> /,",
	"mount options in (bind, rbind) /** -> /**,",
	"remount options in (ro, rw, bind, nosuid, nodev, noexec, noatime, nodiratime) /{,**},",
	"mount fstype=tmpfs tmpfs -> /**,",
}

func sandboxMountBlock() string {
	var b strings.Builder
	b.WriteString("  # Flynn sandbox: the launcher's mounts, made inside the command's own user and\n")
	b.WriteString("  # mount namespaces, in place of docker-default's \"deny mount,\".\n")
	for _, r := range sandboxMounts {
		b.WriteString("  " + r + "\n")
	}
	return b.String()
}

// renderDockerDefault renders the pinned template with the values dockerd uses on a host
// that has AppArmor 3 or later, its tunables and its base abstraction (Ubuntu 22.04 and
// later, Debian 12 and later), under the container profile's name.
func renderDockerDefault(t *testing.T) string {
	t.Helper()
	src := readFile(t, mobyAppArmorTemplate)
	const open = "const baseTemplate = `"
	_, rest, ok := strings.Cut(src, open)
	if !ok {
		t.Fatalf("%s has no baseTemplate; upstream changed its layout, so update renderDockerDefault", mobyAppArmorTemplate)
	}
	body, _, ok := strings.Cut(rest, "`")
	if !ok {
		t.Fatalf("%s: baseTemplate is not closed", mobyAppArmorTemplate)
	}
	tmpl, err := template.New("docker-default").Parse(body)
	if err != nil {
		t.Fatalf("parse docker-default template: %v", err)
	}
	var b strings.Builder
	err = tmpl.Execute(&b, struct {
		Abi           string
		Name          string
		DaemonProfile string
		Imports       []string
		InnerImports  []string
	}{
		Abi:           "abi/3.0",
		Name:          containerProfileName,
		DaemonProfile: "unconfined",
		Imports:       []string{"#include <tunables/global>"},
		InnerImports:  []string{"#include <abstractions/base>"},
	})
	if err != nil {
		t.Fatalf("render docker-default: %v", err)
	}
	return b.String()
}

// TestContainerAppArmorProfileIsDockerDefaultPlusMounts holds the shipped profile to
// docker-default with its mount denial swapped for the launcher's mounts, and to nothing
// else, so a hand edit that lifts another of docker-default's rules fails here.
func TestContainerAppArmorProfileIsDockerDefaultPlusMounts(t *testing.T) {
	base := renderDockerDefault(t)
	if strings.Count(base, dockerDenyMount) != 1 {
		t.Fatalf("docker-default no longer has exactly one %q; review what the container profile replaces", strings.TrimSpace(dockerDenyMount))
	}
	want := strings.Replace(base, dockerDenyMount, sandboxMountBlock(), 1)
	if *updateProfiles {
		if err := os.WriteFile(containerAppArmorProfile, []byte(want), 0o644); err != nil { //nolint:gosec // a profile the operator reads and loads; nothing secret
			t.Fatal(err)
		}
	}
	got := readFile(t, containerAppArmorProfile)
	if got != want {
		t.Fatalf("%s is not the generated profile; run dev/container-profiles to regenerate it", containerAppArmorProfile)
	}
	if !strings.Contains(got, `profile "`+containerProfileName+`" `) {
		t.Errorf("profile does not load as %q", containerProfileName)
	}

	// Independent of the generator: every docker-default line but the mount denial is kept.
	gotLines := strings.Split(got, "\n")
	for _, line := range strings.Split(base, "\n") {
		if line+"\n" == dockerDenyMount {
			continue
		}
		if !slices.Contains(gotLines, line) {
			t.Errorf("docker-default line %q is missing", line)
		}
	}
	if strings.Contains(got, dockerDenyMount) {
		t.Error("the mount denial is still present, so the launcher's mounts stay refused")
	}
	for _, forbidden := range []string{"pivot_root", "mount,", "unconfined)", "capability sys_admin"} {
		for _, r := range sandboxMounts {
			if strings.Contains(r, forbidden) {
				t.Errorf("sandbox mount rule %q contains %q", r, forbidden)
			}
		}
	}
}
