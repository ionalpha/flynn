package spine

// Upcaster migrates an event's payload from its stored SchemaVersion to the
// shape current code expects, and returns a current event unchanged. A Type
// needs one only after a key is renamed, removed or retyped. It must be
// deterministic because replay must reproduce identical results.
type Upcaster func(Event) Event

// UpcastRegistry maps an event Type to its Upcaster. A fold over historical
// events applies it first, so the reducer sees only current shapes. It is a
// value instead of global state so replays stay deterministic.
type UpcastRegistry map[string]Upcaster

// Apply runs e's Upcaster if its Type has one, else returns e unchanged.
func (r UpcastRegistry) Apply(e Event) Event {
	if up, ok := r[e.Type]; ok {
		return up(e)
	}
	return e
}
