package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/ionalpha/flynn/brakes"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/mcp"
	"github.com/ionalpha/flynn/sandbox"
)

// runMCP serves the agent's toolset to a Model Context Protocol client over stdio,
// so another program (an editor, another agent's harness, any MCP client) can call
// the agent's tools without being handed the host. Every tools/call runs through
// the same dispatch waist as the agent's own loop: admitted against the session
// grant, gated at the containment level its trust requires, subject to the safety
// brake, and recorded onto the session's spine stream, so a client's effects are
// governed and sealed exactly like a native run's.
//
// The protocol speaks on stdin and stdout, so all diagnostics go to stderr; stdout
// carries only JSON-RPC. It blocks until the client disconnects (EOF) or the
// process is interrupted.
func runMCP(args []string, dataDir string) error {
	// The verb comes first (only "serve" today), then its flags. Parsing from args[1:]
	// keeps the flags after the verb, since flag parsing stops at the first
	// non-flag argument.
	if len(args) < 1 || args[0] != "serve" {
		return errors.New("usage: flynn mcp serve [--read-only] [--workdir DIR]")
	}
	fs := flag.NewFlagSet("mcp serve", flag.ContinueOnError)
	readOnly := fs.Bool("read-only", false, "expose only the read tools (read, glob, grep); deny write, edit, and shell")
	workdir := fs.String("workdir", "", "directory the tools operate in (default: current directory)")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	dir := *workdir
	if dir == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		dir = cwd
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	return serveMCP(ctx, dataDir, dir, *readOnly, os.Stdin, os.Stdout, os.Stderr)
}

// serveMCP is the served half of `flynn mcp serve` with its streams supplied: it opens the
// durable store, assembles the governed run ingredients, and serves JSON-RPC on in/out
// until the client disconnects or ctx is done. Diagnostics go to logw, never to out, which
// carries protocol traffic only.
func serveMCP(ctx context.Context, dataDir, dir string, readOnly bool, in io.Reader, out, logw io.Writer) error {
	// Record onto the durable spine under the instance signer when one is present, so a
	// served session's governed actions are sealed and verifiable; without an identity
	// the session still runs, recording unsigned.
	signer, serr := runSigner(ctx, dataDir)
	if serr != nil {
		signer = nil
	}
	store, err := openDataStore(ctx, dataDir, snapshotOptions(signer)...)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// The shared run ingredients: a sandbox rooted at dir, the default toolset over it,
	// the full-toolset grant, and a spine sink on a fresh session stream. Reusing this
	// assembly is the point: a served session is governed by the same grant and records
	// onto the same kind of stream as the agent's own run, so authority cannot drift
	// between the two paths.
	parts, err := newMissionParts(dir, store.Log(), "", false, sandbox.ResourceLimits{})
	if err != nil {
		return err
	}
	defer func() { _ = parts.Close() }()

	// The grant the client's calls are admitted against. The default is the full
	// toolset the agent itself may use in this directory; --read-only narrows it to the
	// read tools so an untrusted client cannot write or run a command even though the
	// containment gate would still bound a shell.
	grant := parts.grant
	if readOnly {
		grant = capability.NewGrant("read", "glob", "grep")
	}

	// A rate breaker is the standing safety brake: it halts a client that drives a
	// degenerate tight loop, well above any real pace, and shares one halt state so an
	// operator kill-switch would stop the session too.
	brk := brakes.NewHook(brakes.Limits{MaxActions: defaultMaxActionsPerMinute, Window: time.Minute}, nil)

	d := dispatch.New(
		dispatch.WithAdmitter(capability.Admitter{}),
		dispatch.WithEventSink(parts.sink),
		dispatch.WithHook(capability.NewContainmentGate(parts.sandbox)),
		dispatch.WithHook(brk),
	)

	runID := parts.sess.ID()
	// Bind the run's grant and brake onto the context the server dispatches under, the
	// same bindings the mission executor sets before it dispatches. The server holds no
	// grant of its own; it propagates this context into every governed call.
	ctx = capability.Into(ctx, grant)
	ctx = brakes.Into(ctx, runID)

	srv := mcp.NewServer(d, parts.toolset,
		mcp.WithGoal(runID),
		mcp.WithInfo(mcp.Info{Name: "flynn", Version: version.String()}))

	mode := "read-write"
	if readOnly {
		mode = "read-only"
	}
	_, _ = fmt.Fprintf(logw, "flynn mcp: serving %d tools (%s) in %s on stdio; session %s\n",
		len(parts.toolset), mode, dir, runID)

	return srv.Serve(ctx, in, out)
}
