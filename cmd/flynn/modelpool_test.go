package main

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/catalog"
	"github.com/ionalpha/flynn/inference/orchestrate"
)

func TestClassifyLaunchFailure(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want orchestrate.FailureKind
	}{
		{"nil", nil, orchestrate.FailureNone},
		{"cuda oom", errors.New("serve: the runtime exited:\nggml_cuda error: out of memory"), orchestrate.FailureOOM},
		{"alloc", errors.New("CUDA error: failed to allocate device buffer"), orchestrate.FailureOOM},
		{"timeout", errors.New("serve: the runtime did not answer within 90s"), orchestrate.FailureHang},
		{"crash", errors.New("serve: the runtime exited:\nfailed to load model: bad magic"), orchestrate.FailureCrash},
	}
	for _, tc := range cases {
		if got := classifyLaunchFailure(tc.err); got != tc.want {
			t.Errorf("%s: classifyLaunchFailure = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// firstLocalModel returns a local catalog model to exercise the pool builder against, skipping
// the test if the embedded catalog has none.
func firstLocalModel(t *testing.T) (catalog.Catalog, catalog.ModelSpec) {
	t.Helper()
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	for _, m := range cat.Models {
		if m.Local() {
			return cat, m
		}
	}
	t.Skip("no local model in the catalog to test against")
	return catalog.Catalog{}, catalog.ModelSpec{}
}

func TestBuildPoolResolvesLocalModel(t *testing.T) {
	cat, m := firstLocalModel(t)
	pp, err := buildPool(cat, []string{m.ID}, nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	if len(pp.desired) != 1 || pp.desired[0].ModelID != m.ID {
		t.Fatalf("desired = %+v, want one entry for %q", pp.desired, m.ID)
	}
	if pp.desired[0].Pinned {
		t.Fatal("model must not be pinned without --pin")
	}
	var want int64
	if q, ok := m.SmallestQuant(); ok {
		want = q.SizeBytes
	}
	if pp.footprint(m.ID) != want {
		t.Fatalf("footprint(%q) = %d, want the smallest-quant size %d", m.ID, pp.footprint(m.ID), want)
	}
	if _, ok := pp.specs[m.ID]; !ok {
		t.Fatalf("spec for %q not recorded", m.ID)
	}
}

func TestBuildPoolPins(t *testing.T) {
	cat, m := firstLocalModel(t)
	pp, err := buildPool(cat, []string{m.ID}, map[string]bool{m.ID: true})
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	if !pp.desired[0].Pinned {
		t.Fatal("a model named in --pin must be pinned")
	}
}

func TestBuildPoolDeduplicates(t *testing.T) {
	cat, m := firstLocalModel(t)
	pp, err := buildPool(cat, []string{m.ID, m.ID}, nil)
	if err != nil {
		t.Fatalf("buildPool: %v", err)
	}
	if len(pp.desired) != 1 {
		t.Fatalf("a repeated id must collapse to one entry, got %d", len(pp.desired))
	}
}

func TestBuildPoolRejectsUnknownModel(t *testing.T) {
	cat, _ := firstLocalModel(t)
	if _, err := buildPool(cat, []string{"definitely-not-a-real-model-id"}, nil); err == nil {
		t.Fatal("an unknown id must be rejected")
	}
}

func TestBuildPoolRejectsAPIModel(t *testing.T) {
	cat, err := catalog.Load()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	var api catalog.ModelSpec
	found := false
	for _, m := range cat.Models {
		if !m.Local() {
			api, found = m, true
			break
		}
	}
	if !found {
		t.Skip("no hosted API model in the catalog to test against")
	}
	if _, err := buildPool(cat, []string{api.ID}, nil); err == nil {
		t.Fatalf("a hosted API model (%q) must be rejected for a local pool", api.ID)
	}
}

func TestBuildPoolRequiresAtLeastOne(t *testing.T) {
	cat, _ := firstLocalModel(t)
	pp, err := buildPool(cat, nil, nil)
	if err != nil {
		t.Fatalf("buildPool with no ids should be empty, not error: %v", err)
	}
	if len(pp.desired) != 0 {
		t.Fatalf("no ids should yield no desired entries, got %+v", pp.desired)
	}
}

func TestCommaSet(t *testing.T) {
	got := commaSet(" a , b ,, c ")
	want := map[string]bool{"a": true, "b": true, "c": true}
	if len(got) != len(want) {
		t.Fatalf("commaSet = %v, want %v", got, want)
	}
	for k := range want {
		if !got[k] {
			t.Fatalf("commaSet missing %q (got %v)", k, got)
		}
	}
	if len(commaSet("")) != 0 {
		t.Fatal("an empty value must yield an empty set")
	}
}

func TestStaticPoolProviderReturnsFixedState(t *testing.T) {
	ds := orchestrate.DesiredState{
		Models: []orchestrate.Desired{{ModelID: "a", Footprint: 10}},
		Budget: 123,
	}
	p := staticPoolProvider{ds: ds}
	got, err := p.Desired(context.Background())
	if err != nil {
		t.Fatalf("Desired: %v", err)
	}
	if got.Budget != ds.Budget || len(got.Models) != len(ds.Models) {
		t.Fatalf("Desired = %+v, want %+v", got, ds)
	}
}
