package approval

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// NonceStore enforces the single-use property of an approval: once a nonce has
// been spent, it can never be spent again, so a captured approval cannot be
// replayed. Seen is the non-committing check the verifier uses while deciding
// whether a quorum is met; Use is the commit it makes only once the action is
// authorized, so a partial or failed attempt does not burn a valid approval's
// nonce. An implementation must be safe for concurrent use. The default MemStore
// keeps the spent set in memory; a fleet that must survive a restart supplies a
// durable store behind the same port.
type NonceStore interface {
	// Seen reports whether nonce has already been spent.
	Seen(ctx context.Context, nonce string) (bool, error)
	// Use marks nonce spent, returning ErrNonceUsed if it already was. It is the
	// atomic test-and-set that makes single-use hold even under concurrency.
	Use(ctx context.Context, nonce string) error
}

// ErrNonceUsed means an approval's nonce has already been spent: the approval is a
// replay and is refused.
var ErrNonceUsed = fault.New(fault.Forbidden, "approval_nonce_used", "approval: nonce already used (replay)")

// MemStore is the default in-memory NonceStore, safe for concurrent use. Spent
// nonces last for the life of the process; they do not survive a restart.
type MemStore struct {
	mu    sync.Mutex
	spent map[string]struct{}
}

// NewMemStore builds an empty in-memory nonce store.
func NewMemStore() *MemStore { return &MemStore{spent: map[string]struct{}{}} }

// Seen reports whether nonce has been spent.
func (s *MemStore) Seen(_ context.Context, nonce string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.spent[nonce]
	return ok, nil
}

// Use marks nonce spent, refusing a replay.
func (s *MemStore) Use(_ context.Context, nonce string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.spent[nonce]; ok {
		return ErrNonceUsed
	}
	s.spent[nonce] = struct{}{}
	return nil
}

var _ NonceStore = (*MemStore)(nil)
