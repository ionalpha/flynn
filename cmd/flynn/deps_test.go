package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/dependency"
	"github.com/ionalpha/flynn/resource"
)

// stubProber answers the version probe from a fixed table instead of running a program,
// so the deps commands are exercised without depending on what happens to be installed
// on the machine running the tests.
type stubProber struct {
	out map[string]string
}

func (p stubProber) Probe(_ context.Context, name string, _ []string) (string, error) {
	if v, ok := p.out[name]; ok {
		return v, nil
	}
	return "", errors.New("stub prober: " + name + " not present")
}

// openTestDeps opens the real deps runtime (durable store, synced catalog) over a temp
// data directory, with the version probe replaced so detection is deterministic.
func openTestDeps(t *testing.T, probe map[string]string) *depsRuntime {
	t.Helper()
	rt, err := openDepsRuntime(context.Background(), t.TempDir(), dependency.WithProber(stubProber{out: probe}))
	if err != nil {
		t.Fatalf("open deps runtime: %v", err)
	}
	t.Cleanup(func() { _ = rt.closer() })
	return rt
}

func TestDepsListShowsTheCatalog(t *testing.T) {
	rt := openTestDeps(t, nil)
	var buf bytes.Buffer
	if err := depsList(context.Background(), rt, &buf, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "PIN", "MIN", "PLATFORMS", "DESCRIPTION", "flyctl"} {
		if !strings.Contains(out, want) {
			t.Fatalf("listing missing %q:\n%s", want, out)
		}
	}
	// The row carries the pinned version and the count of pinned platform builds.
	line := lineContaining(out, "flyctl")
	fields := strings.Fields(line)
	if len(fields) < 4 {
		t.Fatalf("row too short: %q", line)
	}
	if fields[1] == "-" {
		t.Fatalf("flyctl should carry a pinned version: %q", line)
	}
}

// TestDepsListEmptyCatalog covers the listing over a store with no dependency specs
// synced into it.
func TestDepsListEmptyCatalog(t *testing.T) {
	reg := resource.NewRegistry()
	if err := dependency.RegisterKind(reg); err != nil {
		t.Fatalf("register kind: %v", err)
	}
	rt := &depsRuntime{
		store:  dependency.NewStore(resource.NewMemory(reg)),
		closer: func() error { return nil },
	}
	var buf bytes.Buffer
	if err := depsList(context.Background(), rt, &buf, nil); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(buf.String(), "no dependencies in the catalog") {
		t.Fatalf("output: %q", buf.String())
	}
}

func TestDepsCheckReportsDetectedVersions(t *testing.T) {
	ctx := context.Background()
	// A build well above the floor: the check reports it as usable.
	rt := openTestDeps(t, map[string]string{"flyctl": "flyctl v0.4.61 linux/amd64"})
	var buf bytes.Buffer
	if err := depsCheck(ctx, rt, &buf, nil); err != nil {
		t.Fatalf("check: %v", err)
	}
	line := lineContaining(buf.String(), "flyctl")
	if !strings.Contains(line, "ok") || !strings.Contains(line, "0.4.61") {
		t.Fatalf("check row = %q, want an ok row carrying the detected version", line)
	}

	// A build below the floor is reported as such rather than accepted.
	below := openTestDeps(t, map[string]string{"flyctl": "flyctl v0.1.0 linux/amd64"})
	var stale bytes.Buffer
	if err := depsCheck(ctx, below, &stale, []string{"flyctl"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	if line := lineContaining(stale.String(), "flyctl"); !strings.Contains(line, "below floor") {
		t.Fatalf("check row = %q, want a below-floor row", line)
	}

	// Nothing present at all: the row says so.
	absent := openTestDeps(t, nil)
	var missing bytes.Buffer
	if err := depsCheck(ctx, absent, &missing, []string{"flyctl"}); err != nil {
		t.Fatalf("check: %v", err)
	}
	if line := lineContaining(missing.String(), "flyctl"); !strings.Contains(line, "not installed") {
		t.Fatalf("check row = %q, want a not-installed row", line)
	}
}

func TestDepsCheckUnknownDependency(t *testing.T) {
	rt := openTestDeps(t, nil)
	err := depsCheck(context.Background(), rt, &bytes.Buffer{}, []string{"not-a-dep"})
	if !errors.Is(err, dependency.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
}

func TestDepStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		rep  dependency.Report
		want string
	}{
		{name: "present and current", rep: dependency.Report{Present: true, MeetsFloor: true}, want: "ok"},
		{
			name: "below floor but installable",
			rep:  dependency.Report{Present: true, CanProvision: true},
			want: "below floor (installable)",
		},
		{name: "below floor with no build to install", rep: dependency.Report{Present: true}, want: "below floor"},
		{name: "absent but installable", rep: dependency.Report{CanProvision: true}, want: "not installed (installable)"},
		{name: "absent with no build for this platform", rep: dependency.Report{}, want: "not installed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := depStatus(tc.rep); got != tc.want {
				t.Fatalf("depStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestDepsInstallUsesAPresentBuild proves the detect-installed-first policy at the
// command level: a present program that meets the floor is reported as present and
// nothing is fetched.
func TestDepsInstallUsesAPresentBuild(t *testing.T) {
	rt := openTestDeps(t, map[string]string{"flyctl": "flyctl v0.4.61 linux/amd64"})
	var buf bytes.Buffer
	if err := depsInstall(context.Background(), rt, &buf, []string{"flyctl"}); err != nil {
		t.Fatalf("install: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "flyctl is present at flyctl") || !strings.Contains(out, "(version 0.4.61)") {
		t.Fatalf("output = %q, want the present build to be reported with its version", out)
	}
}

func TestDepsInstallErrors(t *testing.T) {
	ctx := context.Background()
	rt := openTestDeps(t, nil)

	for _, tc := range []struct {
		name    string
		args    []string
		wantErr string
	}{
		{name: "no name", args: nil, wantErr: "usage:"},
		{name: "too many names", args: []string{"a", "b"}, wantErr: "usage:"},
		{name: "unknown dependency", args: []string{"not-a-dep"}, wantErr: "unknown dependency"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := depsInstall(ctx, rt, &buf, tc.args)
			if err == nil {
				t.Fatalf("expected an error, got output %q", buf.String())
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
			}
			if buf.Len() != 0 {
				t.Fatalf("a failed install must print nothing: %q", buf.String())
			}
		})
	}
}

// TestOpenDepsRuntimeUnusableDataDir proves a data directory that cannot be opened fails
// the command rather than surfacing as an empty dependency catalog.
func TestOpenDepsRuntimeUnusableDataDir(t *testing.T) {
	if _, err := openDepsRuntime(context.Background(), blockedDataDir(t)); err == nil {
		t.Fatal("expected an unopenable data directory to fail")
	}
	if err := runDeps([]string{"ls"}, blockedDataDir(t)); err == nil {
		t.Fatal("expected the command to fail on an unopenable data directory")
	}
}

func TestRunDepsDispatch(t *testing.T) {
	err := runDeps([]string{"nope"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "unknown subcommand") {
		t.Fatalf("error = %v, want an unknown-subcommand error", err)
	}
	// No subcommand defaults to the listing, which runs the whole durable path.
	if err := runDeps(nil, t.TempDir()); err != nil {
		t.Fatalf("deps: %v", err)
	}
	// The install subcommand reaches its own validation.
	if err := runDeps([]string{"install"}, t.TempDir()); err == nil ||
		!strings.Contains(err.Error(), "usage:") {
		t.Fatalf("error = %v, want a usage error", err)
	}
}
