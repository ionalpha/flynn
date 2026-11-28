package session

import (
	"context"
	"time"

	"github.com/ionalpha/flynn/spine"
)

// GrantState is where a recorded wait on a sandbox grant stands.
type GrantState string

// The states of a [SandboxGrant]. Every wait opens with GrantStarted and closes
// with one of the others, unless the process exits first.
const (
	GrantStarted   GrantState = "started"
	GrantFinished  GrantState = "finished"
	GrantCancelled GrantState = "cancelled"
	GrantFailed    GrantState = "failed"
)

// SandboxGrant is one record of a wait on a sandbox directory grant.
type SandboxGrant struct {
	Dir       string     `json:"dir"`
	State     GrantState `json:"state"`
	ElapsedMS int64      `json:"elapsedMs"`
}

// Elapsed is how long the command had waited when the record was written.
func (g SandboxGrant) Elapsed() time.Duration {
	return time.Duration(g.ElapsedMS) * time.Millisecond
}

// RecordGrant appends a [KindSandboxGrant] event. The record of a cancelled wait
// is written under a context that is already done, so cancellation is detached.
func (s *Session) RecordGrant(ctx context.Context, g SandboxGrant, reason string) {
	s.emit(context.WithoutCancel(ctx), Event{Kind: KindSandboxGrant, Actor: spine.ActorSystem, SandboxGrant: &g, Err: reason})
}
