//go:build !unix && !windows

package stdioguard

import (
	"errors"
	"os"
)

// redirectStdout reports that this platform has no way to repoint standard output, so a
// transport cannot be started on it. Refusing is the safe answer: serving frames on an
// unguarded stdout would put the corruption this package exists to prevent back.
func redirectStdout() (frames, strays *os.File, err error) {
	return nil, nil, errors.New("stdioguard: not supported on this platform")
}
