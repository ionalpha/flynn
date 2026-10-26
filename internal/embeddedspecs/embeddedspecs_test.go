package embeddedspecs_test

import (
	"errors"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ionalpha/flynn/internal/embeddedspecs"
)

type spec struct {
	Bin string `json:"bin"`
}

var naming = embeddedspecs.Naming{Prefix: "widget", Noun: "gadget"}

func TestParseOrdersByNameAndKeepsRawBytes(t *testing.T) {
	fsys := fstest.MapFS{
		"specs/zeta.json":  {Data: []byte(`{"bin":"z"}`)},
		"specs/alpha.json": {Data: []byte(`{"bin":"a"}`)},
		"specs/notes.txt":  {Data: []byte(`not a spec`)},
		"specs/sub/x.json": {Data: []byte(`{"bin":"nested"}`)},
	}
	entries, reserved, err := embeddedspecs.Parse[spec](fsys, naming, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Name != "alpha" || entries[1].Name != "zeta" {
		t.Fatalf("entries = %+v, want alpha then zeta only", entries)
	}
	if entries[0].Spec.Bin != "a" || string(entries[0].Raw) != `{"bin":"a"}` {
		t.Errorf("alpha = %+v, want its decoded spec and exact bytes", entries[0])
	}
	if !reserved["alpha"] || !reserved["zeta"] || reserved["notes"] || reserved["x"] {
		t.Errorf("reserved = %v, want exactly alpha and zeta", reserved)
	}
}

func TestParseFailuresNameTheFileAndReturnNothing(t *testing.T) {
	checkErr := errors.New("rejected by check")
	tests := []struct {
		name  string
		fsys  fstest.MapFS
		check func(string, spec) error
		want  string
	}{
		{"no directory", fstest.MapFS{}, nil, "widget: read embedded specs"},
		{"bad json", fstest.MapFS{"specs/a.json": {Data: []byte(`{`)}}, nil, "widget: decode a.json"},
		{"check rejects", fstest.MapFS{"specs/a.json": {Data: []byte(`{}`)}}, func(string, spec) error { return checkErr }, "rejected by check"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			entries, reserved, err := embeddedspecs.Parse(tc.fsys, naming, tc.check)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tc.want)
			}
			if entries != nil || reserved != nil {
				t.Errorf("got entries %v and reserved %v alongside an error, want neither", entries, reserved)
			}
		})
	}
}

func TestParseCheckSeesNameAndSpec(t *testing.T) {
	fsys := fstest.MapFS{"specs/a.json": {Data: []byte(`{"bin":"b"}`)}}
	var gotName, gotBin string
	_, _, err := embeddedspecs.Parse(fsys, naming, func(name string, s spec) error {
		gotName, gotBin = name, s.Bin
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotName != "a" || gotBin != "b" {
		t.Errorf("check saw %q, %q; want a, b", gotName, gotBin)
	}
}

// repeatingFS lists one file twice, the only way to reach the duplicate-name check: a real
// directory cannot hold two entries with one name.
type repeatingFS struct{ fstest.MapFS }

func (r repeatingFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := r.MapFS.ReadDir(name)
	if err != nil {
		return nil, err
	}
	return append(entries, entries...), nil
}

func TestParseRejectsTwoFilesClaimingOneName(t *testing.T) {
	fsys := repeatingFS{fstest.MapFS{"specs/a.json": {Data: []byte(`{}`)}}}
	_, _, err := embeddedspecs.Parse[spec](fsys, naming, nil)
	if err == nil || !strings.Contains(err.Error(), `widget: duplicate official gadget name "a"`) {
		t.Fatalf("err = %v, want the duplicate-name error naming a", err)
	}
}
