package state

// SkillTrigger is a declared reason for the host to load a skill's body into a run
// without the model asking. Which fields apply depends on Kind: "file" and "tool" use
// Match (a path glob, a tool name), "repeat" uses Count and "slow" uses Seconds. A store
// that drops [Skill.Triggers] loses the behaviour the author declared, so each store
// returns the list it was given.
type SkillTrigger struct {
	Kind    string `json:"kind"`
	Match   string `json:"match,omitempty"`
	Count   int    `json:"count,omitempty"`
	Seconds int    `json:"seconds,omitempty"`
}
