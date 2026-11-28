//go:build windows

package sandbox

import (
	"context"
	"fmt"

	"golang.org/x/sys/windows"
)

// grantRoot makes the same grant on the root that a command makes under the
// Local's tier, with the same identity, so the command finds it in place.
func (l *Local) grantRoot(ctx context.Context) error {
	if l.hostReadable {
		_, err := l.grantWorkdir(ctx, nil)
		return err
	}
	sid, err := createOrDeriveACSID(appContainerMoniker(l.root))
	if err != nil {
		return fmt.Errorf("sandbox: appcontainer profile: %w", err)
	}
	defer func() { _ = windows.FreeSid(sid) }()
	_, err = l.grantWorkdir(ctx, sid)
	return err
}
