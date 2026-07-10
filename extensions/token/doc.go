//go:build !token

// Package token provides an optional Solana token capability. Its engine, metadata,
// guarded mint, and agent tools are built only with the "token" build tag, so the
// default Flynn binary links no cryptocurrency dependencies. Build or test with
// -tags token to include the adapter. The chain-agnostic anti-scam policy lives in
// the safety subpackage and always builds.
package token
