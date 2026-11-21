package selection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/session"
	"github.com/ionalpha/flynn/spine"
	"github.com/ionalpha/flynn/state"
)

func TestPermitsSkill(t *testing.T) {
	tests := []struct {
		name string
		c    Config
		want map[string]bool
	}{
		{"default offers all", Config{}, map[string]bool{"a": true, "b": true}},
		{"bare offers none", Config{Bare: true}, map[string]bool{"a": false, "b": false}},
		{"deny removes one", Config{Deny: []string{"a"}}, map[string]bool{"a": false, "b": true}},
		{"allow names the offer", Config{AllowSet: true, Allow: []string{"a"}}, map[string]bool{"a": true, "b": false}},
		{"empty allow offers none", Config{AllowSet: true, Allow: []string{}}, map[string]bool{"a": false, "b": false}},
		{"bare plus allow offers the named", Config{Bare: true, AllowSet: true, Allow: []string{"a"}}, map[string]bool{"a": true, "b": false}},
		{"deny beats allow", Config{AllowSet: true, Allow: []string{"a", "b"}, Deny: []string{"a"}}, map[string]bool{"a": false, "b": true}},
		{"deny beats allow under bare", Config{Bare: true, AllowSet: true, Allow: []string{"a"}, Deny: []string{"a"}}, map[string]bool{"a": false, "b": false}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			for slug, want := range tc.want {
				if got := tc.c.PermitsSkill(slug); got != want {
					t.Errorf("PermitsSkill(%q) = %v, want %v", slug, got, want)
				}
			}
		})
	}
}

