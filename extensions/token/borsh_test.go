package token

import (
	"encoding/binary"
	"testing"

	"pgregory.net/rapid"
)

// readBorshString reads a Borsh string (u32 LE length then bytes) at off, returning
// the decoded string and the offset just past it.
func readBorshString(b []byte, off int) (string, int) {
	n := int(binary.LittleEndian.Uint32(b[off:]))
	off += 4
	return string(b[off : off+n]), off + n
}

// TestCreateV3DataRoundTrip checks, for arbitrary name/symbol/uri, that
// createV3Data leads with the CreateMetadataV3 discriminator and then encodes the
// three strings exactly (correct length prefixes, exact bytes, right order). A
// length-prefix bug would surface here across the whole input space.
func TestCreateV3DataRoundTrip(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.String().Draw(t, "name")
		symbol := rapid.String().Draw(t, "symbol")
		uri := rapid.String().Draw(t, "uri")

		b := createV3Data(name, symbol, uri)
		if len(b) == 0 || b[0] != ixCreateMetadataV3 {
			t.Fatalf("discriminator: got %v, want %d", b, ixCreateMetadataV3)
		}
		gotName, off := readBorshString(b, 1)
		gotSymbol, off := readBorshString(b, off)
		gotURI, _ := readBorshString(b, off)
		if gotName != name || gotSymbol != symbol || gotURI != uri {
			t.Fatalf("round-trip mismatch: name=%q symbol=%q uri=%q", gotName, gotSymbol, gotURI)
		}
	})
}

// TestUpdateDataDiscriminator checks updateData always leads with the modern Update
// discriminator and the V1 variant/data-present markers, for arbitrary inputs.
func TestUpdateDataDiscriminator(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		name := rapid.String().Draw(t, "name")
		symbol := rapid.String().Draw(t, "symbol")
		uri := rapid.String().Draw(t, "uri")

		b := updateData(name, symbol, uri)
		// [ixUpdate][variant=0][new_update_authority=None(0)][data=Some(1)]...
		if len(b) < 4 || b[0] != ixUpdate || b[1] != 0 || b[2] != 0 || b[3] != 1 {
			t.Fatalf("update header wrong: %v", b[:min(4, len(b))])
		}
		gotName, off := readBorshString(b, 4)
		gotSymbol, _ := readBorshString(b, off)
		if gotName != name || gotSymbol != symbol {
			t.Fatalf("update round-trip mismatch: name=%q symbol=%q", gotName, gotSymbol)
		}
	})
}
