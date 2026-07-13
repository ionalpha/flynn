package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/inference"
	"github.com/ionalpha/flynn/sandbox"
)

// scriptedSandbox answers Exec from a table keyed by the exact command line, so a runtime
// probe is driven with fixed version output and no runtime is installed for the test to
// depend on. Every other sandbox operation is unused by the version probe and denied.
type scriptedSandbox struct {
	// replies maps a command line to the result it produces. An absent line is treated as
	// a missing binary: the exec fails, exactly as it does when the program is not on PATH.
	replies map[string]sandbox.ExecResult
	calls   []string
}

func (s *scriptedSandbox) Exec(_ context.Context, cmd sandbox.Command) (sandbox.ExecResult, error) {
	s.calls = append(s.calls, cmd.Line)
	res, ok := s.replies[cmd.Line]
	if !ok {
		return sandbox.ExecResult{}, errors.New("executable file not found")
	}
	return res, nil
}

func (s *scriptedSandbox) ReadFile(context.Context, string) ([]byte, error) {
	return nil, sandbox.ErrDenied
}

func (s *scriptedSandbox) WriteFile(context.Context, string, []byte) error { return sandbox.ErrDenied }

func (s *scriptedSandbox) Glob(context.Context, string) ([]string, error) {
	return nil, sandbox.ErrDenied
}

func (s *scriptedSandbox) Walk(context.Context, string) ([]string, error) {
	return nil, sandbox.ErrDenied
}
func (s *scriptedSandbox) Close() error { return nil }

// TestReportRuntimesFlagsAVulnerableBuild is the security-visible case: a llama.cpp build
// below the parser-fix floor is reported as vulnerable with the reason, not as merely
// installed.
func TestReportRuntimesFlagsAVulnerableBuild(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{
		"llama-server --version": {Output: "version: 3000 (abc1234)"},
	}}
	var out bytes.Buffer
	if err := reportRuntimes(context.Background(), sb, &out); err != nil {
		t.Fatalf("reportRuntimes: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "llama.cpp") || !strings.Contains(got, "VULNERABLE") {
		t.Fatalf("an old llama.cpp build must be reported as vulnerable, got:\n%s", got)
	}
	if !strings.Contains(got, "update the runtime") {
		t.Errorf("the report must say what to do about it, got:\n%s", got)
	}
}

// TestReportRuntimesAcceptsACurrentBuild checks a build at or above the floor reports ok.
func TestReportRuntimesAcceptsACurrentBuild(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{
		"llama-server --version": {Output: "version: 9999 (feedface)"},
		"ollama --version":       {Output: "ollama version is 9.9.9"},
		"vllm --version":         {Output: "9.9.9"},
	}}
	var out bytes.Buffer
	if err := reportRuntimes(context.Background(), sb, &out); err != nil {
		t.Fatalf("reportRuntimes: %v", err)
	}
	got := out.String()
	if strings.Contains(got, "VULNERABLE") || strings.Contains(got, "not installed") {
		t.Fatalf("current builds must all report ok, got:\n%s", got)
	}
	for _, rt := range inference.Runtimes() {
		if !strings.Contains(got, rt.Name) {
			t.Errorf("the report must cover %q, got:\n%s", rt.Name, got)
		}
	}
	if n := strings.Count(got, " ok\n"); n != len(inference.Runtimes()) {
		t.Errorf("want one ok line per runtime, got %d in:\n%s", n, got)
	}
}

