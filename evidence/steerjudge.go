package evidence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
	"github.com/ionalpha/flynn/spine"
)

// ModelSteerJudge rules on whether a run's account of finishing addresses the redirects
// its operator issued. It is the discharge half of the gated obligation: a redirect is
// delivered to the run every turn, and this decides whether what came back answers it.
//
// The question is small on purpose. It is not "did the run do a good job" and not "is the
// objective met"; both of those are asked elsewhere and the answers do not transfer. It is
// "does this sentence say what was done about that instruction", which is a classification
// a cheap model can make, and one where the cheap model's mistakes fall the safe way: a
// judge that says no keeps the obligation open and the run is asked again, where a judge
// that says yes has spent the operator's only guarantee.
//
// Two rules keep it from becoming a formality.
//
// It must quote the account. A verdict of addressed with nothing quoted is the sentence a
// model produces most easily when it has not read carefully, so an unquoted acceptance is
// treated as no judgement at all and the redirect stays outstanding. What was quoted is
// recorded beside the verdict, so a reader can go back to the run's own words.
//
// Naming the redirect is not addressing it. "As requested, I have handled the table issue"
// says nothing about which table anything was written to, and a run under completion
// pressure produces exactly that. The instruction says so, and the quote requirement is
// what makes it checkable: an acceptance has to point at words that describe an action.
type ModelSteerJudge struct {
	model     llm.Model
	log       spine.Log
	system    string
	maxTokens int
}

// defaultSteerSystem frames the model as ruling on one redirect against one account, and
// puts the quote before the verdict. The order is the point: a model that has already
// written "addressed" will justify it, and a model that has already had to find the words
// has bounded what it can claim.
const defaultSteerSystem = `An operator interrupted an autonomous run to redirect it. The run has now reported that it finished, and given an account of what it did. Decide whether that account says what the run did about the redirect.

Answer two things, in this order.
1. QUOTE. Find the part of the account that describes what was done about the redirect, and quote it. Naming the subject is not addressing it: "I handled the table issue" describes nothing that was done. Saying the redirect was received is not addressing it either. If the account does not describe an action that responds to the redirect, quote nothing.
2. RULE. Only then say whether the redirect was addressed. It was addressed only if you quoted something, and only if what you quoted responds to what the operator asked for. An account describing the opposite of what was asked has not addressed it.

Return ONLY a JSON object (no prose, no code fence):
{"quote":string,"addressed":boolean}`

// ModelSteerJudgeOption configures a ModelSteerJudge.
type ModelSteerJudgeOption func(*ModelSteerJudge)

// WithSteerSystem overrides the standing instruction framing the judgement. The default
// puts the quote ahead of the verdict; an override that drops it leaves a model free to
// rule first and account for it afterwards.
func WithSteerSystem(s string) ModelSteerJudgeOption {
	return func(j *ModelSteerJudge) {
		if strings.TrimSpace(s) != "" {
			j.system = s
		}
	}
}

// WithSteerMaxTokens caps the output length requested of the model.
func WithSteerMaxTokens(n int) ModelSteerJudgeOption {
	return func(j *ModelSteerJudge) {
		if n > 0 {
			j.maxTokens = n
		}
	}
}

// NewModelSteerJudge builds a judge that rules on acknowledgements by asking m, recording
// each judgement on log. The log is the run's own, so what the operator asked and what the
// run answered land in the record beside the work they are about.
func NewModelSteerJudge(m llm.Model, log spine.Log, opts ...ModelSteerJudgeOption) *ModelSteerJudge {
	j := &ModelSteerJudge{model: m, log: log, system: defaultSteerSystem, maxTokens: 512}
	for _, o := range opts {
		o(j)
	}
	return j
}

var _ goal.SteerJudge = (*ModelSteerJudge)(nil)

