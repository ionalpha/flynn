package inference

import "sync"

// A runtime's advisory floor is compiled in, so the signed notice feed carries any
// newer floor and it is recorded here. A floor learned at runtime can only raise the
// gate: nothing lowers a floor, marks a refused runtime safe, or clears the overlay. An
// attacker holding both the feed origin and the signing key can therefore only make
// Flynn refuse a runtime, a locally recoverable denial of service, and never run a
// vulnerable one.
var (
	overlayMu sync.RWMutex
	overlay   = map[string]overlayFloor{}
)

// overlayFloor is a floor learned at runtime, with the advisory it came from so a
// refusal can name a reason this build never knew.
type overlayFloor struct {
	version  Version
	advisory string
}

// Raise records a floor for a runtime learned from a signed source after this binary
// was built, keeping whichever floor is higher. A lower floor is a silent no-op, so a
// hostile feed cannot make the notice path noisy or abort it before the rest of the
// feed is read. An unknown runtime name is ignored, so a feed cannot invent a runtime.
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

// FloorFor returns the floor a runtime is held to now: the higher of the compiled-in
// floor and any raised since. The second result reports whether a floor is known, and
// the third names the advisory a raised floor came from (empty for the built-in one).
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

func builtinFloor(runtime string) (Version, bool) {
	r, ok := runtimeNamed(runtime)
	if !ok || len(r.MinSupported) == 0 {
		return nil, false
	}
	return r.MinSupported, true
}

func runtimeNamed(name string) (Runtime, bool) {
	for _, r := range Runtimes() {
		if r.Name == name {
			return r, true
		}
	}
	return Runtime{}, false
}

// resetOverlay clears the raised floors between tests. It is unexported so no
// production path can lower a gate.
func resetOverlay() {
	overlayMu.Lock()
	defer overlayMu.Unlock()
	overlay = map[string]overlayFloor{}
}