// TestReportRuntimesReportsAbsentRuntimes checks an uninstalled runtime is reported as
// absent (never as vulnerable), and that one Flynn can fetch itself says so.
func TestReportRuntimesReportsAbsentRuntimes(t *testing.T) {
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{}}
	var out bytes.Buffer
	if err := reportRuntimes(context.Background(), sb, &out); err != nil {
		t.Fatalf("reportRuntimes: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "local inference runtimes:\n") {
		t.Errorf("missing the report header, got:\n%s", got)
	}
	if n := strings.Count(got, "not installed"); n != len(inference.Runtimes()) {
		t.Fatalf("every runtime should be absent here, got:\n%s", got)
	}
	// llama.cpp is the self-provisioned runtime, so its absence points at the install command.
	if !strings.Contains(got, "flynn models install llama.cpp") {
		t.Errorf("an absent self-provisionable runtime must name the install command, got:\n%s", got)
	}
	// Every binary of every runtime was probed before it was declared absent.
	if len(sb.calls) < len(inference.Runtimes()) {
		t.Errorf("expected a probe per runtime binary, got %v", sb.calls)
	}
}

// TestDetectRuntimeVersionSkipsUnusableBinaries checks the probe falls through a binary
// that fails or prints nothing parseable, and only reports a version it could actually read.
func TestDetectRuntimeVersionSkipsUnusableBinaries(t *testing.T) {
	// llama.cpp probes llama-server first, then llama-cli. The first exits non-zero, so the
	// version must come from the second.
	sb := &scriptedSandbox{replies: map[string]sandbox.ExecResult{
		"llama-server --version": {ExitCode: 1, Output: "version: 9999"},
		"llama-cli --version":    {Output: "version: 8200 (deadbee)"},
	}}
	ver, ok := detectRuntimeVersion(context.Background(), sb, inference.LlamaCpp)
	if !ok {
		t.Fatal("the second binary should have supplied the version")
	}
	if len(ver) != 1 || ver[0] != 8200 {
		t.Fatalf("version = %v, want b8200 from llama-cli", ver)
	}

	// Output with no version token at all is not a usable detection.
	noVersion := &scriptedSandbox{replies: map[string]sandbox.ExecResult{
		"ollama --version": {Output: "could not connect to a running ollama server"},
	}}
	if _, ok := detectRuntimeVersion(context.Background(), noVersion, inference.Ollama); ok {
		t.Error("unparseable output must not count as a detected version")
	}
}

// TestConcernNamesTheReason checks the explanation attached to a vulnerable build: the
// named advisories when the version is exposed to one, and the floor otherwise.
func TestConcernNamesTheReason(t *testing.T) {
	old := concern("llama.cpp", inference.Version{1})
	if old == "" || old == "unsafe" {
		t.Fatalf("an ancient build must be explained, got %q", old)
	}
	exposed := inference.Exposure("llama.cpp", inference.Version{1}, inference.Advisories())
	if len(exposed) > 0 {
		for _, a := range exposed {
			if !strings.Contains(old, a.ID) {
				t.Errorf("concern %q must name advisory %s", old, a.ID)
			}
		}
	} else if !strings.Contains(old, "older than minimum supported") {
		t.Errorf("with no named advisory the floor must be cited, got %q", old)
	}

	// An unknown runtime has neither an advisory nor a floor, so the concern is generic.
	if got := concern("no-such-runtime", inference.Version{1}); got != "unsafe" {
		t.Errorf("concern for an unknown runtime = %q, want unsafe", got)
	}
}

// TestRunRuntimeCheckWiresTheRealSandbox exercises the command as wired: it builds a
// sandbox at the working directory and probes each known runtime. The verdicts depend on
// what is installed on the host, so only the report's shape is asserted.
func TestRunRuntimeCheckWiresTheRealSandbox(t *testing.T) {
	var out bytes.Buffer
	if err := runRuntimeCheck(&out); err != nil {
		t.Fatalf("runRuntimeCheck: %v", err)
	}
	got := out.String()
	if !strings.HasPrefix(got, "local inference runtimes:\n") {
		t.Fatalf("missing the report header, got:\n%s", got)
	}
	for _, rt := range inference.Runtimes() {
		if !strings.Contains(got, rt.Name) {
			t.Errorf("the report must cover %q, got:\n%s", rt.Name, got)
		}
	}
}
