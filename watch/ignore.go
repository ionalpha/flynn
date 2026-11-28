package watch

import (
	"path"
	"strings"

	"github.com/ionalpha/flynn/internal/pathglob"
)

// Ignore matches paths against .gitignore rules: # comments, ! negation, trailing
// / for directories, leading / to anchor, * within a segment and ** across them.
// The last matching rule wins, as in git. Nested ignore files are not read.
type Ignore struct {
	rules *pathglob.Ignore
}

// ParseIgnore builds an Ignore from .gitignore contents. Paths match with forward
// slashes on every host.
func ParseIgnore(content []byte) *Ignore {
	return &Ignore{rules: pathglob.ParseIgnore(content)}
}

// Match reports whether the repo-relative path is ignored. isDir lets a
// directory-only rule apply. A walk prunes an ignored directory, so a negation
// cannot re-include a file inside it.
func (ig *Ignore) Match(rel string, isDir bool) bool {
	rel = strings.Trim(path.Clean(strings.ReplaceAll(rel, "\\", "/")), "/")
	if rel == "." {
		return false
	}
	ignored, _ := ig.rules.Decide(rel, isDir)
	return ignored
}
