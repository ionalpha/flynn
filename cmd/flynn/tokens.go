package main

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/ionalpha/flynn/session"
)

// renderTokens writes a breakdown of a run's cumulative token cost: total input and
// how much of it was served from cache rather than reprocessed, the output, and any
// cache writes, with a note on why input grows across a conversation. It makes a large
// "in" number legible instead of mysterious.
func renderTokens(w io.Writer, u session.Usage, turns int) {
	if u.InputTokens == 0 && u.OutputTokens == 0 {
		_, _ = fmt.Fprintln(w, "no tokens used yet; run a turn first")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
	_, _ = fmt.Fprintf(tw, "tokens this run (%d turns):\n", turns)
	_, _ = fmt.Fprintf(tw, "  input\t%s\n", humanTokens(u.InputTokens))
	if u.InputTokens > 0 && u.CacheReadTokens > 0 {
		pct := u.CacheReadTokens * 100 / u.InputTokens
		_, _ = fmt.Fprintf(tw, "  from cache\t%s\t%d%% of input, reused not reprocessed\n", humanTokens(u.CacheReadTokens), pct)
	}
	if u.CacheWriteTokens > 0 {
		_, _ = fmt.Fprintf(tw, "  cache write\t%s\n", humanTokens(u.CacheWriteTokens))
	}
	_, _ = fmt.Fprintf(tw, "  output\t%s\n", humanTokens(u.OutputTokens))
	_ = tw.Flush()
	_, _ = fmt.Fprintln(w, "input counts each turn's full context: a longer conversation resends it every turn, most of it served from cache.")
}
