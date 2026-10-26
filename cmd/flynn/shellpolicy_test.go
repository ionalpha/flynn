package main

import (
	"context"
	"flag"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ionalpha/flynn/harness"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill/skilltool"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

// checkShell is the policy for a test of what a command's result means (an audit, a proof, a
// unit graph) rather than of where the command may read. The default policy makes every
// launch refuse on a platform that cannot hide a path, and these tests would then be
// asserting on that refusal instead of on what they are about.
var checkShell = shellPolicy{allowCredentialReads: true}

// useHome points the process's notion of a home directory at a fresh one, on every platform
// os.UserHomeDir reads, and returns it.
func useHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestCredentialPathsNameEveryKnownLocation holds the hide set to the places a login token,
// a private key or a keychain socket is kept, so a location dropped from the list is a test
// failure rather than a credential the shell can read.
func TestCredentialPathsNameEveryKnownLocation(t *testing.T) {
	home := filepath.Join(string(filepath.Separator), "home", "agent")
	env := map[string]string{
		"SSH_AUTH_SOCK":                  "/tmp/ssh-XXXX/agent.1",
		"GNUPGHOME":                      "/srv/gnupg",
		"AWS_SHARED_CREDENTIALS_FILE":    "/srv/aws/credentials",
		"GOOGLE_APPLICATION_CREDENTIALS": "/srv/gcp/key.json",
		"KUBECONFIG":                     "/srv/kube/a" + string(filepath.ListSeparator) + "/srv/kube/b",
		"XDG_RUNTIME_DIR":                "/run/user/4242",
		"RUNTIME_DIRECTORY":              "/run/flynn-custom",
	}
	got := credentialPaths(home, func(k string) string { return env[k] }, 4242)

	want := []string{
		filepath.Join(home, ".ssh"),
		filepath.Join(home, ".aws"),
		filepath.Join(home, ".config", "gcloud"),
		filepath.Join(home, ".config", "gh"),
		filepath.Join(home, ".docker"),
		filepath.Join(home, ".kube"),
		filepath.Join(home, ".netrc"),
		filepath.Join(home, ".gnupg"),
		filepath.Join(home, ".claude"),
		filepath.Join(home, ".codex"),
		filepath.Join(home, ".local", "share", "keyrings"),
		"/tmp/ssh-XXXX/agent.1",
		"/srv/gnupg",
		"/srv/aws/credentials",
		"/srv/gcp/key.json",
		"/srv/kube/a",
		"/srv/kube/b",
		runnerRuntimeDir,
		"/run/flynn-custom",
		filepath.Join("/run/user/4242", "bus"),
		filepath.Join("/run/user/4242", "keyring"),
		filepath.Join("/run/user/4242", "gnupg"),
	}
	for _, p := range want {
		if !slices.Contains(got, p) {
			t.Errorf("credentialPaths omits %q\n got: %q", p, got)
		}
	}
}

// TestCredentialPathsFallBackToTheUsersRuntimeDirectory covers a process with no
// XDG_RUNTIME_DIR, which is how a systemd service and a container usually run: the keychain
// and agent sockets are still at the uid's runtime directory, and are still hidden.
func TestCredentialPathsFallBackToTheUsersRuntimeDirectory(t *testing.T) {
	none := func(string) string { return "" }
	got := dedupe(credentialPaths("", none, 1001))
	if want := filepath.Join("/run/user/1001", "bus"); !slices.Contains(got, want) {
		t.Errorf("no fallback to the uid's runtime directory: want %q in %q", want, got)
	}
	// A negative uid is a platform with no uids; there is no directory to guess.
	for _, p := range dedupe(credentialPaths("", none, -1)) {
		if p != runnerRuntimeDir {
			t.Errorf("a guessed path with no uid to derive it from: %q", p)
		}
	}
}

func TestDedupeDropsEmptyAndRepeatedPathsInOrder(t *testing.T) {
	got := dedupe([]string{"", "/a", "/b", "/a", "", "/c", "/b"})
	if want := []string{"/a", "/b", "/c"}; !slices.Equal(got, want) {
		t.Fatalf("dedupe = %q, want %q", got, want)
	}
}

// TestShellFlagsBuildThePolicy shows the flags are the only way to relax the policy, and that
// leaving them off leaves it closed.
func TestShellFlagsBuildThePolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want shellPolicy
	}{
		{"no flags", nil, shellPolicy{dataDir: "/d"}},
		{"network", []string{"-allow-network"}, shellPolicy{dataDir: "/d", allowNetwork: true}},
		{"credentials", []string{"-allow-credential-reads"}, shellPolicy{dataDir: "/d", allowCredentialReads: true}},
		{
			"limits",
			[]string{"-max-memory", "512", "-max-processes", "64"},
			shellPolicy{dataDir: "/d", limits: sandbox.ResourceLimits{MemoryMiB: 512, MaxProcesses: 64}},
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

// TestShellPolicyOptionsRelaxOnlyWhatWasAsked builds a sandbox from each combination and reads
// back what it will enforce. The zero policy must deny the network and hide the data
// directory and a home credential directory; each opt-in must lift its own restriction and
// nothing else.
func TestShellPolicyOptionsRelaxOnlyWhatWasAsked(t *testing.T) {
	home := useHome(t)
	dataDir := filepath.Join(t.TempDir(), "flynn-data")

	for _, tc := range []struct {
		name         string
		allowNetwork bool
		allowCreds   bool
	}{
		{"default", false, false},
		{"network opt-in", true, false},
		{"credential opt-in", false, true},
		{"both opt-ins", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := shellPolicy{dataDir: dataDir, allowNetwork: tc.allowNetwork, allowCredentialReads: tc.allowCreds}
			sb, err := sandbox.NewLocal(t.TempDir(), p.options()...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sb.Close() })

			if got := sb.NetworkDenied(); got == tc.allowNetwork {
				t.Errorf("NetworkDenied = %v with allowNetwork = %v", got, tc.allowNetwork)
			}
			hidden := sb.UnreadablePaths()
			for _, must := range []string{dataDir, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws")} {
				if has := slices.Contains(hidden, must); has == tc.allowCreds {
					t.Errorf("hidden contains %q = %v with allowCredentialReads = %v (hidden: %q)", must, has, tc.allowCreds, hidden)
				}
			}
		})
	}
}

