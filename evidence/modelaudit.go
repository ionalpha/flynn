package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// ModelAuditor rules on the terms that do not reduce to a command. Some of what a run is
// held to is genuinely a judgement about how it worked ("changes stay within the scope the
// objective describes", "the existing public API keeps its meaning"), and no exit code
// settles those. It reads the run's own recorded events and asks a model.
//
// It is the weaker of the two auditors and is built to know it. Three rules keep it from
// becoming a formality.
//
// It will not rule on an absence. A term claiming something is not there cannot be settled
// by reading a log, because a log does not contain what is absent, and a model reading one
// will report the same "I found nothing" whether the run swept the tree or never looked.
// Admission already refuses an unsearchable term (goal.AssertsAbsence), so a term reaching
// here that the model itself reads as an absence claim is one whose wording defeated that
// recognizer. It is refused rather than passed, which makes the two layers independent:
// getting an absence claim waved through takes wording that fools a word list and a model
// that then denies it is one.
//
// It will not stand in for a check that was written. A term declaring a check is audited by
// running it; a model asked to judge from the record whether `grep -r AWS_SECRET .` would
// have exited zero is guessing at an answer that was one command away.
//
// A verdict must cite the record. "The term holds" with nothing pointed at is the sentence
// a run under completion pressure produces most easily, so an uncited verdict is treated as
// no audit at all, and the goal stops. What was cited goes on the spine beside the verdict,
// so a reader can go back to what was looked at rather than taking the verdict's word.
type ModelAuditor struct {
	model     llm.Model
	log       spine.Log
	system    string
	maxTokens int
	maxEvents int
}

// defaultAuditSystem frames the model as an auditor rather than a reviewer, and puts the
// two disclosures it must make (is this an absence claim, what did you read) before the
// verdict. The order is the point: a model that has already written "holds" will justify
// it, and a model that has already named what it read has bounded what it can claim.
const defaultAuditSystem = `You audit one term of an autonomous run: something the run was required to keep true the whole time it worked. You are given the term and the run's own recorded event log.

Rule on the term from the record alone. Where the record does not show something, that is not permission to assume the best about it.

Answer three things, in this order.
1. ABSENCE. Is the term a claim that something is NOT there — no X remains, nothing was touched, X was removed, the tree is clean of Y? Say so. A log cannot settle such a claim: it does not contain what is absent, and reading it would produce "I found nothing" whether or not anyone looked. Report absence: true and stop; do not rule on it.
2. CITE. Name what in the record you actually read to reach your verdict: the events, by type and by what they say. If nothing in the record bears on the term, cite nothing and say the record does not show it.
3. RULE. Only then say whether the term held. A verdict must follow from what you cited. If what you cited does not settle the term, the term did not hold, and the detail says the record does not show it.

Return ONLY a JSON object (no prose, no code fence):
{"absence":boolean,"cited":string,"held":boolean,"detail":string}
"cited" is what you read. "detail" is what you observed, and is required whenever held is false.`

// maxAuditEvents bounds how much of the run's record one audit renders. An audit runs after
// every step, so the record it reads grows across a run while the term it rules on does
// not, and the tail is where the step being audited is. It is a cost bound, not a
// correctness one: a term that needs the whole history to settle is a term wanting a check.
const maxAuditEvents = 200

// maxAuditPayload bounds one rendered event's payload, so a single large tool result does
// not crowd the rest of the record out of the request.
const maxAuditPayload = 400

// ModelAuditorOption configures a ModelAuditor.
type ModelAuditorOption func(*ModelAuditor)

// WithAuditSystem overrides the standing instruction framing the audit. The default puts
// the absence disclosure and the citation ahead of the verdict; an override that drops
// either leaves a model free to rule first and account for it afterwards.
func WithAuditSystem(s string) ModelAuditorOption {
	return func(a *ModelAuditor) {
		if strings.TrimSpace(s) != "" {
			a.system = s
		}
	}
}

// WithAuditMaxTokens caps the output length requested of the model.
func WithAuditMaxTokens(n int) ModelAuditorOption {
	return func(a *ModelAuditor) {
		if n > 0 {
			a.maxTokens = n
		}
	}
}

// WithAuditWindow sets how many of the run's most recent events one audit reads.
func WithAuditWindow(n int) ModelAuditorOption {
	return func(a *ModelAuditor) {
		if n > 0 {
			a.maxEvents = n
		}
	}
}

