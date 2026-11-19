package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/sandbox"
	"github.com/ionalpha/flynn/skill"
	"github.com/ionalpha/flynn/skill/bundled"
	"github.com/ionalpha/flynn/skill/skillab"
	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// versionRefs are the operator's --candidate and --against values. An empty candidate
// is the skill as installed; an empty against is no skill at all.
type versionRefs struct {
	candidate, against string
}

// skillVersion is one version of a skill laid out as a pack, so a run reads its
// references from the same tree as its body. A nil version is the skill as installed.
type skillVersion struct {
	label string
	fsys  fs.FS
	root  string
	skill state.Skill
	dir   string
}

func (v *skillVersion) close() {
	if v != nil && v.dir != "" {
		_ = os.RemoveAll(v.dir)
	}
}

// refPattern is every character a git ref or a path inside it may use here. The
// strings reach git through a shell line, and the sandbox takes nothing else, so the
// charset is what keeps an operator's typo, or a file name in a repository they
// cloned, from becoming a second command. It starts with a letter or digit, so a ref
// cannot be read as an option.
var refPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

func validRef(ref string) error {
	if len(ref) > 200 || !refPattern.MatchString(ref) || strings.Contains(ref, "..") {
		return fmt.Errorf("%q is neither a path that exists nor a git ref made of letters, digits and . _ / -", ref)
	}
	return nil
}

const (
	maxVersionFiles = 256
	gitTimeout      = 30 * time.Second
)

// resolveVersion turns ref into a skill version: a SKILL.md file, a directory holding
// one, or a git ref of the repository in the current directory. The pack it builds is
// a temporary copy, so a baseline's references come from the baseline.
func resolveVersion(ctx context.Context, slug, ref string) (*skillVersion, error) {
	tmp, err := os.MkdirTemp("", "flynn-ab-version-")
	if err != nil {
		return nil, err
	}
	v := &skillVersion{label: slug + " @ " + ref, fsys: os.DirFS(tmp), root: bundled.Root, dir: tmp}
	dest := filepath.Join(tmp, bundled.Root, slug)
	if err := fillVersion(ctx, slug, ref, dest); err != nil {
		v.close()
		return nil, fmt.Errorf("version %s of %s: %w", ref, slug, err)
	}
	pack, err := skillmd.Load(v.fsys, path.Join(bundled.Root, slug))
	if err != nil {
		v.close()
		return nil, fmt.Errorf("version %s of %s: %w", ref, slug, err)
	}
	if v.skill, err = skill.FromPack(pack, state.BundledScope); err != nil {
		v.close()
		return nil, fmt.Errorf("version %s of %s: %w", ref, slug, err)
	}
	return v, nil
}

func fillVersion(ctx context.Context, slug, ref, dest string) error {
	info, err := os.Stat(ref)
	switch {
	case err == nil:
		dir := ref
		if !info.IsDir() {
			if filepath.Base(ref) != "SKILL.md" {
				return fmt.Errorf("%s is a file, and only a SKILL.md names a version", ref)
			}
			dir = filepath.Dir(ref)
		}
		return os.CopyFS(dest, os.DirFS(dir))
	case !errors.Is(err, fs.ErrNotExist):
		return err
	}
	if err := validRef(ref); err != nil {
		return err
	}
	return readGitTree(ctx, ref, path.Join("skill", "bundled", bundled.Root, slug), dest)
}

