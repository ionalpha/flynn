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
// unit graph) rather than of where the command may read. The default policy makes every
// launch refuse on a platform that cannot hide a path, and these tests would then be
// asserting on that refusal instead of on what they are about.
var checkShell = governed.ShellPolicy{AllowCredentialReads: true}

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
// leaving them off leaves it closed.
func TestShellFlagsBuildThePolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want governed.ShellPolicy
	}{
		{"no flags", nil, governed.ShellPolicy{DataDir: "/d"}},
		{"network", []string{"-allow-network"}, governed.ShellPolicy{DataDir: "/d", AllowNetwork: true}},
		{"credentials", []string{"-allow-credential-reads"}, governed.ShellPolicy{DataDir: "/d", AllowCredentialReads: true}},
		{
			"limits",
			[]string{"-max-memory", "512", "-max-processes", "64"},
			governed.ShellPolicy{DataDir: "/d", Limits: sandbox.ResourceLimits{MemoryMiB: 512, MaxProcesses: 64}},
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
			if !sb.NetworkDenied() {
				t.Error("the assembly's sandbox has the host's network")
			}
			if !slices.Contains(sb.UnreadablePaths(), dataDir) {
				t.Errorf("the assembly's sandbox does not hide the data directory: %q", sb.UnreadablePaths())
			}
		})
	}
}
