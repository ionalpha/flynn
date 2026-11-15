package approval

import (
	"context"
	"sync"

	"github.com/ionalpha/flynn/fault"
)

// NonceStore keeps an approval single-use. The verifier calls Seen while
// counting a quorum and Use only once the action is authorized, so a failed
// attempt does not burn a valid approval's nonce. Implementations must be safe
// for concurrent use. A fleet that must survive a restart supplies a durable
// store in place of [MemStore].
type NonceStore interface {
	// Seen reports whether nonce has already been spent.
	Seen(ctx context.Context, nonce string) (bool, error)
	// Use marks nonce spent and returns ErrNonceUsed if it already was. It is an
	// atomic test-and-set, so single use holds under concurrency.
	Use(ctx context.Context, nonce string) error
}

// ErrNonceUsed means the approval is a replay.
var ErrNonceUsed = fault.New(fault.Forbidden, "approval_nonce_used", "approval: nonce already used (replay)")

// MemStore is the in-memory NonceStore, safe for concurrent use. Spent nonces do not
// survive a restart.
type MemStore struct {
	mu    sync.Mutex
	spent map[string]struct{}
}

// NewMemStore returns an empty store with no nonce spent.
func NewMemStore() *MemStore { return &MemStore{spent: map[string]struct{}{}} }

// Seen reports whether nonce has been spent; it never errors.
func (s *MemStore) Seen(_ context.Context, nonce string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.spent[nonce]
	return ok, nil
}

// Use spends nonce, returning [ErrNonceUsed] for a replay.
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