// NewModelAuditor builds an auditor that rules on prose terms by asking m over the run's
// record on log. The log is both what the auditor reads and where the audit is recorded,
// so it is the same one the rest of the run writes to.
func NewModelAuditor(m llm.Model, log spine.Log, opts ...ModelAuditorOption) *ModelAuditor {
	a := &ModelAuditor{
		model:     m,
		log:       log,
		system:    defaultAuditSystem,
		maxTokens: 1024,
		maxEvents: maxAuditEvents,
	}
	for _, o := range opts {
		o(a)
	}
	return a
}

var _ goal.InvariantAuditor = (*ModelAuditor)(nil)

// Audit rules on every term it is handed and returns a breach for each that did not hold.
//
// The record is read once and every term is ruled on against that one reading, so all the
// terms of a pass are judged against the same run rather than against a record that moved
// underneath them.
func (a *ModelAuditor) Audit(ctx context.Context, r resource.Resource, spec goal.Spec, _ goal.Status, terms []goal.Invariant) ([]goal.Breach, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	if a.model == nil {
		return nil, fault.New(fault.Terminal, "audit_no_model",
			"audit: no model wired to rule on the terms that declare no check")
	}
	record, err := a.readRecord(ctx, r)
	if err != nil {
		return nil, err
	}
	var breaches []goal.Breach
	for _, term := range terms {
		held, detail, cited, err := a.auditTerm(ctx, spec, term, record)
		if err != nil {
			return nil, err
		}
		if err := a.recordAudit(ctx, r, term, held, detail, cited); err != nil {
			return nil, err
		}
		if !held {
			breaches = append(breaches, goal.Breach{ID: term.ID, Detail: detail})
		}
	}
	return breaches, nil
}

// auditTerm asks the model about one term and turns the reply into a verdict, refusing the
// two replies that look like verdicts and are not: a term the model reads as an absence
// claim, and a verdict resting on nothing.
func (a *ModelAuditor) auditTerm(ctx context.Context, spec goal.Spec, term goal.Invariant, record string) (held bool, detail, cited string, err error) {
	if strings.TrimSpace(term.Check) != "" {
		return false, "", "", fault.New(fault.Terminal, "audit_check_not_run",
			"audit: invariant "+term.ID+" declares a check, and reading the record is not running it")
	}
	resp, gerr := a.model.Generate(ctx, llm.Request{
		System:    a.system,
		Messages:  []llm.Message{llm.Text(llm.RoleUser, auditPrompt(spec, term, record))},
		MaxTokens: a.maxTokens,
	})
	if gerr != nil {
		if ctx.Err() != nil {
			return false, "", "", ctx.Err()
		}
		return false, "", "", fault.Wrap(fault.Classify(gerr), "audit_model",
			fmt.Errorf("audit: invariant %s: the auditor could not be asked: %w", term.ID, gerr))
	}
	v, perr := parseAuditVerdict(resp.Message.TextContent())
	if perr != nil {
		return false, "", "", fmt.Errorf("audit: invariant %s: %w", term.ID, perr)
	}
	if v.Absence {
		return false, "", "", fault.New(fault.Terminal, "audit_absence_unsearched",
			"audit: invariant "+term.ID+" asserts that something is not there and declares no search that would find one, "+
				"so reading the run's record cannot settle it: give the term a check")
	}
	cited = strings.TrimSpace(v.Cited)
	if cited == "" {
		return false, "", "", fault.New(fault.Terminal, "audit_uncited",
			"audit: invariant "+term.ID+" was ruled on without citing anything in the run's record, which is not an audit")
	}
	if v.Held {
		return true, "", cited, nil
	}
	detail = strings.TrimSpace(v.Detail)
	if detail == "" {
		detail = "the auditor found the term broken and said nothing about what it observed"
	}
	return false, clip(detail, maxDetail), cited, nil
}

// auditVerdict is the wire shape the auditor returns.
type auditVerdict struct {
	Absence bool   `json:"absence"`
	Cited   string `json:"cited"`
	Held    bool   `json:"held"`
	Detail  string `json:"detail"`
}

