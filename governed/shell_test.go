package governed

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/spine"
)

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
			p := ShellPolicy{DataDir: dataDir, AllowNetwork: tc.allowNetwork, AllowCredentialReads: tc.allowCreds}
			sb, err := sandbox.NewLocal(t.TempDir(), p.options()...)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sb.Close() })

			if got := sb.NetworkDenied(); got == tc.allowNetwork {
				t.Errorf("NetworkDenied = %v with AllowNetwork = %v", got, tc.allowNetwork)
			}
			hidden := sb.UnreadablePaths()
			for _, must := range []string{dataDir, filepath.Join(home, ".ssh"), filepath.Join(home, ".aws")} {
				if has := slices.Contains(hidden, must); has == tc.allowCreds {
					t.Errorf("hidden contains %q = %v with AllowCredentialReads = %v (hidden: %q)", must, has, tc.allowCreds, hidden)
				}
			}
		})
	}
}

// posturesOn returns the payload of every shell posture event on stream, oldest first. A
// resumed run assembles again and states its posture again, so its stream holds one per
// assembly.
func posturesOn(t *testing.T, log spine.Log, stream string) []map[string]any {
	t.Helper()
	events, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatal(err)
	}
	var found []map[string]any
	for _, e := range events {
		if e.Type == ShellPostureEventType {
			found = append(found, e.Payload)
		}
	}
	return found
}

// postureOf returns the one shell posture event on a run's stream, failing when there is not
// exactly one.
func postureOf(t *testing.T, log spine.Log, stream string) map[string]any {
	t.Helper()
	found := posturesOn(t, log, stream)
	if len(found) != 1 {
		t.Fatalf("%d %s events on the run's stream, want exactly one", len(found), ShellPostureEventType)
	}
	return found[0]
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

// TestRunsRecordTheShellPosture is the record of a relaxation: whichever way a run is
// assembled, its stream states whether its commands had the network and whether the
// credential locations were hidden, so an opt-in is in the sealed history rather than only on
// a command line nobody kept.
func TestRunsRecordTheShellPosture(t *testing.T) {
	useHome(t)
	dataDir := filepath.Join(t.TempDir(), "flynn-data")

	for _, tc := range []struct {
		name                         string
		policy                       ShellPolicy
		wantNetwork, wantCredentials string
	}{
		{"default", ShellPolicy{DataDir: dataDir}, "denied", "hidden"},
		{"network opt-in", ShellPolicy{DataDir: dataDir, AllowNetwork: true}, "open", "hidden"},
		{"credential opt-in", ShellPolicy{DataDir: dataDir, AllowCredentialReads: true}, "denied", "readable"},
		{"both opt-ins", ShellPolicy{DataDir: dataDir, AllowNetwork: true, AllowCredentialReads: true}, "open", "readable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), WithShellPolicy(tc.policy))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = run.Close() })

			got := postureOf(t, b.Log, run.ID())
			if got["network"] != tc.wantNetwork || got["credentials"] != tc.wantCredentials {
				t.Fatalf("posture = %v, want network %q and credentials %q", got, tc.wantNetwork, tc.wantCredentials)
			}
			if tier := run.Sandbox().(*sandbox.Local).ConfinementTier(); got["tier"] != tier {
				t.Errorf("posture names tier %v, the sandbox runs at %q", got["tier"], tier)
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

// TestRunsRecordWhyTheCredentialsWereReadable: a posture whose credentials are readable says
// whether the policy asked for it or the platform could not hide them, so a record read later
// tells an operator's choice from a platform limit. The fallback hides them wherever the
// platform can, and then records no reason, because nothing was relaxed.
func TestRunsRecordWhyTheCredentialsWereReadable(t *testing.T) {
	useHome(t)
	dataDir := filepath.Join(t.TempDir(), "flynn-data")
	fallback := "hidden"
	fallbackReason := ""
	if !sandbox.ReadDenySupported() {
		fallback, fallbackReason = "readable", CredentialsReadableByPlatform
	}
	for _, tc := range []struct {
		name                        string
		policy                      ShellPolicy
		wantCredentials, wantReason string
	}{
		{"default", ShellPolicy{DataDir: dataDir}, "hidden", ""},
		{"credential opt-in", ShellPolicy{DataDir: dataDir, AllowCredentialReads: true}, "readable", CredentialsReadableByPolicy},
		{"platform fallback", ShellPolicy{DataDir: dataDir, AllowCredentialReadsWhereUnhidable: true}, fallback, fallbackReason},
		{"both", ShellPolicy{DataDir: dataDir, AllowCredentialReads: true, AllowCredentialReadsWhereUnhidable: true}, "readable", CredentialsReadableByPolicy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if readable, reason := tc.policy.CredentialsReadable(); readable != (tc.wantCredentials == "readable") || reason != tc.wantReason {
				t.Fatalf("CredentialsReadable = %v, %q; want credentials %s with reason %q", readable, reason, tc.wantCredentials, tc.wantReason)
			}
			b, _ := memBackends(t)
			run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), WithShellPolicy(tc.policy))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = run.Close() })
			got := postureOf(t, b.Log, run.ID())
			reason, _ := got["credentials_reason"].(string)
			if got["credentials"] != tc.wantCredentials || reason != tc.wantReason {
				t.Fatalf("posture = %v, want credentials %q with reason %q", got, tc.wantCredentials, tc.wantReason)
			}
		})
	}
}

