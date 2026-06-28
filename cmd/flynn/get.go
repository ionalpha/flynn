package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/ionalpha/flynn/archetype"
	"github.com/ionalpha/flynn/controlplane"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/instance"
	"github.com/ionalpha/flynn/internal/version"
	"github.com/ionalpha/flynn/resource"
)

// cpKind binds a resource kind to how the read surface displays it.
type cpKind struct {
	kind       string
	descriptor controlplane.Descriptor
}

// knownKinds are the kinds with curated columns. Any other registered kind is
// still listable and describable through a name-only fallback, so the surface
// never hides a kind, it just shows less of one it has no columns for.
func knownKinds() map[string]cpKind {
	col := func(h string, p func(resource.Resource) string) controlplane.Column {
		return controlplane.Column{Header: h, Project: p}
	}
	mk := func(kind string, cols ...controlplane.Column) cpKind {
		return cpKind{kind: kind, descriptor: controlplane.Descriptor{Kind: kind, Columns: cols}}
	}
	objective := func(r resource.Resource) string {
		return oneLine(controlplane.SpecField("objective")(r), 50)
	}
	entries := []struct {
		aliases []string
		k       cpKind
	}{
		{[]string{"instances", "instance"}, mk(
			instance.Kind,
			col("NAME", controlplane.Name()),
			col("HOST", controlplane.SpecField("host")),
			col("VERSION", controlplane.SpecField("version")),
			col("STATE", controlplane.StatusField("state")),
		)},
		{[]string{"agents", "agent"}, mk(
			archetype.Kind,
			col("NAME", controlplane.Name()),
			col("MODEL", controlplane.SpecField("model")),
			col("DRIVER", controlplane.SpecField("driver")),
		)},
		{[]string{"runs", "run", "goals", "goal"}, mk(
			goal.Kind,
			col("NAME", controlplane.Name()),
			col("PHASE", controlplane.StatusField("phase")),
			col("STEPS", controlplane.StatusField("steps")),
			col("OBJECTIVE", objective),
		)},
	}
	m := map[string]cpKind{}
	for _, e := range entries {
		for _, a := range e.aliases {
			m[a] = e.k
		}
	}
	return m
}

// resolveKind maps a user-typed alias to a display kind: a curated one when known,
// otherwise any registered kind by its singular or plural name with a name-only
// descriptor. It returns whether the alias resolved.
func resolveKind(reg *resource.Registry, alias string) (cpKind, bool) {
	alias = strings.ToLower(strings.TrimSpace(alias))
	if k, ok := knownKinds()[alias]; ok {
		return k, true
	}
	for _, k := range reg.Kinds() {
		if alias == strings.ToLower(k.Name) || alias == k.Singular || alias == k.Plural {
			return cpKind{
				kind: k.Name,
				descriptor: controlplane.Descriptor{
					Kind:    k.Name,
					Columns: []controlplane.Column{{Header: "NAME", Project: controlplane.Name()}},
				},
			}, true
		}
	}
	return cpKind{}, false
}

// dispatchGet implements `flynn get <kind>`: it lists every resource of the kind
// in a table. Listing instances first refreshes this process's own Instance record
// so the live process always appears.
func dispatchGet(args []string, dataDir string) error {
	if len(args) < 1 {
		return errors.New("usage: flynn get <kind> (instances, agents, runs, ...)")
	}
	ctx := context.Background()
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = durable.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	store := durable.Resources(reg)

	ck, ok := resolveKind(reg, args[0])
	if !ok {
		return fmt.Errorf("unknown kind %q; try one of: %s", args[0], strings.Join(kindAliases(), ", "))
	}
	if ck.kind == instance.Kind {
		registerLocalInstance(ctx, durable.InstanceID(), store)
	}
	table, err := controlplane.List(ctx, store, ck.descriptor, nil)
	if err != nil {
		return err
	}
	renderTable(os.Stdout, table)
	return nil
}

