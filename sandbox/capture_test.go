package sandbox

import (
	"context"
	"strings"
	"testing"
)

func TestCaptureCombinesStdoutAndStderr(t *testing.T) {
	l := newTestLocal(t)
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=1", "HELPER_STDERR=diagnostic"},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	// Both streams are collected: the stdout line and the stderr line. This is the point of
	// Capture over Stream, which delivers only stdout.
	if !strings.Contains(res.Output, "line0") {
		t.Errorf("output missing stdout: %q", res.Output)
	}
	if !strings.Contains(res.Output, "diagnostic") {
		t.Errorf("output missing stderr; Capture must combine streams: %q", res.Output)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
}

func TestCaptureReportsNonZeroExit(t *testing.T) {
	l := newTestLocal(t)
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_EXIT=3"},
	})
	// A non-zero exit is a normal result, not a Go error: the command ran.
	if err != nil {
		t.Fatalf("Capture returned an error for a non-zero exit; want the code on the result: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
}

func TestCaptureRejectsEmptyArgv(t *testing.T) {
	l := newTestLocal(t)
	if _, err := l.Capture(context.Background(), CaptureSpec{}); err == nil {
		t.Fatal("Capture with no argv returned nil error; want a rejection")
	}
}

func TestCaptureGrantsEnvButScrubsHost(t *testing.T) {
	t.Setenv("SANDBOX_HOST_SECRET", "must-not-leak")
	l := newTestLocal(t)
	// The host secret is set in this process's environment but never granted, so it must
	// not survive the deny-by-default scrub.
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_ECHO_ENV=SANDBOX_HOST_SECRET"},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if strings.Contains(res.Output, "must-not-leak") {
		t.Errorf("host secret leaked into the child env: %q", res.Output)
	}

	// A granted variable does reach the child.
	res, err = l.Capture(context.Background(), CaptureSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_ECHO_ENV=GRANTED", "GRANTED=reached"},
	})
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if !strings.Contains(res.Output, "env:reached") {
		t.Errorf("granted var did not reach the child: %q", res.Output)
	}
}

func TestCaptureConfinedProducesOutput(t *testing.T) {
	l := newTestLocal(t, WithDefaultConfinement())
	// A confined child can exec only a binary it can read: on Windows the AppContainer
	// cannot read the test binary in its temp dir, so copy the helper into the working
	// directory, the one location the confinement grants it.
	bin := copyHelperInto(t, l.Root())
	res, err := l.Capture(context.Background(), CaptureSpec{
		Argv: []string{bin, "-test.run=TestHelperProcess"},
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=2", "HELPER_STDERR=confined-err"},
	})
	if err != nil {
		t.Fatalf("Capture confined: %v", err)
	}
	if !strings.Contains(res.Output, "line0") || !strings.Contains(res.Output, "confined-err") {
		t.Fatalf("confined capture output = %q, want both stdout and stderr", res.Output)
	}
}

func TestCaptureCanceledContextFails(t *testing.T) {
	l := newTestLocal(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already canceled before the launch
	if _, err := l.Capture(ctx, CaptureSpec{
		Argv: helperArgv(),
		Env:  []string{"SANDBOX_STREAM_HELPER=1", "HELPER_LINES=1"},
	}); err == nil {
		t.Fatal("Capture under a canceled context returned nil error; want a failure")
	}
}
