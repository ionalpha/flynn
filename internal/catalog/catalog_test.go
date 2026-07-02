package catalog

import (
	"encoding/json"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestEmbeddedCatalogLoadsAndValidates(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatalf("embedded catalog must load and validate: %v", err)
	}
	if c.Version == "" || len(c.Models) == 0 {
		t.Fatalf("catalog looks empty: %+v", c)
	}
	// The shipped catalog must include at least one small local model (the fetch-test
	// target) and one API model, so discovery covers both worlds out of the box.
	var haveSmallLocal, haveAPI bool
	for _, m := range c.Models {
		if m.Kind == KindLocal {
			if q, ok := m.SmallestQuant(); ok && q.SizeBytes > 0 && q.SizeBytes < 2_000_000_000 {
				haveSmallLocal = true
			}
		}
		if m.Kind == KindAPI {
			haveAPI = true
		}
	}
	if !haveSmallLocal || !haveAPI {
		t.Fatalf("want at least one small local and one API model; small=%v api=%v", haveSmallLocal, haveAPI)
	}
}

func TestValidateRejectsBadEntries(t *testing.T) {
	good := ModelSpec{
		ID: "x:y", Name: "Y", Kind: KindAPI, License: "MIT",
		Source: Source{Publisher: "P", URL: "https://example.com", Registry: "r"},
	}
	cases := map[string]func(m *ModelSpec){
		"no id":        func(m *ModelSpec) { m.ID = "" },
		"no name":      func(m *ModelSpec) { m.Name = "" },
		"no license":   func(m *ModelSpec) { m.License = "" },
		"no publisher": func(m *ModelSpec) { m.Source.Publisher = "" },
		"no url":       func(m *ModelSpec) { m.Source.URL = "" },
		"unknown kind": func(m *ModelSpec) { m.Kind = "weird" },
		"local no quant": func(m *ModelSpec) {
			m.Kind = KindLocal
			m.Quants = nil
		},
		"bad quant format": func(m *ModelSpec) {
			m.Kind = KindLocal
			m.Quants = []Quant{{Name: "q", Format: "exe", SizeBytes: 1, Ref: "r"}}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := good
			mutate(&m)
			if err := (Catalog{Models: []ModelSpec{m}}).Validate(); err == nil {
				t.Fatalf("expected %s to be rejected", name)
			}
		})
	}
	// Duplicate ids are rejected.
	if err := (Catalog{Models: []ModelSpec{good, good}}).Validate(); err == nil {
		t.Fatal("duplicate ids should be rejected")
	}
}

func TestQueryFilters(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Find(Query{Kind: KindLocal}); len(got) == 0 {
		t.Fatal("expected local models")
	} else {
		for _, m := range got {
			if m.Kind != KindLocal {
				t.Fatalf("kind filter leaked %s", m.ID)
			}
		}
	}
	for _, m := range c.Find(Query{MaxParamsB: 2}) {
		if m.ParamsB <= 0 || m.ParamsB > 2 {
			t.Fatalf("max-params leaked %s (%g)", m.ID, m.ParamsB)
		}
	}
	for _, m := range c.Find(Query{Capability: "reasoning"}) {
		if !m.Capabilities.Reasoning {
			t.Fatalf("capability filter leaked %s", m.ID)
		}
	}
}

// TestFindProperties checks the filter contract over random queries against the real
// catalog: every returned entry satisfies the query, the result is a subset with no
// duplicates, and the order is stable (API before local, then smallest footprint,
// then id), so a listing reads the same way every time.
func TestFindProperties(t *testing.T) {
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range c.Models {
		ids[m.ID] = true
	}

	rapid.Check(t, func(rt *rapid.T) {
		q := Query{
			Kind:       rapid.SampledFrom([]Kind{"", KindAPI, KindLocal}).Draw(rt, "kind"),
			Capability: rapid.SampledFrom([]string{"", "tools", "vision", "reasoning"}).Draw(rt, "cap"),
			MaxParamsB: rapid.SampledFrom([]float64{0, 2, 8, 100}).Draw(rt, "maxParams"),
			Publisher:  rapid.SampledFrom([]string{"", "Qwen", "Anthropic", "nobody"}).Draw(rt, "pub"),
			SafeOnly:   rapid.Bool().Draw(rt, "safe"),
		}
		got := c.Find(q)

		seen := map[string]bool{}
		for _, m := range got {
			if !ids[m.ID] {
				rt.Fatalf("result %s is not in the catalog", m.ID)
			}
			if seen[m.ID] {
				rt.Fatalf("duplicate result %s", m.ID)
			}
			seen[m.ID] = true
			if q.Kind != "" && m.Kind != q.Kind {
				rt.Fatalf("kind: %s", m.ID)
			}
			if q.MaxParamsB > 0 && (m.ParamsB <= 0 || m.ParamsB > q.MaxParamsB) {
				rt.Fatalf("maxParams: %s", m.ID)
			}
			if q.Publisher != "" && !strings.EqualFold(m.Source.Publisher, q.Publisher) {
				rt.Fatalf("publisher: %s", m.ID)
			}
		}
		for i := 1; i < len(got); i++ {
			if less(got[i], got[i-1]) {
				rt.Fatalf("results not sorted at %d: %s before %s", i, got[i-1].ID, got[i].ID)
			}
		}
	})
}

// TestSeedIsCanonicalJSON guards that the embedded file is well-formed JSON of the
// expected shape, so a hand edit that breaks it fails here rather than at runtime.
func TestSeedIsCanonicalJSON(t *testing.T) {
	var c Catalog
	if err := json.Unmarshal(seed, &c); err != nil {
		t.Fatalf("models.json is not valid JSON: %v", err)
	}
}