// readGitTree writes the files of dir at ref into dest. Git runs through the sandbox,
// which takes a shell line, so every string on it has passed refPattern. A binary
// file is not supported: the sandbox returns text.
func readGitTree(ctx context.Context, ref, dir, dest string) error {
	sb, err := sandbox.NewLocal(".", sandbox.WithExecTimeout(gitTimeout))
	if err != nil {
		return err
	}
	defer func() { _ = sb.Close() }()
	listed, err := sb.Exec(ctx, sandbox.Command{Line: "git ls-tree -r --name-only --full-tree " + ref + " -- " + dir})
	if err != nil || listed.ExitCode != 0 {
		return fmt.Errorf("git could not list %s at %s: %v %s", dir, ref, err, strings.TrimSpace(listed.Output))
	}
	files := strings.Fields(listed.Output)
	if len(files) == 0 {
		return fmt.Errorf("%s has no %s", ref, dir)
	}
	if len(files) > maxVersionFiles {
		return fmt.Errorf("%s holds more than %d files", dir, maxVersionFiles)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o750); err != nil {
		return err
	}
	root, err := os.OpenRoot(filepath.Dir(dest))
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	for _, name := range files {
		if err := validRef(name); err != nil || !strings.HasPrefix(name, dir+"/") {
			return fmt.Errorf("repository path %q cannot be read safely", name)
		}
		shown, err := sb.Exec(ctx, sandbox.Command{Line: "git show " + ref + ":" + name})
		if err != nil || shown.ExitCode != 0 {
			return fmt.Errorf("git could not read %s at %s: %v", name, ref, err)
		}
		rel := filepath.Join(filepath.Base(dest), filepath.FromSlash(strings.TrimPrefix(name, dir+"/")))
		if err := root.MkdirAll(filepath.Dir(rel), 0o750); err != nil {
			return err
		}
		if err := root.WriteFile(rel, []byte(shown.Output), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// install makes v the skill the store offers, replacing the pack's own copy of the
// same slug.
func (v *skillVersion) install(ctx context.Context, store state.SkillStore) error {
	prev, err := store.Get(ctx, v.skill.Slug)
	if err == nil && prev.Scope == state.BundledScope {
		v.skill.ID = prev.ID
	}
	_, err = store.Upsert(ctx, v.skill)
	return err
}

// meteredModel counts what a run costs and writes it down turn by turn. The cost is
// read from the responses and never estimated. A call that returned an error is not
// counted, because it reported no usage and the harness would be inventing one.
type meteredModel struct {
	inner llm.Model

	mu    sync.Mutex
	cost  skillab.Cost
	calls int
	log   strings.Builder
	last  string
}

func newMeteredModel(inner llm.Model) *meteredModel { return &meteredModel{inner: inner} }

// maxLoggedBlock bounds one block of the transcript, so a tool that printed a large
// file does not make the record larger than the run.
const maxLoggedBlock = 16 << 10

// Generate records the request's new messages and the response. The first call is
// recorded whole; later calls carry the history again, possibly pruned or compacted by
// the loop, so only the final message is new.
func (m *meteredModel) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	resp, err := m.inner.Generate(ctx, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.calls == 0 {
		for _, msg := range req.Messages {
			writeMessage(&m.log, msg)
		}
	} else if n := len(req.Messages); n > 0 {
		writeMessage(&m.log, req.Messages[n-1])
	}
	m.calls++
	if err != nil {
		_, _ = fmt.Fprintf(&m.log, "[call failed: %v]\n", err)
		return resp, err
	}
	writeMessage(&m.log, resp.Message)
	m.last = resp.Message.TextContent()
	m.cost.Turns++
	m.cost.InputTokens += resp.Usage.InputTokens
	m.cost.OutputTokens += resp.Usage.OutputTokens
	m.cost.CacheReadTokens += resp.Usage.CacheReadTokens
	m.cost.CacheWriteTokens += resp.Usage.CacheWriteTokens
	return resp, nil
}

func (m *meteredModel) snapshot() (skillab.Cost, string, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cost, m.log.String(), m.last
}

func writeMessage(b *strings.Builder, msg llm.Message) {
	_, _ = fmt.Fprintf(b, "[%s]\n", msg.Role)
	for _, blk := range msg.Blocks {
		switch {
		case blk.Kind == llm.KindText:
			b.WriteString(clip(blk.Text))
		case blk.ToolUse != nil:
			_, _ = fmt.Fprintf(b, "call %s %s", blk.ToolUse.Name, clip(string(blk.ToolUse.Input)))
		case blk.ToolResult != nil:
			_, _ = fmt.Fprintf(b, "result %s", clip(blk.ToolResult.Content))
		default:
			continue
		}
		b.WriteString("\n")
	}
}

func clip(s string) string {
	if len(s) <= maxLoggedBlock {
		return s
	}
	return s[:maxLoggedBlock] + "\n[cut]"
}
