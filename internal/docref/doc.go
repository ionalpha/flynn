// Package docref holds the gate that keeps comments honest about names. Its test
// walks the module and fails on a comment that refers to pkg.Name, or links
// [pkg.Name], where pkg resolves to a package in this module that declares no such
// Name. A rename the comment did not follow is the commonest way a comment goes
// stale, and the only one a parser can see.
//
// The check is deliberately narrow. A reference is checked only when its package
// resolves unambiguously: an import of the file, the file's own package, or the one
// package in the module with that name when no standard-library package shares it.
// Anything else is left alone, so the gate stays quiet enough to block on.
package docref