// parseAuditVerdict extracts the JSON object from the reply. Models wrap JSON in prose or
// a code fence despite instructions, so the outermost object is taken. A reply with no
// object, or one that does not parse, is a terminal fault rather than a default verdict:
// the default that suggests itself is the zero value, which reads as "the term holds", and
// an auditor whose parse failure means "holds" is worse than no auditor.
func parseAuditVerdict(text string) (auditVerdict, error) {
	raw := extractObject(text)
	if raw == "" {
		return auditVerdict{}, fault.New(fault.Terminal, "audit_parse", "the auditor returned no JSON object")
	}
	var v auditVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return auditVerdict{}, fault.Wrap(fault.Terminal, "audit_parse", err)
	}
	return v, nil
}

// extractObject returns the outermost JSON object in text (from the first "{" to the last
// "}"), or "" if there is none.
func extractObject(text string) string {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}

// auditPrompt renders the goal's objective, the term under audit and the run's record. The
// objective is included because a term is stated against it: "changes stay in scope" means
// nothing without the scope the goal describes.
func auditPrompt(spec goal.Spec, term goal.Invariant, record string) string {
	var b strings.Builder
	b.WriteString("The run's objective:\n")
	b.WriteString(spec.Objective)
	b.WriteString("\n\nThe term under audit:\n")
	fmt.Fprintf(&b, "%s: %s\n", term.ID, term.Statement)
	b.WriteString("\n--- The run's recorded events ---\n")
	if record == "" {
		b.WriteString("(the run has recorded nothing)\n")
	} else {
		b.WriteString(record)
	}
	return b.String()
}

// readRecord renders the tail of the run's own event stream. It reads the goal's stream,
// which is where the run's steps, tool calls and governance decisions land, so the auditor
// rules over what the run did rather than over what the run says it did.
func (a *ModelAuditor) readRecord(ctx context.Context, r resource.Resource) (string, error) {
	if a.log == nil {
		return "", fault.New(fault.Terminal, "audit_no_log",
			"audit: no spine log wired to read the run's record from")
	}
	events, err := a.log.Read(ctx, spine.Query{Stream: r.Name})
	if err != nil {
		return "", fault.Wrap(fault.Transient, "audit_read", err)
	}
	if len(events) > a.maxEvents {
		events = events[len(events)-a.maxEvents:]
	}
	var b strings.Builder
	for _, e := range events {
		fmt.Fprintf(&b, "%d %s %s %s\n", e.Seq, e.Time.UTC().Format("15:04:05"), e.Actor, e.Type)
		if payload := renderPayload(e.Payload); payload != "" {
			b.WriteString("    ")
			b.WriteString(payload)
			b.WriteByte('\n')
		}
	}
	return b.String(), nil
}

// renderPayload flattens an event payload into one line with its keys in a stable order, so
// two renderings of the same record are the same text and a term is not judged differently
// on a map iteration. Each value is clipped, because one large tool result would otherwise
// crowd the rest of the record out of the window.
func renderPayload(payload map[string]any) string {
	if len(payload) == 0 {
		return ""
	}
	keys := make([]string, 0, len(payload))
	for k := range payload {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+clip(strings.TrimSpace(fmt.Sprint(payload[k])), maxAuditPayload))
	}
	return strings.Join(parts, " ")
}

// recordAudit appends the audit to the goal's stream, marked as asserted: nothing was run
// for this verdict, and the record says so rather than letting a model's judgement read
// like an observed exit code. What the auditor cited rides along, because a verdict whose
// citation is not kept is one nobody can go back and check.
//
// A failure to write it fails the audit, for the reason it does in the command auditor: an
// audit nobody can show happened is what the record exists to prevent.
func (a *ModelAuditor) recordAudit(ctx context.Context, r resource.Resource, term goal.Invariant, held bool, detail, cited string) error {
	payload := map[string]any{
		chain.InvariantKey:      term.ID,
		chain.InvariantHeldKey:  held,
		chain.InvariantCitedKey: clip(cited, maxDetail),
		chain.ItemProvenanceKey: chain.ProvenanceAsserted,
	}
	if detail != "" {
		payload[chain.InvariantDetailKey] = detail
	}
	// ActorSystem: the runtime asked the auditor and recorded what came back. The
	// asserted marker is what keeps this distinguishable from an executed audit, so the
	// actor does not have to carry that distinction as well.
	if _, err := a.log.Append(ctx, spine.AppendInput{
		Stream:  r.Name,
		Type:    chain.InvariantAudited,
		Actor:   spine.ActorSystem,
		Payload: payload,
	}); err != nil {
		return fault.Wrap(fault.Transient, "audit_append", err)
	}
	return nil
}
