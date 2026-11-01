package main

import (
	"flag"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/externagent"
	"github.com/ionalpha/flynn/governed"
	"github.com/ionalpha/flynn/sandbox"
)

// checkShell is the policy for a test of what a command's result means (an audit, a proof, a
// unit graph) rather than of where the command may read or reach. The default policy makes
// every launch refuse on a platform that cannot hide a path or deny the network, and these
// tests would then be asserting on that refusal instead of on what they are about. The
// network fallback denies it wherever the platform can.
var checkShell = governed.ShellPolicy{AllowCredentialReads: true, AllowNetworkWhereUndeniable: true}

// useHome points the process's notion of a home directory at a fresh one, on every platform
// os.UserHomeDir reads, and returns it.
func useHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestShellFlagsBuildThePolicy shows the flags are the only way to relax the policy, and that
// leaving them off leaves it closed wherever the platform can close it.
func TestShellFlagsBuildThePolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want governed.ShellPolicy
	}{
		{"no flags", nil, governed.ShellPolicy{DataDir: "/d", AllowCredentialReadsWhereUnhidable: true, AllowNetworkWhereUndeniable: true}},
		{"network", []string{"-allow-network"}, governed.ShellPolicy{DataDir: "/d", AllowNetwork: true, AllowCredentialReadsWhereUnhidable: true, AllowNetworkWhereUndeniable: true}},
		{"credentials", []string{"-allow-credential-reads"}, governed.ShellPolicy{DataDir: "/d", AllowCredentialReads: true, AllowCredentialReadsWhereUnhidable: true, AllowNetworkWhereUndeniable: true}},
		{"strict credentials", []string{"-require-credential-hiding"}, governed.ShellPolicy{DataDir: "/d", AllowNetworkWhereUndeniable: true}},
		{"strict network", []string{"-require-network-denial"}, governed.ShellPolicy{DataDir: "/d", AllowCredentialReadsWhereUnhidable: true}},
		{
			"limits",
			[]string{"-max-memory", "512", "-max-processes", "64"},
			governed.ShellPolicy{DataDir: "/d", Limits: sandbox.ResourceLimits{MemoryMiB: 512, MaxProcesses: 64}, AllowCredentialReadsWhereUnhidable: true, AllowNetworkWhereUndeniable: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("flynn", flag.ContinueOnError)
			flags := registerShellFlags(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatal(err)
			}
			if got := flags.policy("/d"); got != tc.want {
				t.Fatalf("policy = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestShippedRunPathsApplyDefaultEgress runs the assemblies the shipped commands use (the
// single conversation `flynn run` and `flynn serve` drive, the fan-out, and the external
// harness) with a policy that names a data directory, and requires each sandbox to deny the
// network and hide that directory. They all go through [governed.Assemble], and this is the
// check that the binary hands it the policy rather than leaving the run on a sandbox of its
// own.
func TestShippedRunPathsApplyDefaultEgress(t *testing.T) {
	useHome(t)
	dataDir := filepath.Join(t.TempDir(), "flynn-data")
	policy := governed.ShellPolicy{DataDir: dataDir}

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))
	base := assembly{
		model: alwaysDone{}, workdir: t.TempDir(), system: defaultSystemPrompt,
		rstore: rstore, jq: store.Jobs(), log: store.Log(), shell: policy,
	}

	fanout := base
	fanout.fanout = &fanoutConfig{}
	external := base
	external.model = nil
	external.ext = &externAgent{model: "gpt-5-codex", driver: externagent.NewDriver(externagent.NewCodex("", nil), nil, t.TempDir())}
	t.Cleanup(external.ext.close)
	for name, asm := range map[string]assembly{
		"single conversation": base,
		"fan-out":             fanout,
		"external harness":    external,
	} {
		t.Run(name, func(t *testing.T) {
			run, err := asm.assemble(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = run.Close() })
			sb, ok := run.Sandbox().(*sandbox.Local)
			if !ok {
				t.Fatalf("the run's sandbox is %T, want the confined local one", run.Sandbox())
			}
			// The strict policy asks for no network. A platform that can deny it does; one that
			// cannot reports the network open (and refuses the launch, see the sandbox tests).
			if sb.NetworkDenied() != sandbox.NetworkDenySupported() {
				t.Errorf("NetworkDenied = %v with NetworkDenySupported = %v", sb.NetworkDenied(), sandbox.NetworkDenySupported())
			}
			if !slices.Contains(sb.UnreadablePaths(), dataDir) {
				t.Errorf("the assembly's sandbox does not hide the data directory: %q", sb.UnreadablePaths())
			}
		})
	}
}

// TestCredentialNoticeNamesOnlyThePlatformFallback: the operator is told when the platform,
// and nothing they typed, left the credentials readable, and only then.
func TestCredentialNoticeNamesOnlyThePlatformFallback(t *testing.T) {
	fallback := governed.ShellPolicy{AllowCredentialReadsWhereUnhidable: true}
	if got := credentialNotice(fallback); (got != "") != !sandbox.ReadDenySupported() {
		t.Errorf("notice %q with ReadDenySupported = %v", got, sandbox.ReadDenySupported())
	}
	for name, p := range map[string]governed.ShellPolicy{
		"strict":   {},
		"operator": {AllowCredentialReads: true, AllowCredentialReadsWhereUnhidable: true},
	} {
		if got := credentialNotice(p); got != "" {
			t.Errorf("%s: notice %q, want none", name, got)
		}
	}
}

// TestNetworkNoticeNamesOnlyThePlatformFallback: the operator is told when the platform, and
// nothing they typed, left the network open, and only then.
func TestNetworkNoticeNamesOnlyThePlatformFallback(t *testing.T) {
	fallback := governed.ShellPolicy{AllowNetworkWhereUndeniable: true}
	if got := networkNotice(fallback); (got != "") != !sandbox.NetworkDenySupported() {
		t.Errorf("notice %q with NetworkDenySupported = %v", got, sandbox.NetworkDenySupported())
	}
	for name, p := range map[string]governed.ShellPolicy{
		"strict":   {},
		"operator": {AllowNetwork: true, AllowNetworkWhereUndeniable: true},
	} {
		if got := networkNotice(p); got != "" {
			t.Errorf("%s: notice %q, want none", name, got)
		}
	}
}
