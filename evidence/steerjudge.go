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

// ModelSteerJudge decides whether a run's finishing account addresses each operator
// redirect. It asks only "does this say what was done about that instruction", so a
// cheap model can answer, and a wrong no only asks the run again.
//
// The judge must quote the account, and the quote is recorded. Naming the redirect is not
// addressing it ("I have handled the table issue"), so the quote must describe an action.
type ModelSteerJudge struct {
	model     llm.Model
	log       spine.Log
	system    string
	maxTokens int
}

// defaultSteerSystem asks for the quote before the verdict, because a model that has
// written "addressed" will justify it.
const defaultSteerSystem = `An operator interrupted an autonomous run to redirect it. The run has now reported that it finished, and given an account of what it did. Decide whether that account says what the run did about the redirect.

Answer two things, in this order.
1. QUOTE. Find the part of the account that describes what was done about the redirect, and quote it. Naming the subject is not addressing it: "I handled the table issue" describes nothing that was done. Saying the redirect was received is not addressing it either. If the account does not describe an action that responds to the redirect, quote nothing.
2. RULE. Only then say whether the redirect was addressed. It was addressed only if you quoted something, and only if what you quoted responds to what the operator asked for. An account describing the opposite of what was asked has not addressed it.

Return ONLY a JSON object (no prose, no code fence):
{"quote":string,"addressed":boolean}`

// ModelSteerJudgeOption configures a ModelSteerJudge.
type ModelSteerJudgeOption func(*ModelSteerJudge)

// WithSteerSystem overrides the system prompt. An override that drops the quote lets a
// model rule first and justify afterwards.
func WithSteerSystem(s string) ModelSteerJudgeOption {
	return func(j *ModelSteerJudge) {
		if strings.TrimSpace(s) != "" {
			j.system = s
		}
	}
}

// WithSteerMaxTokens caps the model's output (default 512).
func WithSteerMaxTokens(n int) ModelSteerJudgeOption {
	return func(j *ModelSteerJudge) {
		if n > 0 {
			j.maxTokens = n
		}
	}
}

// NewModelSteerJudge asks m and records each judgement on log, which should be the run's
// own so the judgement sits beside the work.
func NewModelSteerJudge(m llm.Model, log spine.Log, opts ...ModelSteerJudgeOption) *ModelSteerJudge {
	j := &ModelSteerJudge{model: m, log: log, system: defaultSteerSystem, maxTokens: 512}
	for _, o := range opts {
		o(j)
	}
	return j
}

var _ goal.SteerJudge = (*ModelSteerJudge)(nil)

// Acknowledged returns one acknowledgement per redirect the account addressed. Each is
// judged separately, because one verdict over several would discharge an ignored one.
func (j *ModelSteerJudge) Acknowledged(ctx context.Context, r resource.Resource, spec goal.Spec, _ goal.Status, outstanding []goal.Steer, account string) ([]goal.Acknowledgement, error) {
	if len(outstanding) == 0 {
		return nil, nil
	}
	if j.model == nil {
		return nil, fault.New(fault.Terminal, "steer_no_model",
			"steer: no model wired to rule on whether the run addressed the operator's redirect")
	}
	// An empty account addresses nothing, so no model call is needed.
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

// judge treats "addressed" with nothing quoted as not addressed.
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
		// An uncited acceptance keeps the operator's instruction open.
		return "", false, nil
	}
	return clip(quote, maxDetail), true, nil
}

type steerVerdict struct {
	Quote     string `json:"quote"`
	Addressed bool   `json:"addressed"`
}

// parseSteerVerdict takes the outermost object. An unparseable reply is Terminal, not a
// "not addressed" verdict, so a broken judge is not recorded as a fact about the run.
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

// steerPrompt includes the objective, because "use the other table" is legible only next
// to it.
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

// record marks the judgement asserted and keeps the account, because the transcript it
// came from is pruned. A failed write fails the judgement, as in the auditors.
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
