package main

import (
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/fuzzy"
)

// commandCompleter completes a slash-command line: the command name while it is being
// typed, and for /model the model id from the catalog, so a user can pick a model
// without typing it in full.
type commandCompleter struct {
	models []string
}

func newCommandCompleter() *commandCompleter {
	return &commandCompleter{models: allCatalogModelIDs()}
}

// Suggest implements app.CommandCompleter: it completes the command word until a space
// is typed, then the argument for the commands that take one.
func (c *commandCompleter) Suggest(line string) []app.CommandCandidate {
	line = strings.TrimLeft(line, " ")
	if !strings.HasPrefix(line, "/") {
		return nil
	}
	name, arg, hasSpace := strings.Cut(line, " ")
	if hasSpace {
		if name == "/model" {
			return c.modelArgs(arg)
		}
		return nil
	}
	return commandNames(name)
}

// commandNames completes the command word. A word that already names a command needs no
// completion (Enter should submit it, and "/model " falls through to argument
// completion), so an exact match returns nothing. The candidates are the same table both
// interfaces dispatch through, so the menu cannot offer a command the session will not
// run. An alias is not offered: "?" is shorter than any completion of it.
func commandNames(partial string) []app.CommandCandidate {
	names := make([]string, 0, len(sessionCommands()))
	for _, cmd := range sessionCommands() {
		if cmd.name == partial {
			return nil
		}
		names = append(names, cmd.name)
	}
	out := make([]app.CommandCandidate, 0, menuLimit)
	for _, name := range fuzzy.Rank(partial, names, menuLimit, nil) {
		apply := name
		if commandTakesArg(name) {
			apply += " "
		}
		out = append(out, app.CommandCandidate{Show: name, Apply: apply})
	}
	return out
}

// modelArgs completes a /model argument against the catalog's model ids. A fully typed
// id needs no completion.
func (c *commandCompleter) modelArgs(partial string) []app.CommandCandidate {
	for _, id := range c.models {
		if id == partial {
			return nil
		}
	}
	out := make([]app.CommandCandidate, 0, menuLimit)
	for _, id := range fuzzy.Rank(partial, c.models, menuLimit, nil) {
		out = append(out, app.CommandCandidate{Show: id, Apply: "/model " + id})
	}
	return out
}

// commandTakesArg reports whether a command name takes an argument, which is the same
// question as whether /help shows it with a placeholder.
func commandTakesArg(name string) bool {
	for _, cmd := range sessionCommands() {
		if cmd.name == name {
			return cmd.arg != ""
		}
	}
	return false
}

// allCatalogModelIDs returns every model id in the catalog, hosted and local, for
// /model completion. A catalog that cannot load yields no ids rather than an error; the
// user can still type an id in full.
func allCatalogModelIDs() []string {
	cat, err := catalog.Load()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(cat.Models))
	for _, m := range cat.Models {
		ids = append(ids, m.ID)
	}
	return ids
}
