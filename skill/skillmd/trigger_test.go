package skillmd

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeTriggers(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    []Trigger
		invalid bool
	}{
		{"every kind", `[{"kind":"file","match":"**/go.mod"},{"kind":"tool","match":"shell"},{"kind":"repeat","count":3},{"kind":"slow","seconds":60,"match":"shell"}]`,
			[]Trigger{{Kind: "file", Match: "**/go.mod"}, {Kind: "tool", Match: "shell"}, {Kind: "repeat", Count: 3}, {Kind: "slow", Seconds: 60, Match: "shell"}}, false},
		{"unknown kind skipped, known kept", `[{"kind":"mood","match":"x"},{"kind":"tool","match":"shell"}]`, []Trigger{{Kind: "tool", Match: "shell"}}, false},
		{"unknown field ignored", `[{"kind":"tool","match":"shell","future":{"a":1}}]`, []Trigger{{Kind: "tool", Match: "shell"}}, false},
		{"empty list", `[]`, nil, false},
		{"not an array", `{"kind":"tool"}`, nil, true},
		{"null", `null`, nil, true},
		{"element not an object", `["tool"]`, nil, true},
		{"file without glob", `[{"kind":"file"}]`, nil, true},
		{"bad glob", `[{"kind":"file","match":"[a"}]`, nil, true},
		{"tool without name", `[{"kind":"tool"}]`, nil, true},
		{"repeat too low", `[{"kind":"repeat","count":1}]`, nil, true},
		{"repeat too high", `[{"kind":"repeat","count":21}]`, nil, true},
		{"slow zero", `[{"kind":"slow"}]`, nil, true},
		{"slow too long", `[{"kind":"slow","seconds":3601}]`, nil, true},
		{"too many", "[" + strings.Repeat(`{"kind":"tool","match":"a"},`, 8) + `{"kind":"tool","match":"a"}]`, nil, true},
		{"match too long", `[{"kind":"tool","match":"` + strings.Repeat("a", 257) + `"}]`, nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeTriggers(tc.in)
			if tc.invalid {
				if !errors.Is(err, ErrInvalid) {
					t.Fatalf("err = %v, want ErrInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestEncodeTriggersRoundTrip(t *testing.T) {
	in := []Trigger{{Kind: TriggerFile, Match: "*.go"}, {Kind: TriggerRepeat, Count: 3}}
	got, err := DecodeTriggers(EncodeTriggers(in))
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip = %+v, %v; want %+v", got, err, in)
	}
	if EncodeTriggers(nil) != "" {
		t.Fatal("no triggers must encode as the empty string so a writer omits the key")
	}
}

func TestValidateRefusesMalformedTriggers(t *testing.T) {
	d := Doc{Name: "x", Description: "d", Metadata: map[string]string{MetaTriggers: `[{"kind":"slow"}]`}}
	if err := Validate(d, "x"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Validate = %v, want ErrInvalid", err)
	}
	d.Metadata[MetaTriggers] = `[{"kind":"slow","seconds":5}]`
	if err := Validate(d, "x"); err != nil {
		t.Fatal(err)
	}
}
