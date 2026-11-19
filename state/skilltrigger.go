package state

import "github.com/ionalpha/flynn/skill/skillmd"

// SkillTrigger is a declared reason for the host to load a skill's body into a run
// without the model asking. It is the [skillmd.Trigger] a SKILL.md carries, so a store
// that drops [Skill.Triggers] loses the behaviour the author declared.
type SkillTrigger = skillmd.Trigger
