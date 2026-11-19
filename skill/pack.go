package skill

import (
	"fmt"

	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// FromPack maps a loaded skill directory to a skill in scope. Resources stay in the
// pack, read by path at execution time from the caller's fs.FS.
func FromPack(p skillmd.Pack, scope state.Scope) (state.Skill, error) {
	return FromDoc(p.Doc, scope)
}

// FromDoc maps a SKILL.md document to a skill in scope, with the name as slug. Pass a
// document [skillmd.Validate] accepted; only metadata decoding can fail here.
//
// Three things do not cross. Offers, Reads and Wins are evidence from this machine,
// so a pack cannot arrive pre-ranked. AllowedTools is dropped because honouring it
// lets a text file pre-approve tools; admission grants tools. Metadata keys outside
// our namespace are dropped because a skill cannot store them, so only Parse then
// Format round-trips a pack byte for byte.
func FromDoc(d skillmd.Doc, scope state.Scope) (state.Skill, error) {
	sk := state.Skill{
		Slug:        d.Name,
		Name:        d.Name,
		Description: d.Description,
		Body:        d.Body,
		Scope:       scope,
	}
	// skillmd.Get also reads the retired namespace.
	sk.Check, _ = skillmd.Get(d.Metadata, skillmd.MetaCheck)
	if title, _ := skillmd.Get(d.Metadata, skillmd.MetaTitle); title != "" {
		sk.Name = title
	}
	if raw, ok := skillmd.Get(d.Metadata, skillmd.MetaTags); ok {
		tags, err := skillmd.DecodeList(raw)
		if err != nil {
			return state.Skill{}, fmt.Errorf("skill: %s: %w", d.Name, err)
		}
		sk.Tags = tags
	}
	if raw, ok := skillmd.Get(d.Metadata, skillmd.MetaTriggers); ok {
		ts, err := skillmd.DecodeTriggers(raw)
		if err != nil {
			return state.Skill{}, fmt.Errorf("skill: %s: %w", d.Name, err)
		}
		for _, t := range ts {
			sk.Triggers = append(sk.Triggers, state.SkillTrigger(t))
		}
	}
	return sk, nil
}

// ToDoc maps a stored skill to a SKILL.md document, ready for Validate. A stored slug
// can be an illegal skill name, so it is checked here, where the error can name the
// skill.
func ToDoc(sk state.Skill) (skillmd.Doc, error) {
	if err := skillmd.ValidateName(sk.Slug); err != nil {
		return skillmd.Doc{}, fmt.Errorf("skill: slug %q is not a valid skill name: %w", sk.Slug, err)
	}
	d := skillmd.Doc{
		Name:        sk.Slug,
		Description: sk.Description,
		Body:        sk.Body,
	}
	meta := map[string]string{}
	if sk.Name != "" && sk.Name != sk.Slug {
		meta[skillmd.MetaTitle] = sk.Name
	}
	if sk.Check != "" {
		meta[skillmd.MetaCheck] = sk.Check
	}
	if len(sk.Tags) > 0 {
		meta[skillmd.MetaTags] = skillmd.EncodeList(sk.Tags)
	}
	if len(sk.Triggers) > 0 {
		ts := make([]skillmd.Trigger, len(sk.Triggers))
		for i, t := range sk.Triggers {
			ts[i] = skillmd.Trigger(t)
		}
		meta[skillmd.MetaTriggers] = skillmd.EncodeTriggers(ts)
	}
	if len(meta) > 0 {
		d.Metadata = meta
	}
	return d, nil
}

// Export writes skills as a conformant skill tree under dir, one directory each. The
// whole set is validated first, so a bad skill names its slug and nothing is half
// written. The usual failure is a missing description, which the format requires.
func Export(dir string, skills []state.Skill) error {
	docs := make([]skillmd.Doc, 0, len(skills))
	for _, sk := range skills {
		doc, err := ToDoc(sk)
		if err != nil {
			return err
		}
		if err := skillmd.Validate(doc, doc.Name); err != nil {
			return fmt.Errorf("skill: %s cannot be exported: %w", sk.Slug, err)
		}
		docs = append(docs, doc)
	}
	return skillmd.WriteAll(dir, docs)
}
