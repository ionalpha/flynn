package governed

import (
	"context"

	"github.com/ionalpha/flynn/sandbox"
)

// GrantWorkdir makes the directory grant that the first command of a run under
// this policy would otherwise wait for. See [sandbox.Local.GrantRoot].
func (p ShellPolicy) GrantWorkdir(ctx context.Context, workdir string) error {
	sb, err := sandbox.NewLocal(workdir, p.options()...)
	if err != nil {
		return err
	}
	defer func() { _ = sb.Close() }()
	return sb.GrantRoot(ctx)
}
