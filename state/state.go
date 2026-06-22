// Package state defines the persistence and context seam between the
// open-source agent and any host.
//
// This is the open-core boundary: the agent depends only on these interfaces.
// The open agent ships local implementations (e.g. in-memory here, SQLite
// later). A commercial host such as an Ion Alpha instance can provide its own
// implementation backed by a knowledge graph and fleet/federated learning —
// without this package importing the host.
package state

import "context"

// Store is the agent's durable backend for sessions, skills, and memory.
type Store interface {
	// Name identifies the backend (for diagnostics), e.g. "memory", "sqlite".
	Name() string
	// Get returns the value for key. The bool reports whether it was found.
	Get(ctx context.Context, key string) ([]byte, bool, error)
	// Put stores val under key.
	Put(ctx context.Context, key string, val []byte) error
}

// Memory is an in-memory Store so the agent runs with zero setup. It is not
// safe for concurrent use and is intended as the standalone default and for
// tests; durable backends land in follow-up tasks.
type Memory struct {
	m map[string][]byte
}

// NewMemory returns an empty in-memory Store.
func NewMemory() *Memory {
	return &Memory{m: make(map[string][]byte)}
}

// Name implements Store.
func (s *Memory) Name() string { return "memory" }

// Get implements Store.
func (s *Memory) Get(_ context.Context, key string) ([]byte, bool, error) {
	v, ok := s.m[key]
	return v, ok, nil
}

// Put implements Store.
func (s *Memory) Put(_ context.Context, key string, val []byte) error {
	s.m[key] = val
	return nil
}
