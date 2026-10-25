package governed

import (
	"context"

	"github.com/ionalpha/flynn/mission"
)

// tee delivers every conversational event to the session's reporter first and the host's
// second. The session is the spine's writer, so the record is complete whatever the
// host's reporter does with the event.
type tee struct {
	record, host mission.Reporter
}

func (t tee) Report(ctx context.Context, ev mission.Event) {
	t.record.Report(ctx, ev)
	t.host.Report(ctx, ev)
}