// TestStrictPolicyRefusesWhereThePlatformCannotHide: without the fallback, a platform that
// cannot hide a path refuses the command rather than running it with the credentials in
// view, so an embedder that never opted in gets no silent relaxation.
func TestStrictPolicyRefusesWhereThePlatformCannotHide(t *testing.T) {
	if sandbox.ReadDenySupported() {
		t.Skip("this platform hides paths, so the strict policy runs commands")
	}
	useHome(t)
	sb, err := sandbox.NewLocal(t.TempDir(), ShellPolicy{}.options()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sb.Close() })
	if _, err := sb.Exec(ctxT(t), sandbox.Command{Line: "echo ran"}); !errors.Is(err, sandbox.ErrReadDeny) {
		t.Fatalf("got %v, want ErrReadDeny", err)
	}
	ok, err := sandbox.NewLocal(t.TempDir(), ShellPolicy{AllowCredentialReadsWhereUnhidable: true}.options()...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ok.Close() })
	res, err := ok.Exec(ctxT(t), sandbox.Command{Line: "echo ran"})
	if err != nil || !strings.Contains(res.Output, "ran") {
		t.Fatalf("the fallback did not run the command: %v, %q", err, res.Output)
	}
}

// TestEmbedderGetsTheSecureShellByDefault: a host that names no shell policy gets the
// strictest one on every path that builds a sandbox, and the run says so on its stream. It is
// the guarantee that an embedded run is no more open than the binary's.
func TestEmbedderGetsTheSecureShellByDefault(t *testing.T) {
	home := useHome(t)

	check := func(t *testing.T, sb sandbox.Sandbox, log spine.Log, stream string) {
		t.Helper()
		local, ok := sb.(*sandbox.Local)
		if !ok {
			t.Fatalf("the run's sandbox is %T, want the one it builds", sb)
		}
		if !local.NetworkDenied() {
			t.Error("a run with no shell policy has the host's network")
		}
		if !slices.Contains(local.UnreadablePaths(), filepath.Join(home, ".ssh")) {
			t.Errorf("a run with no shell policy can read ~/.ssh: hidden = %q", local.UnreadablePaths())
		}
		got := postureOf(t, log, stream)
		if got["network"] != "denied" || got["credentials"] != "hidden" {
			t.Errorf("posture = %v, want the network denied and the credentials hidden", got)
		}
	}

	t.Run("conversation", func(t *testing.T) {
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = run.Close() })
		check(t, run.Sandbox(), b.Log, run.ID())
	})
	t.Run("fanout", func(t *testing.T) {
		b, _ := memBackends(t)
		run, err := Assemble(ctxT(t), say("done"), b, WithWorkDir(t.TempDir()), WithFanout(nil))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = run.Close() })
		check(t, run.Sandbox(), b.Log, run.ID())
	})
	t.Run("kit", func(t *testing.T) {
		b, _ := memBackends(t)
		k, err := NewKit(ctxT(t), b.Log, WithWorkDir(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = k.Close() })
		check(t, k.p.sb, b.Log, k.SessionID())
	})
}

// TestShellPolicyReachesTheKit: the options a served tool host passes are applied to the
// sandbox its calls run in, not only to a goal loop's.
func TestShellPolicyReachesTheKit(t *testing.T) {
	useHome(t)
	b, _ := memBackends(t)
	k, err := NewKit(ctxT(t), b.Log, WithWorkDir(t.TempDir()), WithShellPolicy(ShellPolicy{AllowNetwork: true, AllowCredentialReads: true}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = k.Close() })

	got := postureOf(t, b.Log, k.SessionID())
	if got["network"] != "open" || got["credentials"] != "readable" {
		t.Fatalf("kit posture = %v, want the relaxation recorded", got)
	}
}

// TestShellPolicyNeedsTheSandboxTheRunBuilds: a policy that cannot take effect is refused
// rather than recorded as in force. A host's own sandbox is the host's to confine, and a
// toolset run has no shell.
func TestShellPolicyNeedsTheSandboxTheRunBuilds(t *testing.T) {
	b, _ := memBackends(t)
	host, err := sandbox.NewLocal(t.TempDir(), sandbox.WithDefaultConfinement())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = host.Close() })

	_, err = Assemble(ctxT(t), say("done"), b, WithSandbox(host), WithShellPolicy(ShellPolicy{}))
	if err == nil || !strings.Contains(err.Error(), "WithShellPolicy") {
		t.Fatalf("WithShellPolicy over a host sandbox: err = %v, want a refusal naming the option", err)
	}

	_, err = Assemble(ctxT(t), say("done"), b,
		WithToolset([]mission.Tool{namedTool("only")}, capability.NewGrant("only", mission.ActionModelGenerate)),
		Waive(GuardSandbox, "no shell"), WithShellPolicy(ShellPolicy{}))
	if err == nil || !strings.Contains(err.Error(), "WithShellPolicy") {
		t.Fatalf("WithShellPolicy on a toolset run: err = %v, want a refusal naming the option", err)
	}
}
