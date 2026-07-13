package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/archetype"
	"github.com/ionalpha/flynn/internal/instance"
	"github.com/ionalpha/flynn/resource"
)

// seedAgents writes agents into the durable store under dataDir and closes it, so the
// read commands (which open the store themselves) see a store a previous command left
// behind, the way they do in the binary.
func seedAgents(t *testing.T, dataDir string, specs map[string]string) {
	t.Helper()
	ctx := context.Background()
	store, err := openDataStore(ctx, dataDir)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer func() { _ = store.Close() }()
	rs := store.Resources(mustRegistry(t))
	for name, spec := range specs {
		if _, err := rs.Put(ctx, resource.Resource{
			APIVersion: archetype.GroupVersion,
			Kind:       archetype.Kind,
			Name:       name,
			Spec:       json.RawMessage(spec),
		}); err != nil {
			t.Fatalf("put %s: %v", name, err)
		}
	}
}

// TestGetListsWhatIsStored: `flynn get agents` reads the durable store a previous
// command wrote and projects the kind's curated columns, and an empty kind says so
// rather than printing a bare header.
func TestGetListsWhatIsStored(t *testing.T) {
	dir := t.TempDir()
	seedAgents(t, dir, map[string]string{
		"researcher": `{"model":"anthropic:claude-opus-4-8","driver":"native"}`,
	})

	var out bytes.Buffer
	if err := getResources(&out, []string{"agents"}, dir); err != nil {
		t.Fatalf("get agents: %v", err)
	}
	for _, want := range []string{"NAME", "MODEL", "DRIVER", "researcher", "anthropic:claude-opus-4-8", "native"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("get agents missing %q:\n%s", want, out.String())
		}
	}

	// A kind with nothing stored is reported, not printed as an empty table.
	var empty bytes.Buffer
	if err := getResources(&empty, []string{"runs"}, dir); err != nil {
		t.Fatalf("get runs: %v", err)
	}
	if !strings.Contains(empty.String(), "no resources found") {
		t.Fatalf("an empty kind printed %q", empty.String())
	}
}

// TestGetInstancesShowsTheLiveProcess: listing instances registers this process's own
// Instance record first, so the running flynn always appears in its own listing even on
// a store that has never seen one.
func TestGetInstancesShowsTheLiveProcess(t *testing.T) {
	dir := t.TempDir()
	var out bytes.Buffer
	if err := getResources(&out, []string{"instances"}, dir); err != nil {
		t.Fatalf("get instances: %v", err)
	}
	host, _ := os.Hostname()
	if !strings.Contains(out.String(), "HOST") || !strings.Contains(out.String(), host) {
		t.Fatalf("the live process is missing from its own instance listing (host %q):\n%s", host, out.String())
	}
}

// TestGetRefusesWhatItCannotRead: no kind is a usage error, and an unknown kind names
// the kinds that do exist rather than failing blankly.
func TestGetRefusesWhatItCannotRead(t *testing.T) {
	dir := t.TempDir()
	if err := getResources(&bytes.Buffer{}, nil, dir); err == nil {
		t.Fatal("get with no kind must be a usage error")
	}
	err := getResources(&bytes.Buffer{}, []string{"widgets"}, dir)
	if err == nil {
		t.Fatal("get of an unknown kind must be refused")
	}
	if !strings.Contains(err.Error(), "widgets") || !strings.Contains(err.Error(), "agents") {
		t.Fatalf("the refusal must name the unknown kind and the known ones, got %q", err)
	}
}

