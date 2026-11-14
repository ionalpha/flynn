package projectctx

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"github.com/ionalpha/flynn/internal/fsatomic"
)

// FileStore is a [Remembered] kept in one JSON file. It holds one approval per root,
// so approving changed content replaces the earlier approval and the file does not
// grow with every edit to an instruction file.
type FileStore struct {
	path string
	mu   sync.Mutex
}

// NewFileStore returns a store backed by the file at path. The file need not exist.
func NewFileStore(path string) *FileStore { return &FileStore{path: path} }

type storedApprovals struct {
	Version int               `json:"version"`
	Roots   map[string]string `json:"roots"`
}

// Approved reports whether digest is the approved content for root. An unreadable or
// malformed file approves nothing: the failure mode of a damaged store is a question,
// never a silent yes.
func (s *FileStore) Approved(root, digest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		return false
	}
	got, ok := st.Roots[root]
	return ok && got == digest
}

// Approve records digest as the approved content for root.
func (s *FileStore) Approve(root, digest string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.load()
	if err != nil {
		// A damaged file is replaced rather than blocking every future approval; the
		// content it held approved nothing (see Approved).
		st = storedApprovals{}
	}
	if st.Roots == nil {
		st.Roots = map[string]string{}
	}
	st.Version = 1
	st.Roots[root] = digest
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	return fsatomic.WriteFile(s.path, append(b, '\n'), 0o600)
}

func (s *FileStore) load() (storedApprovals, error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return storedApprovals{}, nil
	}
	if err != nil {
		return storedApprovals{}, err
	}
	var st storedApprovals
	if err := json.Unmarshal(b, &st); err != nil {
		return storedApprovals{}, err
	}
	return st, nil
}
