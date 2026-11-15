package app

import (
	"github.com/ionalpha/flynn/internal/tui/input"
	"github.com/ionalpha/flynn/internal/tui/screen"
	"github.com/ionalpha/flynn/internal/tui/theme"
)

// completionTrigger starts a completion token; the accepted path lands in the
// prompt as plain text.
const completionTrigger = '@'

// menuMaxRows caps the visible menu; longer lists scroll with the selection.
const menuMaxRows = 6

// menu is the completion popup's state, guarded by the App's mutex and mutated
// only on the event loop goroutine.
type menu struct {
	open  bool
	items []string
	sel   int
	query string
	top   int // first visible row

	// command marks a slash-command menu, where apply holds the whole line to set
	// on accept for each item.
	command bool
	apply   []string
}

// menuKey consumes navigation, accept (Tab, Enter) and dismiss (Escape) keys
// while the menu is open and reports whether it consumed the key.
func (a *App) menuKey(k input.Key) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.menu.open {
		return false
	}
	switch {
	case k.Code == input.KeyUp && k.Mods == 0,
		k.Code == 'p' && k.Mods == input.ModCtrl:
		a.menu.move(-1)
	case k.Code == input.KeyDown && k.Mods == 0,
		k.Code == 'n' && k.Mods == input.ModCtrl:
		a.menu.move(1)
	case k.Code == input.KeyTab && k.Mods == 0,
		k.Code == input.KeyEnter && k.Mods == 0:
		a.acceptLocked()
	case k.Code == input.KeyEsc:
		a.menu = menu{}
	default:
		return false
	}
	return true
}

// move steps the selection with wraparound, keeping it in the visible window.
func (m *menu) move(delta int) {
	n := len(m.items)
	if n == 0 {
		return
	}
	m.sel = (m.sel + delta + n) % n
	if m.sel < m.top {
		m.top = m.sel
	}
	if m.sel >= m.top+menuMaxRows {
		m.top = m.sel - menuMaxRows + 1
	}
}

// acceptLocked splices the selected candidate in and closes the menu. The editor
// re-checks the token, so a buffer that changed under a queued key is a no-op.
func (a *App) acceptLocked() {
	if a.menu.sel < len(a.menu.items) {
		if a.menu.command {
			a.setContentLocked(a.menu.apply[a.menu.sel])
		} else {
			item := a.menu.items[a.menu.sel]
			a.editor.CompleteToken(completionTrigger, item)
			if h := a.cfg.Completer; h != nil {
				a.accepted = append(a.accepted, item)
			}
		}
	}
	a.menu = menu{}
}

// refreshMenu re-derives the menu after the editor consumed an event. It runs
// with the mutex released because the completers are host hooks.
func (a *App) refreshMenu() {
	if a.cfg.Completer == nil && a.cfg.Commands == nil {
		return
	}
	a.mu.Lock()
	var query string
	var active bool
	if a.cfg.Completer != nil {
		query, active = a.editor.Token(completionTrigger)
	}
	unchanged := active && a.menu.open && !a.menu.command && query == a.menu.query
	line := a.editor.Content()
	a.mu.Unlock()

	// An active @-token wins over command completion.
	if active {
		if unchanged {
			return
		}
		items := a.cfg.Completer.Complete(query)
		a.mu.Lock()
		if len(items) == 0 {
			a.menu = menu{}
		} else {
			a.menu = menu{open: true, items: items, query: query}
		}
		a.mu.Unlock()
		return
	}

	if a.cfg.Commands != nil {
		if cands := a.cfg.Commands.Suggest(line); len(cands) > 0 {
			show := make([]string, len(cands))
			apply := make([]string, len(cands))
			for i, c := range cands {
				show[i], apply[i] = c.Show, c.Apply
			}
			a.mu.Lock()
			a.menu = menu{open: true, items: show, apply: apply, query: line, command: true}
			a.mu.Unlock()
			return
		}
	}
	a.mu.Lock()
	a.menu = menu{}
	a.mu.Unlock()
}

// notifyAccepted drains the accepted queue to the Completer with no locks held.
func (a *App) notifyAccepted() {
	a.mu.Lock()
	picked := a.accepted
	a.accepted = nil
	a.mu.Unlock()
	for _, item := range picked {
		a.cfg.Completer.Accepted(item)
	}
}

// menuRowsLocked renders the visible window, each row clipped to the width.
func (a *App) menuRowsLocked() []string {
	m := &a.menu
	end := m.top + menuMaxRows
	if end > len(m.items) {
		end = len(m.items)
	}
	rows := make([]string, 0, end-m.top)
	for i := m.top; i < end; i++ {
		marker, role := "  ", theme.Overlay
		if i == m.sel {
			marker, role = "> ", theme.Selection
		}
		rows = append(rows, a.cfg.Theme.Render(role, screen.Truncate(marker+m.items[i], a.width)))
	}
	return rows
}
