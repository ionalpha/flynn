package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ionalpha/flynn/sandbox"
)

// errSandboxRefused is the silent error `flynn sandbox check` returns when the host
// refuses confinement: the report is already printed, and the empty message makes the
// dispatcher exit non-zero without a second line.
var errSandboxRefused = errors.New("")

// runSandbox implements `flynn sandbox check [--json] [--ready-file PATH]`, the self-test for the Linux
// sandbox. It starts a real confined child and reports whether the host allows it, or why
// it does not and what to change. It exits non-zero on any result other than a clean pass.
//
// --ready-file PATH also records the result there for a supervisor, refusals included, so a
// stale pass is overwritten; the path must lie outside every directory a confined command
// can write. There is no flag, argument or environment variable that skips the check or
// accepts a refusal, and an unknown flag is an error.
func runSandbox(args []string, _ string) error {
	return sandboxCommand(args, os.Stdout, os.Stderr, sandbox.CheckConfinement)
}

// sandboxCommand is runSandbox with its output and the check injected, so a test can drive
// a refusal without a host that refuses.
func sandboxCommand(args []string, out, errOut io.Writer, check func(context.Context) sandbox.Readiness) error {
	const usage = "usage: flynn sandbox check [--json] [--ready-file PATH]"
	if len(args) == 0 || args[0] != "check" {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet("sandbox check", flag.ContinueOnError)
	fs.SetOutput(errOut)
	asJSON := fs.Bool("json", false, "print the result as JSON")
	readyFile := fs.String("ready-file", "", "also record the result at this path, outside any directory the sandbox can write")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return errors.New(usage)
	}

	r := check(context.Background())
	var werr error
	if *asJSON {
		werr = writeReadinessJSON(out, r)
	} else {
		werr = writeReadiness(out, r)
	}
	if werr != nil {
		return werr
	}
	if *readyFile != "" {
		if err := sandbox.WriteReadinessFile(*readyFile, r); err != nil {
			return err
		}
	}
	// The exit status follows the result and nothing else, including a failure to print it.
	if !r.Ready {
		return errSandboxRefused
	}
	return nil
}

func writeReadinessJSON(out io.Writer, r sandbox.Readiness) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// writeReadiness prints the result for a person: a pass with what it covered, or a
// refusal with its cause, where it stopped, the fix, and the stronger tiers to use instead.
func writeReadiness(out io.Writer, r sandbox.Readiness) error {
	var b strings.Builder
	if r.Ready {
		fmt.Fprintf(&b, "confinement ready: %s\n", r.Detail)
	} else {
		fmt.Fprintf(&b, "confinement refused: %s\n", r.Cause)
		fmt.Fprintf(&b, "  stage:  %s\n", r.Stage)
		fmt.Fprintf(&b, "  detail: %s\n", r.Detail)
		fmt.Fprintf(&b, "  fix:    %s\n", r.Fix)
		for _, c := range r.Also {
			fmt.Fprintf(&b, "  also possible: %s\n", c)
		}
		fmt.Fprintf(&b, "  instead: %s\n", r.Alternative)
		b.WriteString("Flynn will not run commands unconfined on this host.\n")
	}
	if r.Arch != "" {
		fmt.Fprintf(&b, "  architecture: %s\n", r.Arch)
	}
	for _, w := range r.Warnings {
		fmt.Fprintf(&b, "  warning: %s\n", w)
	}
	_, err := io.WriteString(out, b.String())
	return err
}
