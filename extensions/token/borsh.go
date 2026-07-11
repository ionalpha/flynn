package token

import (
	"bytes"
	"encoding/binary"
)

// Metaplex Token Metadata instruction discriminators (single-byte enum index).
const (
	ixCreateMetadataV3 = 33 // create uses V3; the legacy V2 create is removed on-chain
	ixUpdate           = 50 // modern Update; the legacy V2 update mismatches V3 metadata on length
)

// borshString writes a Borsh string: u32 LE length prefix then the raw bytes.
func borshString(buf *bytes.Buffer, s string) {
	// #nosec G115 -- these are token metadata fields (name, symbol, uri), whose
	// lengths are bounded far below uint32.
	_ = binary.Write(buf, binary.LittleEndian, uint32(len(s)))
	buf.WriteString(s)
}

// dataV2 writes a Metaplex DataV2: name, symbol, uri, sellerFee=0, and None for
// creators/collection/uses.
func dataV2(buf *bytes.Buffer, name, symbol, uri string) {
	borshString(buf, name)
	borshString(buf, symbol)
	borshString(buf, uri)
	_ = binary.Write(buf, binary.LittleEndian, uint16(0)) // seller_fee_basis_points
	buf.WriteByte(0)                                      // creators: None
	buf.WriteByte(0)                                      // collection: None
	buf.WriteByte(0)                                      // uses: None
}

// createV3Data builds the instruction data for CreateMetadataAccountV3 (mutable).
func createV3Data(name, symbol, uri string) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(ixCreateMetadataV3)
	dataV2(buf, name, symbol, uri)
	buf.WriteByte(0) // is_mutable = false: the identity (name/symbol/logo) is permanently locked
	buf.WriteByte(0) // collection_details: None
	return buf.Bytes()
}

// updateData builds the instruction data for the modern Update (UpdateArgs::V1).
func updateData(name, symbol, uri string) []byte {
	buf := &bytes.Buffer{}
	buf.WriteByte(ixUpdate)
	buf.WriteByte(0) // UpdateArgs variant = V1
	buf.WriteByte(0) // new_update_authority: None
	buf.WriteByte(1) // data: Some
	borshString(buf, name)
	borshString(buf, symbol)
	borshString(buf, uri)
	_ = binary.Write(buf, binary.LittleEndian, uint16(0)) // seller_fee_basis_points
	buf.WriteByte(0)                                      // creators: None
	buf.WriteByte(0)                                      // primary_sale_happened: None
	buf.WriteByte(0)                                      // is_mutable: None (unchanged)
	buf.WriteByte(0)                                      // collection: CollectionToggle::None
	buf.WriteByte(0)                                      // collection_details: None
	buf.WriteByte(0)                                      // uses: UsesToggle::None
	buf.WriteByte(0)                                      // rule_set: RuleSetToggle::None
	buf.WriteByte(0)                                      // authorization_data: None
	return buf.Bytes()
}
