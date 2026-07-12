package main

import (
	"context"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

// The notice channel must be reachable as a command. A receiver nobody can invoke is a
// receiver nobody can check.
func TestNoticesIsRegisteredAsACommand(t *testing.T) {
	if _, ok := dataDirCommands["notices"]; !ok {
		t.Fatal("`flynn notices` is not wired into the command table")
	}
}

// A build with no publisher key, and a user who turned the channel off, must both come out
// the same way: nothing on the network, nothing printed, and no error that could fail the
// command the user actually ran.
func TestNoticesAreInertWithoutAKeyOrWithTheOffSwitch(t *testing.T) {
	t.Setenv(notices.OffEnv, "1")
	dir := t.TempDir()

	if err := runNotices(nil, dir); err != nil {
		t.Fatalf("`flynn notices` failed on a disabled channel: %v", err)
	}
	// startupNotices runs before every command. Whatever the state of the machine, it
	// returns, and it does not panic or block.
	startupNotices(context.Background(), dir)
}

func TestNoticesRejectsUnknownFlags(t *testing.T) {
	if err := runNotices([]string{"--everything"}, t.TempDir()); err == nil {
		t.Fatal("an unknown flag was accepted")
	}
}
