package resource_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/ionalpha/flynn/resource"
)

// widgetSpec and widgetState are a typed kind's spec/status pair, the shape the
// generic codec exists to serve.
type widgetSpec struct {
	Size  string `json:"size"`
	Count int    `json:"count"`
}

type widgetState struct {
	Phase      string               `json:"phase"`
	Conditions []resource.Condition `json:"conditions,omitempty"`
	Observed   map[string]any       `json:"observed,omitempty"`
}

// TestDecodeSpecReadsTypedSpec gates the generic spec codec: a populated spec
// decodes into the typed struct, an absent spec decodes to the zero value without
// a nil check, and malformed JSON is reported rather than silently dropped.
func TestDecodeSpecReadsTypedSpec(t *testing.T) {
	cases := []struct {
		name    string
		spec    json.RawMessage
		want    widgetSpec
		wantErr bool
	}{
		{"populated", json.RawMessage(`{"size":"m","count":3}`), widgetSpec{Size: "m", Count: 3}, false},
		{"empty spec is the zero value", nil, widgetSpec{}, false},
		{"malformed json", json.RawMessage(`{"size":`), widgetSpec{}, true},
		{"wrong type for a field", json.RawMessage(`{"count":"three"}`), widgetSpec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resource.DecodeSpec[widgetSpec](resource.Resource{Spec: tc.spec})
			if (err != nil) != tc.wantErr {
				t.Fatalf("DecodeSpec err = %v, wantErr = %v", err, tc.wantErr)
			}
			if err != nil {
				return
			}
			if got != tc.want {
				t.Fatalf("DecodeSpec = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestDecodeStatusReadsTypedStatus gates the status half of the codec: a fresh
// resource has no status and must read as the zero value, a written status reads
// back, and a corrupt one errors.
func TestDecodeStatusReadsTypedStatus(t *testing.T) {
	fresh, err := resource.DecodeStatus[widgetState](resource.Resource{})
	if err != nil {
		t.Fatalf("DecodeStatus on an empty status: %v", err)
	}
	if fresh.Phase != "" || fresh.Conditions != nil {
		t.Fatalf("an unset status must decode to the zero value, got %+v", fresh)
	}

	enc, err := resource.EncodeStatus(widgetState{Phase: "ready", Observed: map[string]any{"n": float64(2)}})
	if err != nil {
		t.Fatalf("EncodeStatus: %v", err)
	}
	got, err := resource.DecodeStatus[widgetState](resource.Resource{Status: enc})
	if err != nil {
		t.Fatalf("DecodeStatus: %v", err)
	}
	if got.Phase != "ready" || got.Observed["n"] != float64(2) {
		t.Fatalf("status did not round-trip through the codec: %+v", got)
	}

	if _, err := resource.DecodeStatus[widgetState](resource.Resource{Status: json.RawMessage(`[]`)}); err == nil {
		t.Fatal("a status that is not an object must fail to decode")
	}
}

// TestEncodeStatusFailsOnUnencodableValue proves EncodeStatus reports an encoding
// failure instead of writing a truncated status: a value JSON cannot represent
// must surface as an error to the controller.
func TestEncodeStatusFailsOnUnencodableValue(t *testing.T) {
	if _, err := resource.EncodeStatus(widgetState{Observed: map[string]any{"ch": make(chan int)}}); err == nil {
		t.Fatal("EncodeStatus must reject a value JSON cannot represent")
	}
}

// TestSetConditionUpsertsAndStampsOnlyOnChange gates the condition rule a
// reconciler depends on: a repeated no-op reconcile must not churn
// LastTransitionTime, while a real status flip must stamp the new time. Times are
// fixed, so the assertion is on the rule, not on a wall clock.
func TestSetConditionUpsertsAndStampsOnlyOnChange(t *testing.T) {
	t0 := time.Unix(1_700_000_000, 0).UTC()
	t1 := t0.Add(time.Hour)
	t2 := t1.Add(time.Hour)

	conds := resource.SetCondition(nil, resource.Condition{Type: "Ready", Status: "False", Reason: "Pending"}, t0)
	if len(conds) != 1 || conds[0].LastTransitionTime != t0 {
		t.Fatalf("a new condition must be appended and stamped at now, got %+v", conds)
	}

	// Same status, new message: an upsert that must not move the transition time.
	conds = resource.SetCondition(conds, resource.Condition{Type: "Ready", Status: "False", Message: "still waiting"}, t1)
	if len(conds) != 1 {
		t.Fatalf("upsert by type must not append a duplicate, got %d conditions", len(conds))
	}
	if conds[0].LastTransitionTime != t0 {
		t.Fatalf("an unchanged status must keep its transition time %v, got %v", t0, conds[0].LastTransitionTime)
	}
	if conds[0].Message != "still waiting" {
		t.Fatalf("the upsert must replace the condition body, got %+v", conds[0])
	}

	// A real flip stamps the new time.
	conds = resource.SetCondition(conds, resource.Condition{Type: "Ready", Status: "True"}, t2)
	if conds[0].LastTransitionTime != t2 {
		t.Fatalf("a status change must stamp now (%v), got %v", t2, conds[0].LastTransitionTime)
	}

	// A different type is a separate condition, not an overwrite.
	conds = resource.SetCondition(conds, resource.Condition{Type: "Stalled", Status: "False"}, t2)
	if len(conds) != 2 {
		t.Fatalf("a new condition type must be appended, got %+v", conds)
	}
}
