package inference

import "sync"

// The advisory floor a runtime is held to is a compiled-in value (Runtime.MinSupported),
// which means a parser flaw disclosed after a release cannot reach an installed Flynn
// until the user upgrades it. The signed notice feed can carry a newer floor, and this is
// where one lands.
//
// The rule the whole design rests on: a floor learned at runtime may only ever RAISE the
// gate. Raise takes the maximum of what is compiled in and what arrives, and there is no
// function anywhere that lowers a floor, marks a refused runtime safe, or clears the
// overlay. That is not a convention, it is the entire API.
//
// The consequence is worth stating plainly, because it is the reason a network-delivered
// security policy is safe to have at all: an attacker who compromises the feed origin AND
// steals the signing key can only make Flynn refuse to run a runtime. They can never make
// it run a vulnerable one. The worst case is a denial of service, which is loud and
// visible and locally recoverable, rather than a remote path to code execution. A channel
// that could relax a gate would have handed them the opposite.
var (
	overlayMu sync.RWMutex
	overlay   = map[string]overlayFloor{}
)

// overlayFloor is a floor learned at runtime, and the advisory it came from so a refusal
// can still name a reason it was not built knowing.
type overlayFloor struct {
	version  Version
	advisory string
}

// Raise records a floor for a runtime, learned from a signed source after this binary was
// built. It keeps whichever floor is higher, so calling it with an older floor than the
// one already in force (whether compiled in or previously raised) does nothing at all.
//
// It is deliberately a no-op rather than an error in that case. A hostile feed must not be
// able to make the notice path fail by sending a lower floor, because a caller that
// reports the failure would be a caller an attacker can make noisy, and one that aborts
// would be a caller an attacker can use to skip the rest of the feed.
//
// An unknown runtime name is ignored: a feed cannot invent a runtime and it cannot gate
// one Flynn does not drive.
func Raise(runtime string, v Version, advisoryID string) {
	if len(v) == 0 {
		return
	}
	if _, known := runtimeNamed(runtime); !known {
		return
	}

	overlayMu.Lock()
	defer overlayMu.Unlock()

	// Never below what was compiled in.
	if built, ok := builtinFloor(runtime); ok && v.Less(built) {
		return
	}
	// Never below what was already raised to.
	if cur, ok := overlay[runtime]; ok && v.Less(cur.version) {
		return
	}
	overlay[runtime] = overlayFloor{version: v, advisory: advisoryID}
}

// FloorFor returns the floor a runtime is held to right now: the higher of the compiled-in
// floor and any floor raised since. The second result reports whether a floor is known at
// all, and the third names the advisory a raised floor came from (empty when the floor is
// the one this binary was built with).
func FloorFor(runtime string) (Version, bool, string) {
	built, hasBuilt := builtinFloor(runtime)

	overlayMu.RLock()
	raised, hasRaised := overlay[runtime]
	overlayMu.RUnlock()

	switch {
	case hasRaised && (!hasBuilt || built.Less(raised.version)):
		return raised.version, true, raised.advisory
	case hasBuilt:
		return built, true, ""
	default:
		return nil, false, ""
	}
}

// builtinFloor returns the floor compiled into this binary for a runtime.
func builtinFloor(runtime string) (Version, bool) {
	r, ok := runtimeNamed(runtime)
	if !ok || len(r.MinSupported) == 0 {
		return nil, false
	}
	return r.MinSupported, true
}

// runtimeNamed returns the known runtime with this name.
func runtimeNamed(name string) (Runtime, bool) {
	for _, r := range Runtimes() {
		if r.Name == name {
			return r, true
		}
	}
	return Runtime{}, false
}

// resetOverlay clears the raised floors. It exists for tests, which must not leak a raised
// floor into each other, and is unexported precisely so that no production path can reach
// it: there is no supported way to lower a gate.
func resetOverlay() {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlay = map[string]overlayFloor{}
}
