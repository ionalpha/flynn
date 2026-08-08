package notices_test

// How a notice reads on screen. Every severity gets its label and security sorts first,
// so an advisory is not buried under the chatter it arrived with.

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/notices"
)

// TestRenderLabelsEachSeverityAndOrdersSecurityFirst covers every branch of the label
// prefix and the ordering rule: security must not be buried under the chatter.
func TestRenderLabelsEachSeverityAndOrdersSecurityFirst(t *testing.T) {
	ns := []notices.Notice{
		{ID: "i", Severity: notices.Info, Summary: "informational thing"},
		{ID: "d", Severity: notices.Deprecation, Summary: "deprecated thing"},
		{ID: "s", Severity: notices.Security, Summary: "security thing", URL: "https://flynnhq.com/a"},
	}
	var buf bytes.Buffer
	if !notices.Render(&buf, ns, false) {
		t.Fatal("Render reported writing nothing")
	}
	out := buf.String()

	sec := strings.Index(out, "SECURITY: security thing")
	dep := strings.Index(out, "notice:  deprecated thing")
	inf := strings.Index(out, "notice:  informational thing")
	if sec < 0 || dep < 0 || inf < 0 {
		t.Fatalf("a severity was not labelled:\n%s", out)
	}
	if sec >= dep || dep >= inf {
		t.Fatalf("severities are out of order (security must come first):\n%s", out)
	}
	if !strings.Contains(out, "https://flynnhq.com/a") {
		t.Fatalf("the advisory URL was not printed:\n%s", out)
	}

	// Nothing pending and not stale writes nothing at all, so a quiet channel adds no
	// noise to a command's output.
	var quiet bytes.Buffer
	if notices.Render(&quiet, nil, false) {
		t.Fatalf("Render wrote for an empty feed: %q", quiet.String())
	}
	if quiet.Len() != 0 {
		t.Fatalf("Render wrote %q for an empty feed", quiet.String())
	}

	// Staleness alone still writes, because silence would read as all-clear.
	var stale bytes.Buffer
	if !notices.Render(&stale, nil, true) {
		t.Fatal("a stale feed with no notices should still say so")
	}
	if !strings.Contains(stale.String(), "not been refreshed recently") {
		t.Fatalf("stale warning missing: %q", stale.String())
	}
}
