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

// ModelAuditor rules on terms no exit code settles ("changes stay within scope") by asking
// a model over the run's recorded events. Three rules keep this weaker auditor honest:
//
//   - It refuses an absence claim, because a log shows "nothing found" whether or not
//     anyone looked. Such a term slipped past goal.AssertsAbsence at admission.
//   - It refuses a term that declares a check, which must be run.
//   - A verdict must cite the record, because an uncited "holds" is the easiest answer
//     under completion pressure. The citation is recorded.
type ModelAuditor struct {
	model     llm.Model
	log       spine.Log
	system    string
	maxTokens int
	maxEvents int
}

// defaultAuditSystem asks for the absence check and the citation before the verdict,
// because a model that has written "holds" will justify it, while one that has named
// what it read has bounded what it can claim.
const defaultAuditSystem = `You audit one term of an autonomous run: something the run was required to keep true the whole time it worked. You are given the term and the run's own recorded event log.

Rule on the term from the record alone. Where the record does not show something, that is not permission to assume the best about it.

Answer three things, in this order.
1. ABSENCE. Is the term a claim that something is NOT there (no X remains, nothing was touched, X was removed, the tree is clean of Y)? Say so. A log cannot settle such a claim: it does not contain what is absent, and reading it would produce "I found nothing" whether or not anyone looked. Report absence: true and stop; do not rule on it.
2. CITE. Name what in the record you actually read to reach your verdict: the events, by type and by what they say. If nothing in the record bears on the term, cite nothing and say the record does not show it.
3. RULE. Only then say whether the term held. A verdict must follow from what you cited. If what you cited does not settle the term, the term did not hold, and the detail says the record does not show it.

Return ONLY a JSON object (no prose, no code fence):
{"absence":boolean,"cited":string,"held":boolean,"detail":string}
"cited" is what you read. "detail" is what you observed, and is required whenever held is false.`

// maxAuditEvents keeps the tail of the record, where the audited step is, to bound cost.
// A term that needs the whole history wants a check.
const maxAuditEvents = 200

// maxAuditPayload bounds one event's payload, so a large tool result cannot crowd out the
// rest of the record.
const maxAuditPayload = 400

// ModelAuditorOption configures a ModelAuditor.
type ModelAuditorOption func(*ModelAuditor)

// WithAuditSystem overrides the system prompt. An override that drops the absence check
// or the citation lets a model rule first and justify afterwards.
func WithAuditSystem(s string) ModelAuditorOption {
	return func(a *ModelAuditor) {
		if strings.TrimSpace(s) != "" {
			a.system = s
		}
	}
}

// WithAuditMaxTokens caps the model's output (default 1024).
func WithAuditMaxTokens(n int) ModelAuditorOption {
	return func(a *ModelAuditor) {
		if n > 0 {
			a.maxTokens = n
		}
	}
}

// WithAuditWindow sets how many recent events one audit reads (default 200).
func WithAuditWindow(n int) ModelAuditorOption {
	return func(a *ModelAuditor) {
		if n > 0 {
			a.maxEvents = n
		}
	}
}

// NewModelAuditor asks m over the record on log, and records audits on the same log, so
// pass the run's own.
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

// Audit returns a breach for each term that did not hold. The record is read once, so
// every term in a pass is judged against the same run.
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

// auditTerm refuses the two replies that look like verdicts and are not: an absence
// claim and an uncited ruling.
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

type auditVerdict struct {
	Absence bool   `json:"absence"`
	Cited   string `json:"cited"`
	Held    bool   `json:"held"`
	Detail  string `json:"detail"`
}

// parseAuditVerdict takes the outermost object, because models wrap JSON in prose or a
// fence. An unparseable reply is Terminal, because the zero verdict reads as "holds".
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

// extractObject returns text from the first "{" to the last "}", or "".
func extractObject(text string) string {
	start := strings.IndexByte(text, '{')
	end := strings.LastIndexByte(text, '}')
	if start < 0 || end <= start {
		return ""
	}
	return text[start : end+1]
}

// auditPrompt includes the objective, because "changes stay in scope" means nothing
// without the scope it describes.
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

// readRecord renders the tail of the goal's stream, so the auditor rules on what the run
// did, not what it says it did.
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

// renderPayload sorts keys, so a term is never judged differently on map iteration
// order.
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

// recordAudit marks the audit asserted, because nothing was run, and keeps the citation
// so the verdict can be checked later. A failed write fails the audit, as in
// [CommandAuditor].
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
