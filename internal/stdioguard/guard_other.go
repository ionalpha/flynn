//go:build !unix && !windows

package stdioguard

import (
	"errors"
	"os"
)

// redirectStdout refuses on a platform that cannot repoint standard output, because
// serving frames on an unguarded stdout reintroduces the corruption this package
// prevents.
func redirectStdout() (frames, strays *os.File, err error) {
	return nil, nil, errors.New("stdioguard: not supported on this platform")
}
