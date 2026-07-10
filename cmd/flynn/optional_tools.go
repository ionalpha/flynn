package main

import "github.com/ionalpha/flynn/mission"

// An optionalToolProvider contributes the tools of one optional, build-tag-gated
// capability. Each capability appends one to optionalToolProviders from an init
// function compiled only under its own build tag, so the default binary registers
// none and links none of that capability's dependencies. Adding an optional
// capability is a new self-registering file, never an edit to the run assembly. A
// provider returns an error when it is configured but cannot be built (for example a
// signer that is set but unreadable), so a misconfiguration fails startup loudly
// rather than silently dropping the capability.
type optionalToolProvider func() ([]mission.Tool, error)

// optionalToolProviders is appended to by each optional capability's init, under
// that capability's build tag.
var optionalToolProviders []optionalToolProvider

// optionalTools returns every registered capability's tools for this build, in
// registration order, or the first provider error. It is empty in the default build.
func optionalTools() ([]mission.Tool, error) {
	var out []mission.Tool
	for _, p := range optionalToolProviders {
		tools, err := p()
		if err != nil {
			return nil, err
		}
		out = append(out, tools...)
	}
	return out, nil
}
