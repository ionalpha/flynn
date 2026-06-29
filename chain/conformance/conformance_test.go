package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
)

var update = flag.Bool("update", false, "regenerate the golden conformance vector files")

const vectorsDir = "testdata/vectors"

// manifest is the on-disk index of the vector set.
type manifest struct {
	SuiteVersion string          `json:"suite_version"`
	Tier         string          `json:"tier"`
	Vectors      []manifestEntry `json:"vectors"`
}

type manifestEntry struct {
	ID          string   `json:"id"`
	Expect      string   `json:"expect"`
	FailureCode string   `json:"failure_code,omitempty"`
	Flags       []string `json:"flags"`
	Description string   `json:"description"`
	Events      []string `json:"events"`
}

// eventPaths returns the deterministic relative file path for each of a vector's
// events. A single-event vector uses one file; a multi-event vector suffixes the
// index.
func eventPaths(v Vector) []string {
	dir := "valid"
	if v.Expect == Reject {
		dir = "invalid"
	}
	base := strings.ReplaceAll(v.ID, ".", "_")
	paths := make([]string, len(v.Events))
	for i := range v.Events {
		name := base + ".cbor"
		if len(v.Events) > 1 {
			name = fmt.Sprintf("%s_%d.cbor", base, i)
		}
		paths[i] = filepath.ToSlash(filepath.Join(dir, name))
	}
	return paths
}

func faultCode(err error) string {
	var fe *fault.Error
	if errors.As(err, &fe) {
		return fe.Code
	}
	return ""
}

// buildManifest derives the manifest and the path-to-bytes file set from the
// generated vectors.
func buildManifest() (manifest, map[string][]byte) {
	m := manifest{SuiteVersion: SuiteVersion, Tier: Tier}
	files := map[string][]byte{}
	for _, v := range Generate() {
		paths := eventPaths(v)
		for i, p := range paths {
			files[p] = v.Events[i]
		}
		m.Vectors = append(m.Vectors, manifestEntry{
			ID:          v.ID,
			Expect:      string(v.Expect),
			FailureCode: v.FailureCode,
			Flags:       v.Flags,
			Description: v.Description,
			Events:      paths,
		})
	}
	return m, files
}

func manifestBytes(m manifest) []byte {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		panic("conformance: marshal manifest: " + err.Error())
	}
	return append(b, '\n')
}

// TestVectorsConform runs every generated vector through the real chain verifier
// and asserts it reaches the declared verdict, and for a rejection the declared
// failure code. This is what makes the suite the canon: the reference verifier and
// the published vectors agree by construction.
func TestVectorsConform(t *testing.T) {
	v := chain.NewVerifier()
	for _, vec := range Generate() {
		t.Run(vec.ID, func(t *testing.T) {
			_, err := v.VerifyStream(vec.Events)
			switch vec.Expect {
			case Accept:
				if err != nil {
					t.Fatalf("expected accept, got error: %v", err)
				}
			case Reject:
				if err == nil {
					t.Fatal("expected reject, got accept")
				}
				if code := faultCode(err); code != vec.FailureCode {
					t.Fatalf("wrong failure code: got %q want %q (err: %v)", code, vec.FailureCode, err)
				}
			default:
				t.Fatalf("unknown verdict %q", vec.Expect)
			}
		})
	}
}

// TestGoldenVectorsMatch checks that the committed fixture files and manifest match
// what the generator produces, so a drift is caught in review. Run with -update to
// regenerate them after an intentional change.
func TestGoldenVectorsMatch(t *testing.T) {
	m, files := buildManifest()
	manBytes := manifestBytes(m)

	if *update {
		if err := os.RemoveAll(vectorsDir); err != nil {
			t.Fatal(err)
		}
		for p, b := range files {
			full := filepath.Join(vectorsDir, filepath.FromSlash(p))
			if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, b, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.MkdirAll(vectorsDir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(vectorsDir, "manifest.json"), manBytes, 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %d vector files and manifest.json", len(files))
		return
	}

	for p, want := range files {
		got, err := os.ReadFile(filepath.Join(vectorsDir, filepath.FromSlash(p)))
		if err != nil {
			t.Fatalf("missing vector file %s (run: go test ./chain/conformance -update): %v", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("vector file %s differs from the generator (run -update if intentional)", p)
		}
	}
	gotMan, err := os.ReadFile(filepath.Join(vectorsDir, "manifest.json"))
	if err != nil {
		t.Fatalf("missing manifest.json (run -update): %v", err)
	}
	if !bytes.Equal(gotMan, manBytes) {
		t.Fatal("manifest.json differs from the generator (run -update if intentional)")
	}
}