// Acknowledged rules on each outstanding redirect against the run's account and returns one
// acknowledgement per redirect the account addressed.
//
// Every redirect is ruled on separately. A run that answered one and ignored another has
// answered one, and asking about them together invites a single verdict over the pile,
// which is the reading that discharges the ignored one.
func (j *ModelSteerJudge) Acknowledged(ctx context.Context, r resource.Resource, spec goal.Spec, _ goal.Status, outstanding []goal.Steer, account string) ([]goal.Acknowledgement, error) {
	if len(outstanding) == 0 {
		return nil, nil
	}
	if j.model == nil {
		return nil, fault.New(fault.Terminal, "steer_no_model",
			"steer: no model wired to rule on whether the run addressed the operator's redirect")
	}
	// An account with nothing in it cannot address anything, and asking a model to confirm
	// that costs a call to learn what is already known.
	if strings.TrimSpace(account) == "" {
		for _, st := range outstanding {
			if err := j.record(ctx, r, st, false, "", account); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}
	var acks []goal.Acknowledgement
	for _, st := range outstanding {
		quote, addressed, err := j.judge(ctx, spec, st, account)
		if err != nil {
			return nil, err
		}
		if err := j.record(ctx, r, st, addressed, quote, account); err != nil {
			return nil, err
		}
		if addressed {
			acks = append(acks, goal.Acknowledgement{ID: st.ID, How: quote})
		}
	}
	return acks, nil
}

// judge asks the model about one redirect and turns the reply into a verdict, refusing the
// reply that looks like an acceptance and is not: addressed, with nothing quoted.
func (j *ModelSteerJudge) judge(ctx context.Context, spec goal.Spec, st goal.Steer, account string) (quote string, addressed bool, err error) {
	resp, gerr := j.model.Generate(ctx, llm.Request{
		System:    j.system,
		Messages:  []llm.Message{llm.Text(llm.RoleUser, steerPrompt(spec, st, account))},
		MaxTokens: j.maxTokens,
	})
	if gerr != nil {
		if ctx.Err() != nil {
			return "", false, ctx.Err()
		}
		return "", false, fault.Wrap(fault.Classify(gerr), "steer_model",
			fmt.Errorf("steer: %s: the judge could not be asked: %w", st.ID, gerr))
	}
	v, perr := parseSteerVerdict(resp.Message.TextContent())
	if perr != nil {
		return "", false, fmt.Errorf("steer: %s: %w", st.ID, perr)
	}
	quote = strings.TrimSpace(v.Quote)
	if !v.Addressed {
		return "", false, nil
	}
	if quote == "" {
		// The verdict rests on nothing, which is not a judgement. The safe reading is the
		// one that keeps the obligation open: the run is refused its completion and the
		// operator's instruction stands, rather than being discharged by a model that did
		// not point at anything.
		return "", false, nil
	}
	return clip(quote, maxDetail), true, nil
}

// steerVerdict is the wire shape the judge returns.
type steerVerdict struct {
	Quote     string `json:"quote"`
	Addressed bool   `json:"addressed"`
}

// parseSteerVerdict extracts the JSON object from the reply. Models wrap JSON in prose or a
// code fence despite instructions, so the outermost object is taken. A reply with no object,
// or one that does not parse, is a terminal fault rather than a default verdict: the run may
// well have complied, and recording it as having ignored its operator would put a broken
// judge's failure onto the run's record as a fact about the run.
func parseSteerVerdict(text string) (steerVerdict, error) {
	raw := extractObject(text)
	if raw == "" {
		return steerVerdict{}, fault.New(fault.Terminal, "steer_parse", "the judge returned no JSON object")
	}
	var v steerVerdict
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return steerVerdict{}, fault.Wrap(fault.Terminal, "steer_parse", err)
	}
	return v, nil
}

// steerPrompt renders the objective, the redirect and the run's account. The objective is
// included because a redirect is stated against it: "use the other table" is only legible
// next to what the run was trying to do.
func steerPrompt(spec goal.Spec, st goal.Steer, account string) string {
	var b strings.Builder
	b.WriteString("The run's objective:\n")
	b.WriteString(spec.Objective)
	b.WriteString("\n\nThe operator's redirect:\n")
	b.WriteString(st.Instruction)
	b.WriteString("\n\nThe run's account of what it did:\n")
	b.WriteString(account)
	return b.String()
}

// record appends the judgement to the goal's stream, marked as asserted: nothing was run
// for this verdict, and the record says so rather than letting a model's judgement read
// like an observed exit code. The account rides along, because a refused acknowledgement is
// only legible next to what was actually said, and the transcript that sentence came from
// is pruned and eventually gone.
//
// A failure to write it fails the judgement, for the reason it does in the auditors: a
// judgement nobody can show happened is what the record exists to prevent.
func (j *ModelSteerJudge) record(ctx context.Context, r resource.Resource, st goal.Steer, addressed bool, quote, account string) error {
	if j.log == nil {
		return fault.New(fault.Terminal, "steer_no_log",
			"steer: no spine log wired to record the judgement on")
	}
	payload := map[string]any{
		chain.SteerKey:          st.ID,
		chain.SteerAddressedKey: addressed,
		chain.SteerAccountKey:   clip(account, maxDetail),
		chain.ItemProvenanceKey: chain.ProvenanceAsserted,
	}
	if quote != "" {
		payload[chain.SteerHowKey] = quote
	}
	if _, err := j.log.Append(ctx, spine.AppendInput{
		Stream:  r.Name,
		Type:    chain.SteerJudged,
		Actor:   spine.ActorSystem,
		Payload: payload,
	}); err != nil {
		return fault.Wrap(fault.Transient, "steer_append", err)
	}
	return nil
}
