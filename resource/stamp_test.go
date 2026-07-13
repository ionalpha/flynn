package resource

import (
	"errors"
	"testing"
	"time"

	"github.com/ionalpha/flynn/clock"
	"github.com/ionalpha/flynn/envelope"
	"github.com/ionalpha/flynn/hlc"
	"github.com/ionalpha/flynn/ids"
)

// newTestStamper builds a Stamper on a manual clock and a registry with one
// unconstrained kind, so a write is deterministic and needs no wall clock.
func newTestStamper(t *testing.T) (*Stamper, *Registry) {
	t.Helper()
	reg := NewRegistry()
	if err := reg.Register(Kind{APIVersion: "test/v1", Name: "Thing"}); err != nil {
		t.Fatal(err)
	}
	clk := clock.NewManual(time.Unix(1_700_000_000, 0))
	return NewStamper("node-1", clk, hlc.NewClock(hlc.WithPhysical(clk)), ids.NewGenerator(ids.WithClock(clk)), reg), reg
}

// TestStamperRegistryIsTheAdmissionRegistry proves the stamper exposes the exact
// registry it admits against, so a backend holding only a Stamper can answer "is
// this kind registered?" without being handed the registry a second time.
func TestStamperRegistryIsTheAdmissionRegistry(t *testing.T) {
	st, reg := newTestStamper(t)
	if st.Registry() != reg {
		t.Fatal("Registry() must return the registry the stamper admits against")
	}
	// The identity is meaningful: a kind registered afterwards is visible through it.
	if err := st.Registry().Register(Kind{APIVersion: "test/v1", Name: "Late"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.Put(nil, Resource{APIVersion: "test/v1", Kind: "Late", Name: "a"}); err != nil {
		t.Fatalf("a kind registered through Registry() must be admitted: %v", err)
	}
}

// TestStamperRejectsAnUnaddressableResource gates the one shape no backend can
// store: a record without a kind or without any way to derive a name has no
// address, so it must be refused before an id is minted.
func TestStamperRejectsAnUnaddressableResource(t *testing.T) {
	st, _ := newTestStamper(t)
	cases := []struct {
		name string
		r    Resource
	}{
		{"no apiVersion", Resource{Kind: "Thing", Name: "a"}},
		{"no kind", Resource{APIVersion: "test/v1", Name: "a"}},
		{"no name and no generateName", Resource{APIVersion: "test/v1", Kind: "Thing"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := st.Put(nil, tc.r); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Put(%+v) err = %v, want ErrInvalid", tc.r, err)
			}
		})
	}
}

// TestValidateForMerge gates the merge envelope: a replicated record must carry
// every field Resolve orders by. A half-built local resource fed into the merge
// path would be applied with no identity and no clock, so each missing field is a
// refusal, not a default.
func TestValidateForMerge(t *testing.T) {
	full := func() Resource {
		return Resource{
			APIVersion: "test/v1", Kind: "Thing", ID: "rid-1", Name: "alpha",
			Envelope: Envelope{
				Envelope: envelope.Envelope{
					OriginInstanceID: "remote",
					LastWriterID:     "remote",
					UpdatedHLC:       hlc.Time{Wall: 100},
				},
			},
		}
	}
	cases := []struct {
		name    string
		mutate  func(*Resource)
		wantErr bool
	}{
		{"complete envelope", func(*Resource) {}, false},
		{"no id", func(r *Resource) { r.ID = "" }, true},
		{"no apiVersion", func(r *Resource) { r.APIVersion = "" }, true},
		{"no kind", func(r *Resource) { r.Kind = "" }, true},
		{"no name", func(r *Resource) { r.Name = "" }, true},
		{"no hlc", func(r *Resource) { r.UpdatedHLC = hlc.Time{} }, true},
		{"no origin instance", func(r *Resource) { r.OriginInstanceID = "" }, true},
		{"no last writer", func(r *Resource) { r.LastWriterID = "" }, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := full()
			tc.mutate(&r)
			err := ValidateForMerge(r)
			if (err != nil) != tc.wantErr {
				t.Fatalf("ValidateForMerge err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil && !errors.Is(err, ErrInvalid) {
				t.Fatalf("err = %v, want it to wrap ErrInvalid", err)
			}
		})
	}
}