// dispatchDescribe implements `flynn describe <kind> <name-or-id>`: it prints the
// resource's fields and its recent change history from the event log.
func dispatchDescribe(args []string, dataDir string) error {
	if len(args) < 2 {
		return errors.New("usage: flynn describe <kind> <name-or-id>")
	}
	ctx := context.Background()
	durable, err := openDataStore(ctx, dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = durable.Close() }()
	reg, err := missionRegistry()
	if err != nil {
		return err
	}
	store := durable.Resources(reg)

	ck, ok := resolveKind(reg, args[0])
	if !ok {
		return fmt.Errorf("unknown kind %q; try one of: %s", args[0], strings.Join(kindAliases(), ", "))
	}
	id, err := resolveID(ctx, store, ck.kind, args[1])
	if err != nil {
		return err
	}
	detail, err := controlplane.Describe(ctx, store, durable.Log(), ck.descriptor, id, 20)
	if err != nil {
		return err
	}
	renderDetail(os.Stdout, ck.kind, detail)
	return nil
}

// resolveID accepts either a resource id or a logical name and returns the id. It
// tries the id first (the unambiguous handle), then the name in the global scope.
func resolveID(ctx context.Context, store resource.Store, kind, ref string) (string, error) {
	if _, err := store.GetByID(ctx, ref); err == nil {
		return ref, nil
	} else if !errors.Is(err, resource.ErrNotFound) {
		return "", err
	}
	r, err := store.Get(ctx, kind, resource.Scope{}, ref)
	if err != nil {
		if errors.Is(err, resource.ErrNotFound) {
			return "", fmt.Errorf("no %s found named or id %q", kind, ref)
		}
		return "", err
	}
	return r.ID, nil
}

// registerLocalInstance upserts this process's Instance record so `get instances`
// always shows the live process. A failure to register is not fatal to a read: the
// command still lists whatever is stored.
func registerLocalInstance(ctx context.Context, id string, store resource.Store) {
	host, _ := os.Hostname()
	_, _ = instance.Register(ctx, store, resource.Scope{}, id, instance.Spec{
		Host:    host,
		Version: version.String(),
	})
}

func kindAliases() []string {
	seen := map[string]bool{}
	var out []string
	for a := range knownKinds() {
		if !seen[a] {
			seen[a] = true
			out = append(out, a)
		}
	}
	sort.Strings(out)
	return out
}

// renderTable writes a table as aligned columns, or a friendly note when empty.
func renderTable(out io.Writer, t controlplane.Table) {
	if len(t.Rows) == 0 {
		_, _ = fmt.Fprintln(out, "no resources found")
		return
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, strings.Join(t.Columns, "\t"))
	for _, r := range t.Rows {
		_, _ = fmt.Fprintln(w, strings.Join(r.Cells, "\t"))
	}
	_ = w.Flush()
}

// renderDetail prints a single resource's identity, projected columns, and recent
// change history.
func renderDetail(out io.Writer, kind string, d controlplane.Detail) {
	_, _ = fmt.Fprintf(out, "Kind:    %s\n", kind)
	_, _ = fmt.Fprintf(out, "Name:    %s\n", d.Resource.Name)
	_, _ = fmt.Fprintf(out, "ID:      %s\n", d.Resource.ID)
	for i, h := range d.Columns {
		if i < len(d.Row.Cells) {
			_, _ = fmt.Fprintf(out, "%-8s %s\n", h+":", d.Row.Cells[i])
		}
	}
	if len(d.Events) == 0 {
		return
	}
	_, _ = fmt.Fprintln(out, "\nRecent events:")
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "SEQ\tTYPE\tACTOR\tTIME")
	for _, ev := range d.Events {
		_, _ = fmt.Fprintf(w, "%d\t%s\t%s\t%s\n", ev.Seq, ev.Type, ev.Actor, ev.Time.UTC().Format("2006-01-02T15:04:05Z"))
	}
	_ = w.Flush()
}
