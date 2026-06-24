package spine

// Event payloads evolve. Because the log is append-only and replayed by code
// newer than the code that wrote it, an old event must still be readable after
// its payload shape changes. The version on each event (Event.SchemaVersion)
// records which shape it was written in; an Upcaster migrates an older shape up
// to the one current code expects.
//
// This is the home for that evolution. It is empty until the first time a
// payload's shape changes incompatibly (a renamed, removed, or retyped key), at
// which point that event Type gets an Upcaster and starts writing a higher
// version. New keys do not need an upcaster: reading code can treat a missing key
// as its zero value.

// Upcaster migrates one event's payload from the shape it was stored in up to the
// shape current code expects for its Type. It must be deterministic: replay feeds
// the same stored events through the same upcasters and must reproduce identical
// results (see ARCHITECTURE.md, the determinism invariant). An Upcaster should
// branch on the event's SchemaVersion and return it unchanged when already
// current.
type Upcaster func(Event) Event

// UpcastRegistry maps an event Type to the Upcaster that brings older payloads of
// that Type up to current. A consumer that folds historical events applies the
// registry first, so the reducer only ever sees current-shaped payloads. Holding
// the registry as an explicit value (rather than package-global mutable state)
// keeps upcasting deterministic across runs and replays.
type UpcastRegistry map[string]Upcaster

// Apply upcasts e if its Type has a registered Upcaster, otherwise returns it
// unchanged. The Upcaster itself decides, from e.SchemaVersion, whether any
// migration is needed.
func (r UpcastRegistry) Apply(e Event) Event {
	if up, ok := r[e.Type]; ok {
		return up(e)
	}
	return e
}
