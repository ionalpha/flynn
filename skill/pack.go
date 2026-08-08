package skill

import (
	"fmt"

	"github.com/ionalpha/flynn/skill/skillmd"
	"github.com/ionalpha/flynn/state"
)

// The mapping between a SKILL.md document and a stored skill.
//
// It lives here rather than in skillmd because skillmd is a codec and knows nothing
// about persistence, and because this is the one place that has to answer what the
// format's fields mean to us: which of our fields the format has a home for, which
// ride in metadata under the convention in skillmd/metadata.go, and which do not
// cross at all.
//
// Three fields do not cross, each for its own reason.
//
// Offers, Reads and Wins are evidence about runs on this machine. They are not
// content, they do not describe the skill, and importing someone else's counters
// would let a pack arrive pre-ranked. An imported skill starts with none.
//
// AllowedTools is dropped on import, never honoured. It is a text file declaring
// which tools are pre-approved to run, which is a privilege-escalation path into the
// capability model; the specification marks it experimental and says nothing about
// trust. A tool a skill needs is granted through admission, by us, or not at all.
//
// Metadata keys outside our namespace are dropped on import, because a stored skill
// has nowhere to keep them. The lossless round trip the codec promises is at the
// document level: Parse then Format preserves a foreign pack byte for byte. Going
// through the store is a conversion, and this comment is where it says so.

// FromPack maps a loaded skill directory to a skill in scope. The pack's resources
// stay in the pack: they are addressed by path and read at execution time, and the
// caller keeps the fs.FS they came from.
func FromPack(p skillmd.Pack, scope state.Scope) (state.Skill, error) {
	return FromDoc(p.Doc, scope)
}

// FromDoc maps a SKILL.md document to a skill in scope. The document's name is the
// slug, since both are the stable identifier and both are already constrained to the
// same shape. Pass a document Validate has accepted; FromDoc reads it, it does not
// re-judge it, except for the metadata it decodes, which can fail on its own.
func FromDoc(d skillmd.Doc, scope state.Scope) (state.Skill, error) {
	sk := state.Skill{
		Slug:        d.Name,
		Name:        d.Name,
		Description: d.Description,
		Body:        d.Body,
		Scope:       scope,
		Check:       d.Metadata[skillmd.MetaCheck],
	}
	if title := d.Metadata[skillmd.MetaTitle]; title != "" {
		sk.Name = title
	}
	if raw, ok := d.Metadata[skillmd.MetaTags]; ok {
		tags, err := skillmd.DecodeList(raw)
		if err != nil {
			return state.Skill{}, fmt.Errorf("skill: %s: %w", d.Name, err)
		}
		sk.Tags = tags
	}
	return sk, nil
}

// ToDoc maps a stored skill to a SKILL.md document, ready for Validate.
//
// The slug becomes the name, and that is the conversion most likely to fail: our
// slugs come from a database and the format's names are constrained, so a slug that
// was fine as a database key can be illegal as a skill name. It is checked here, at
// the boundary, rather than by whatever writes the file, because the caller that
// gets an error here can still say which skill it was about.
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
	if len(meta) > 0 {
		d.Metadata = meta
	}
	return d, nil
}

// Export writes skills as a conformant skill tree under dir, one directory per
// skill, ready to be dropped into any harness that reads the format.
//
// The whole set is converted before anything is written, so a skill the format
// cannot express stops the export with its slug named instead of leaving a directory
// half-written. The usual cause is a description: the format requires one, and a
// skill distilled before descriptions existed has none.
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
