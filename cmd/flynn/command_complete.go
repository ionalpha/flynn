package main

import (
	"strings"

	"github.com/ionalpha/flynn/internal/catalog"
	"github.com/ionalpha/flynn/internal/tui/app"
	"github.com/ionalpha/flynn/internal/tui/fuzzy"
)

// completableCommand is a session command the composer can complete, and whether it
// takes an argument (so accepting it leaves a trailing space to type the argument).
type completableCommand struct {
	name string
	arg  bool
}

var completableCommands = []completableCommand{
	{"/model", true},
	{"/models", false},
	{"/tokens", false},
	{"/memory", false},
	{"/remember", true},
	{"/skills", false},
	{"/seal", false},
	{"/verify", false},
	{"/export", false},
	{"/fork", false},
	{"/replay", false},
	{"/compact", false},
	{"/clear", false},
	{"/help", false},
}

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
// completion), so an exact match returns nothing.
func commandNames(partial string) []app.CommandCandidate {
	names := make([]string, 0, len(completableCommands))
	for _, cmd := range completableCommands {
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

// commandTakesArg reports whether a command name takes an argument.
func commandTakesArg(name string) bool {
	for _, cmd := range completableCommands {
		if cmd.name == name {
			return cmd.arg
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