func TestOffersSkills(t *testing.T) {
	tests := []struct {
		name string
		c    Config
		want bool
	}{
		{"default", Config{}, true},
		{"bare", Config{Bare: true}, false},
		{"deny alone", Config{Deny: []string{"a"}}, true},
		{"allow none", Config{AllowSet: true, Allow: []string{}}, false},
		{"allow all denied", Config{AllowSet: true, Allow: []string{"a"}, Deny: []string{"a"}}, false},
		{"bare with allow", Config{Bare: true, AllowSet: true, Allow: []string{"a"}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.c.OffersSkills(); got != tc.want {
				t.Errorf("OffersSkills = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseList(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr string
	}{
		{"a,b", []string{"a", "b"}, ""},
		{" a , b ,a", []string{"a", "b"}, ""},
		{"none", []string{}, ""},
		{"", nil, "comma-separated"},
		{"a,,b", nil, "empty name"},
		{"a,none", nil, "cannot be mixed"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ParseList("--skills", tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ParseList = %#v, %v; want %#v", got, err, tc.want)
			}
		})
	}
}

func TestOver(t *testing.T) {
	file := Config{Bare: true, BareSet: true, AllowSet: true, Allow: []string{"a"}, Deny: []string{"x"}, ToolsSet: true, Tools: []string{"read"}}
	tests := []struct {
		name string
		flag Config
		want Config
	}{
		{"flags state nothing", Config{}, file},
		{"flag turns bare off", Config{BareSet: true}, Config{Bare: false, BareSet: true, AllowSet: true, Allow: []string{"a"}, Deny: []string{"x"}, ToolsSet: true, Tools: []string{"read"}}},
		{"flag allow replaces file allow", Config{AllowSet: true, Allow: []string{"b"}}, Config{Bare: true, BareSet: true, AllowSet: true, Allow: []string{"b"}, Deny: []string{"x"}, ToolsSet: true, Tools: []string{"read"}}},
		{"deny lists join", Config{Deny: []string{"y", "x"}}, Config{Bare: true, BareSet: true, AllowSet: true, Allow: []string{"a"}, Deny: []string{"x", "y"}, ToolsSet: true, Tools: []string{"read"}}},
		{"flag tools replace file tools", Config{ToolsSet: true, Tools: []string{}}, Config{Bare: true, BareSet: true, AllowSet: true, Allow: []string{"a"}, Deny: []string{"x"}, ToolsSet: true, Tools: []string{}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.flag.Over(file); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Over = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestChecksNameTheValidOnes(t *testing.T) {
	err := Config{Allow: []string{"debug", "nope"}}.CheckSkills([]string{"tdd", "debug"})
	if err == nil || !strings.Contains(err.Error(), `"nope"`) || !strings.Contains(err.Error(), "debug, tdd") {
		t.Errorf("unknown allowed skill: err = %v", err)
	}
	err = Config{Deny: []string{"gone"}}.CheckSkills([]string{"tdd"})
	if err == nil || !strings.Contains(err.Error(), "--no-skill") || !strings.Contains(err.Error(), "tdd") {
		t.Errorf("unknown denied skill: err = %v", err)
	}
	if err := (Config{Allow: []string{"tdd"}, Deny: []string{"tdd"}}).CheckSkills([]string{"tdd"}); err != nil {
		t.Errorf("known names refused: %v", err)
	}
	err = Config{Tools: []string{"read", "teleport"}}.CheckTools([]string{"write", "read"})
	if err == nil || !strings.Contains(err.Error(), `"teleport"`) || !strings.Contains(err.Error(), "read, write") {
		t.Errorf("unknown tool: err = %v", err)
	}
	if err := (Config{}).CheckTools(nil); err != nil {
		t.Errorf("no tools named: %v", err)
	}
}

func seed(t *testing.T) state.SkillStore {
	t.Helper()
	store := state.NewMemory().Skills()
	for _, slug := range []string{"alpha", "beta", "gamma"} {
		if _, err := store.Upsert(context.Background(), state.Skill{Slug: slug, Name: slug, Description: "deploy " + slug, Body: "body of " + slug}); err != nil {
			t.Fatal(err)
		}
	}
	return store
}

func slugs(in []state.Skill) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		out = append(out, s.Slug)
	}
	return out
}

func TestFilteredStore(t *testing.T) {
	ctx := context.Background()
	inner := seed(t)
	view := Skills(inner, Config{Deny: []string{"beta"}})

	got, err := view.Search(ctx, "deploy", 0)
	if err != nil || !reflect.DeepEqual(slugs(got), []string{"alpha", "gamma"}) {
		t.Errorf("Search = %v, %v; want alpha and gamma", slugs(got), err)
	}
	// The denied skill must not take a slot: two permitted skills fill a limit of two.
	got, err = view.Search(ctx, "deploy", 2)
	if err != nil || len(got) != 2 {
		t.Errorf("Search with limit 2 = %v, %v; want the two permitted skills", slugs(got), err)
	}
	listed, err := view.List(ctx, state.Scope{})
	if err != nil || !reflect.DeepEqual(slugs(listed), []string{"alpha", "gamma"}) {
		t.Errorf("List = %v, %v", slugs(listed), err)
	}
	if _, err := view.Get(ctx, "beta"); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get(denied slug) err = %v, want ErrNotFound", err)
	}
	if _, err := view.Get(ctx, "alpha"); err != nil {
		t.Errorf("Get(permitted) err = %v", err)
	}

	// The store accepts an id in place of a slug; the filter must look at what came back.
	beta, err := inner.Get(ctx, "beta")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := view.Get(ctx, beta.ID); !errors.Is(err, state.ErrNotFound) {
		t.Errorf("Get(denied id) err = %v, want ErrNotFound", err)
	}

	// Selection is a view: the denied skill is still stored.
	if _, err := inner.Get(ctx, "beta"); err != nil {
		t.Errorf("the underlying store lost the denied skill: %v", err)
	}
}

func TestFilteredStoreWritesPassThrough(t *testing.T) {
	ctx := context.Background()
	inner := seed(t)
	view := Skills(inner, Config{Bare: true})
	if _, err := view.Upsert(ctx, state.Skill{Slug: "delta", Name: "delta", Description: "d", Body: "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := inner.Get(ctx, "delta"); err != nil {
		t.Errorf("a write through the view did not reach the store: %v", err)
	}
}

func TestSkillsWithoutRulesIsTheStore(t *testing.T) {
	inner := seed(t)
	if view := Skills(inner, Config{}); view != inner {
		t.Error("a default selection wrapped the store")
	}
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		want    Config
		wantErr string
	}{
		{"missing file", "", Config{}, ""},
		{
			"all fields", `{"bare":true,"skills":["a"],"no_skills":["b"],"tools":["read"]}`,
			Config{Bare: true, BareSet: true, AllowSet: true, Allow: []string{"a"}, Deny: []string{"b"}, ToolsSet: true, Tools: []string{"read"}},
			"",
		},
		{
			"empty lists are stated", `{"skills":[],"tools":[]}`,
			Config{AllowSet: true, Allow: []string{}, ToolsSet: true, Tools: []string{}},
			"",
		},
		{"absent lists are not", `{"bare":false}`, Config{BareSet: true}, ""},
		{"unknown key", `{"skill":["a"]}`, Config{}, "unknown field"},
		{"not json", `bare`, Config{}, FileName},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.body != "" {
				if err := os.WriteFile(filepath.Join(dir, FileName), []byte(tc.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := Load(dir)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Load = %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestAppendRoundTripsThroughTheSession(t *testing.T) {
	ctx := context.Background()
	log := spine.NewMemoryLog()
	c := Config{Bare: true, ToolsSet: true, Tools: []string{"grep", "read"}, Deny: []string{"beta"}}
	if err := Append(ctx, log, "run-1", c, Summary{Offered: 0, Tools: c.Tools}); err != nil {
		t.Fatal(err)
	}
	evs, err := log.Read(ctx, spine.Query{Stream: "run-1"})
	if err != nil || len(evs) != 1 || evs[0].Type != EventType {
		t.Fatalf("recorded = %v, %v; want one %s", evs, err, EventType)
	}
	hist, err := session.History(ctx, log, "run-1")
	if err != nil || len(hist) != 1 {
		t.Fatalf("history = %d events, %v", len(hist), err)
	}
	e := hist[0]
	if e.Kind != session.KindSelection || e.Reason != "bare" {
		t.Fatalf("projected event = %+v", e)
	}
	for _, want := range []string{"skills offered: 0", "skills denied: beta", "tools: grep, read"} {
		if !strings.Contains(e.Text, want) {
			t.Errorf("event text lacks %q:\n%s", want, e.Text)
		}
	}
}

func TestEventTypeMatchesSession(t *testing.T) {
	if string(session.KindSelection) != EventType {
		t.Errorf("session.KindSelection = %q, selection.EventType = %q", session.KindSelection, EventType)
	}
}