// postureOf returns the one shell posture event on a run's stream, failing when there is not
// exactly one.
func postureOf(t *testing.T, log spine.Log, stream string) map[string]any {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	var found []spine.Event
	for _, e := range events {
		if e.Type == shellPostureEvent {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		t.Fatalf("%d %s events on the run's stream, want exactly one", len(found), shellPostureEvent)
	}
	return found[0].Payload
}

// TestRunsRecordTheShellPosture is the record of a relaxation: whichever way a run is
// assembled, its stream states whether its commands had the network and whether the
// credential locations were hidden, so an opt-in is in the sealed history rather than only on
// a command line nobody kept.
func TestRunsRecordTheShellPosture(t *testing.T) {
	useHome(t)
	dataDir := filepath.Join(t.TempDir(), "flynn-data")
	skills := skilltool.New(state.NewMemory().Skills())

	for _, tc := range []struct {
		name                         string
		policy                       shellPolicy
		wantNetwork, wantCredentials string
	}{
		{"default", shellPolicy{dataDir: dataDir}, "denied", "hidden"},
		{"network opt-in", shellPolicy{dataDir: dataDir, allowNetwork: true}, "open", "hidden"},
		{"credential opt-in", shellPolicy{dataDir: dataDir, allowCredentialReads: true}, "denied", "readable"},
		{"both opt-ins", shellPolicy{dataDir: dataDir, allowNetwork: true, allowCredentialReads: true}, "open", "readable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := spine.NewMemoryLog()
			parts, err := newMissionParts(t.TempDir(), log, skills, "", false, tc.policy)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = parts.Close() })

			got := postureOf(t, log, parts.sess.ID())
			if got["network"] != tc.wantNetwork || got["credentials"] != tc.wantCredentials {
				t.Fatalf("posture = %v, want network %q and credentials %q", got, tc.wantNetwork, tc.wantCredentials)
			}
			if got["tier"] != parts.sandbox.ConfinementTier() {
				t.Errorf("posture names tier %v, the sandbox runs at %q", got["tier"], parts.sandbox.ConfinementTier())
			}
			hidden, present := got["hidden"]
			if (tc.wantCredentials == "hidden") != present {
				t.Fatalf("hidden list present = %v with credentials %q", present, tc.wantCredentials)
			}
			if present && !slices.Contains(toStrings(hidden), dataDir) {
				t.Errorf("the hidden list omits the data directory: %v", hidden)
			}
		})
	}
}

// toStrings reads a recorded list back whichever form the log holds it in.
func toStrings(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, len(l))
		for i, e := range l {
			out[i] = fmt.Sprint(e)
		}
		return out
	}
	return nil
}

// TestShippedRunPathsApplyDefaultEgress runs the assemblies the shipped commands use
// (the single conversation `flynn run` and `flynn serve` drive, the fan-out and the MCP
// server's parts) with a policy that names a data directory, and requires each sandbox to
// deny the network and hide that directory. They share one builder, and this is the check
// that none of them grows a sandbox of its own that skips the policy.
func TestShippedRunPathsApplyDefaultEgress(t *testing.T) {
	useHome(t)
	dataDir := filepath.Join(t.TempDir(), "flynn-data")
	policy := shellPolicy{dataDir: dataDir}

	store := memStore(t)
	rstore := store.Resources(mustRegistry(t))

	build := map[string]func() (*missionParts, func(), error){
		"parts": func() (*missionParts, func(), error) {
			p, err := newMissionParts(t.TempDir(), spine.NewMemoryLog(), nil, "", false, policy)
			return p, func() {}, err
		},
		"single conversation": func() (*missionParts, func(), error) {
			r, err := assembleMission(alwaysDone{}, harness.Plan{}, t.TempDir(), defaultSystemPrompt,
				rstore, store.Jobs(), store.Log(), nil, "", policy, false, false, gateSetup{})
			if err != nil {
				return nil, nil, err
			}
			return r.parts, func() { _ = r.Close() }, nil
		},
		"fan-out": func() (*missionParts, func(), error) {
			r, err := assembleFanoutMission(alwaysDone{}, harness.Plan{}, t.TempDir(), defaultSystemPrompt,
				rstore, store.Jobs(), store.Log(), nil, "", nil, policy, gateSetup{})
			if err != nil {
				return nil, nil, err
			}
			return r.parts, func() { _ = r.Close() }, nil
		},
	}
	for name, mk := range build {
		t.Run(name, func(t *testing.T) {
			parts, closeRun, err := mk()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(closeRun)
			if !parts.sandbox.NetworkDenied() {
				t.Error("the assembly's sandbox has the host's network")
			}
			if !slices.Contains(parts.sandbox.UnreadablePaths(), dataDir) {
				t.Errorf("the assembly's sandbox does not hide the data directory: %q", parts.sandbox.UnreadablePaths())
			}
		})
	}
}
