//go:build unix || windows

package stdioguard

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ionalpha/flynn/internal/ndjson"
)

// Stray writes the helper process makes, each by a different route to "stdout".
const (
	strayRaw = "STRAY-VIA-RAW-DESCRIPTOR\n"
	strayGo  = "STRAY-VIA-OS-STDOUT\n"
	// forged looks like a valid frame. A child that prints it onto the frame stream would
	// be injecting a message the peer takes for ours.
	forged = `{"jsonrpc":"2.0","id":99,"result":"forged"}` + "\n"
)

const helperEnv = "STDIOGUARD_TEST_HELPER"

// TestHelperProcess is not a test. The tests below re-run the test binary with
// [helperEnv] set, and this function is what runs in that child.
func TestHelperProcess(t *testing.T) {
	switch os.Getenv(helperEnv) {
	case "":
		return
	case "guarded":
		helperGuarded()
	case "unguarded":
		helperUnguarded()
	case "nostdout":
		helperNoStdout()
	case "emit":
		_, _ = os.Stdout.WriteString(forged)
	case "probe":
		h, _ := strconv.ParseUint(os.Getenv("STDIOGUARD_TEST_FRAME_HANDLE"), 10, 64)
		_ = rawWriteHandle(uintptr(h), []byte(forged))
	}
	os.Exit(0)
}

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "HELPER FAILURE: "+format+"\n", a...)
	os.Exit(3)
}

// helperGuarded installs the guard and then makes every kind of stray write between
// frames. What it writes to the frame writer is the only thing that may reach stdout.
func helperGuarded() {
	g, err := Install(WithStrayDestination(os.Stderr))
	if err != nil {
		fail("install: %v", err)
	}
	if _, err := Install(); !errors.Is(err, ErrAlreadyInstalled) {
		fail("second install = %v, want ErrAlreadyInstalled", err)
	}
	w := ndjson.NewWriter(g.Frames())
	frame := func(n int) {
		if err := w.WriteFrame(map[string]int{"n": n}); err != nil {
			fail("frame %d: %v", n, err)
		}
	}

	frame(1)
	if err := rawWriteStdout([]byte(strayRaw)); err != nil {
		fail("raw write: %v", err)
	}
	frame(2)
	if _, err := os.Stdout.WriteString(strayGo); err != nil {
		fail("os.Stdout write: %v", err)
	}
	frame(3)
	// A child that inherits the process's standard output and prints a valid-looking frame.
	runChild("emit", nil)
	frame(4)
	// A child that tries the parent's frame writer by its descriptor or handle number.
	runChild("probe", []string{"STDIOGUARD_TEST_FRAME_HANDLE=" + strconv.FormatUint(uint64(g.frames.(*os.File).Fd()), 10)})
	frame(5)

	// The drain is asynchronous: wait for every stray to be counted before reporting.
	want := int64(len(strayRaw) + len(strayGo) + len(forged))
	for range 4000 {
		if g.Stats().StrayBytes >= want {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Fprintf(os.Stderr, "stats:%d\n", g.Stats().StrayBytes)
}

// helperNoStdout tries to install the guard in a process that has no standard output. There
// is nothing to preserve and nothing to divert, so Install must say so and leave the process
// installable by a later attempt with a real stdout.
func helperNoStdout() {
	if err := closeStdout(); err != nil {
		fail("close stdout: %v", err)
	}
	if _, err := Install(); !errors.Is(err, ErrNoStdout) {
		fail("install without stdout = %v, want ErrNoStdout", err)
	}
	if _, err := Install(); !errors.Is(err, ErrNoStdout) {
		fail("a failed install must not count as installed: second attempt = %v", err)
	}
}

// helperUnguarded makes the same writes with no guard. It exists so the test can show the
// harness sees corruption when nothing prevents it.
func helperUnguarded() {
	w := ndjson.NewWriter(os.Stdout)
	_ = w.WriteFrame(map[string]int{"n": 1})
	_, _ = os.Stdout.WriteString(strayGo)
	_ = w.WriteFrame(map[string]int{"n": 2})
	runChild("emit", nil)
	_ = w.WriteFrame(map[string]int{"n": 3})
}

// runChild starts the test binary in the given helper mode with this process's standard
// streams, which after Install means the stray pipe.
func runChild(mode string, extraEnv []string) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(append(os.Environ(), helperEnv+"="+mode), extraEnv...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fail("child %s: %v", mode, err)
	}
}

// runHelper runs the test binary in a helper mode and returns what it wrote to stdout and
// stderr.
func runHelper(t *testing.T, mode string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(), helperEnv+"="+mode)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("helper %s: %v\nstdout: %q\nstderr: %s", mode, err, out.String(), errb.String())
	}
	return out.String(), errb.String()
}

// wantFrames is the exact byte stream the helper's frames produce.
func wantFrames(n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "{\"n\":%d}\n", i)
	}
	return b.String()
}

// TestFrameStreamIsByteIdenticalUnderStrayWrites is the guard's contract: a raw write to
// descriptor 1, a write through os.Stdout and a child printing a valid-looking frame all
// happen between frames, and stdout carries exactly the frames and nothing else.
func TestFrameStreamIsByteIdenticalUnderStrayWrites(t *testing.T) {
	stdout, stderr := runHelper(t, "guarded")

	if want := wantFrames(5); stdout != want {
		t.Fatalf("frame stream was corrupted\n got: %q\nwant: %q", stdout, want)
	}
	for _, stray := range []string{strayRaw, strayGo, forged} {
		if !strings.Contains(stderr, stray) {
			t.Errorf("stray write %q was not diverted and recorded\nstderr: %s", stray, stderr)
		}
	}
	want := fmt.Sprintf("stats:%d\n", len(strayRaw)+len(strayGo)+len(forged))
	if !strings.Contains(stderr, want) {
		t.Errorf("stray byte count wrong, want %q in:\n%s", want, stderr)
	}
}

// TestInstallWithoutStdoutFailsCleanly runs the guard in a process whose standard output is
// closed. The helper exits 3 with a message when Install does not return ErrNoStdout.
func TestInstallWithoutStdoutFailsCleanly(t *testing.T) {
	runHelper(t, "nostdout")
}

// TestHarnessSeesCorruptionWithoutTheGuard runs the same writes with no guard and requires
// the frame stream to come out wrong. If this passed with a clean stream, the test above
// would prove nothing.
func TestHarnessSeesCorruptionWithoutTheGuard(t *testing.T) {
	stdout, _ := runHelper(t, "unguarded")
	if stdout == wantFrames(3) {
		t.Fatal("unguarded stdout stayed clean; the corruption tests cannot detect anything")
	}
	if !strings.Contains(stdout, strayGo) || !strings.Contains(stdout, forged) {
		t.Fatalf("unguarded stdout is missing the stray writes the guard test relies on: %q", stdout)
	}
}