// TestDescribeShowsAResourceAndItsHistory: describe resolves a resource by name, prints
// its identity and projected columns, and refuses a name that is not there.
func TestDescribeShowsAResourceAndItsHistory(t *testing.T) {
	dir := t.TempDir()
	seedAgents(t, dir, map[string]string{"shipper": `{"model":"openai:gpt-5.5","driver":"native"}`})

	var out bytes.Buffer
	if err := describeResource(&out, []string{"agents", "shipper"}, dir); err != nil {
		t.Fatalf("describe: %v", err)
	}
	got := out.String()
	for _, want := range []string{"Kind:", "Agent", "Name:", "shipper", "ID:", "MODEL:", "openai:gpt-5.5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("describe missing %q:\n%s", want, got)
		}
	}

	if err := describeResource(&out, []string{"agents"}, dir); err == nil {
		t.Fatal("describe without a name must be a usage error")
	}
	if err := describeResource(&out, []string{"widgets", "x"}, dir); err == nil {
		t.Fatal("describe of an unknown kind must be refused")
	}
	err := describeResource(&out, []string{"agents", "ghost"}, dir)
	if err == nil || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("describe of an absent resource = %v, want a refusal naming it", err)
	}
}

// TestDiffComparesTwoResourcesOfAKind: diff resolves both sides, reports the fields
// that differ, and refuses when either side is missing.
func TestDiffComparesTwoResourcesOfAKind(t *testing.T) {
	dir := t.TempDir()
	seedAgents(t, dir, map[string]string{
		"a": `{"model":"anthropic:claude-opus-4-8","driver":"native"}`,
		"b": `{"model":"openai:gpt-5.5","driver":"native"}`,
	})

	var out bytes.Buffer
	if err := diffResources(&out, []string{"agents", "a", "b"}, dir); err != nil {
		t.Fatalf("diff: %v", err)
	}
	got := out.String()
	for _, want := range []string{"FIELD", "spec.model", "anthropic:claude-opus-4-8", "openai:gpt-5.5"} {
		if !strings.Contains(got, want) {
			t.Fatalf("diff missing %q:\n%s", want, got)
		}
	}
	// The field both agents share is not a difference.
	if strings.Contains(got, "spec.driver") {
		t.Fatalf("diff reported an identical field as a difference:\n%s", got)
	}

	if err := diffResources(&out, []string{"agents", "a"}, dir); err == nil {
		t.Fatal("diff with one side must be a usage error")
	}
	if err := diffResources(&out, []string{"widgets", "a", "b"}, dir); err == nil {
		t.Fatal("diff of an unknown kind must be refused")
	}
	if err := diffResources(&out, []string{"agents", "ghost", "b"}, dir); err == nil {
		t.Fatal("diff must refuse an absent left side")
	}
	if err := diffResources(&out, []string{"agents", "a", "ghost"}, dir); err == nil {
		t.Fatal("diff must refuse an absent right side")
	}
}

// TestKindAliasesAreTheOnesTheRefusalOffers: the alias list an unknown kind is answered
// with is sorted, deduplicated, and actually resolvable, so every suggestion works.
func TestKindAliasesAreTheOnesTheRefusalOffers(t *testing.T) {
	aliases := kindAliases()
	if len(aliases) == 0 {
		t.Fatal("no aliases offered")
	}
	reg := mustRegistry(t)
	seen := map[string]bool{}
	for i, a := range aliases {
		if seen[a] {
			t.Fatalf("alias %q offered twice", a)
		}
		seen[a] = true
		if i > 0 && aliases[i-1] > a {
			t.Fatalf("aliases are not sorted: %q before %q", aliases[i-1], a)
		}
		if _, ok := resolveKind(reg, a); !ok {
			t.Fatalf("offered alias %q does not resolve", a)
		}
	}
	for _, want := range []string{"agents", "instances", "runs", "services"} {
		if !seen[want] {
			t.Fatalf("alias list is missing %q: %v", want, aliases)
		}
	}
}

// TestRegisterLocalInstanceIsUpsertNotDuplicate: registering the live process twice
// leaves one record, so a repeated `flynn get instances` does not grow the store.
func TestRegisterLocalInstanceIsUpsertNotDuplicate(t *testing.T) {
	ctx := context.Background()
	store, err := openStore(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	rs := store.Resources(mustRegistry(t))

	registerLocalInstance(ctx, "node-1", rs)
	registerLocalInstance(ctx, "node-1", rs)

	all, err := rs.ListAll(ctx, instance.Kind, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("registering the same instance twice left %d records, want 1", len(all))
	}
	if all[0].Name != "node-1" {
		t.Fatalf("instance name = %q", all[0].Name)
	}
}
