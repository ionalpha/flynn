package evidence

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/chain"
	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm/llmtest"
	"github.com/ionalpha/flynn/spine"
)

func redirect(id, instruction string) goal.Steer {
	return goal.Steer{ID: id, Instruction: instruction}
}

func steerSpec() goal.Spec {
	return goal.Spec{Objective: "add the audit trail", StopCondition: "the trail is written"}
}

// judgedEvents returns the steer judgements recorded on a stream.
func judgedEvents(t *testing.T, log spine.Log, stream string) []spine.Event {
	t.Helper()
	all, err := log.Read(context.Background(), spine.Query{Stream: stream})
	if err != nil {
		t.Fatalf("read record: %v", err)
	}
	var out []spine.Event
	for _, e := range all {
		if e.Type == chain.SteerJudged {
			out = append(out, e)
		}
	}
	return out
}

// TestAnAccountThatSaysWhatItDidDischargesTheRedirect, and the words it was accepted on go
// on the record beside the verdict, so a reader can go back to what the run actually said.
func TestAnAccountThatSaysWhatItDidDischargesTheRedirect(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"moved the writer onto the events table","addressed":true}`))
	j := NewModelSteerJudge(m, log)

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s1"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "you are writing to sessions; write to events instead")},
		"moved the writer onto the events table and re-ran the trail")
	if err != nil {
		t.Fatalf("Acknowledged: %v", err)
	}
	if len(acks) != 1 || acks[0].ID != "steer-1" {
		t.Fatalf("acknowledgements = %+v, want the redirect discharged", acks)
	}
	if acks[0].How != "moved the writer onto the events table" {
		t.Fatalf("how = %q, want what the judge quoted", acks[0].How)
	}

	// What the judge was asked matters as much as what it said: the redirect, the run's
	// account, and the objective the redirect is only legible against.
	reqs := m.Requests()
	if len(reqs) != 1 {
		t.Fatalf("the model was called %d times, want once per redirect", len(reqs))
	}
	asked := reqs[0].Messages[0].TextContent()
	for _, want := range []string{"write to events instead", "re-ran the trail", "add the audit trail"} {
		if !strings.Contains(asked, want) {
			t.Fatalf("the judge was not shown %q:\n%s", want, asked)
		}
	}

	events := judgedEvents(t, log, "run-s1")
	if len(events) != 1 {
		t.Fatalf("judgements on the record = %d, want one", len(events))
	}
	if addressed, _ := events[0].Payload[chain.SteerAddressedKey].(bool); !addressed {
		t.Fatalf("the record does not carry the verdict: %+v", events[0].Payload)
	}
	if how, _ := events[0].Payload[chain.SteerHowKey].(string); how != "moved the writer onto the events table" {
		t.Fatalf("the record does not carry what was quoted: %+v", events[0].Payload)
	}
	if account, _ := events[0].Payload[chain.SteerAccountKey].(string); !strings.Contains(account, "re-ran the trail") {
		t.Fatalf("the record does not carry the account ruled on: %+v", events[0].Payload)
	}
	// Nothing was run to reach this, and the record says so rather than letting a model's
	// judgement read like an observed exit code.
	if prov, _ := events[0].Payload[chain.ItemProvenanceKey].(string); prov != chain.ProvenanceAsserted {
		t.Fatalf("provenance = %q, want asserted", prov)
	}
}

// TestAnAccountThatIgnoresTheRedirectDischargesNothing: the run finished, said what it did,
// and none of it was about what the operator asked for.
func TestAnAccountThatIgnoresTheRedirectDischargesNothing(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"","addressed":false}`))
	j := NewModelSteerJudge(m, log)

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s2"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")},
		"wrote the audit trail to the sessions table")
	if err != nil {
		t.Fatalf("Acknowledged: %v", err)
	}
	if len(acks) != 0 {
		t.Fatalf("an account that ignored the redirect discharged it: %+v", acks)
	}
	events := judgedEvents(t, log, "run-s2")
	if len(events) != 1 {
		t.Fatalf("judgements on the record = %d, want the refusal recorded too", len(events))
	}
	if addressed, _ := events[0].Payload[chain.SteerAddressedKey].(bool); addressed {
		t.Fatalf("the refusal was recorded as an acceptance: %+v", events[0].Payload)
	}
}

// TestAnAcceptanceRestingOnNothingIsNotAnAcknowledgement: "addressed" with nothing quoted
// is the reply a model produces most easily when it has not read carefully, and the safe
// reading keeps the obligation open.
func TestAnAcceptanceRestingOnNothingIsNotAnAcknowledgement(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"   ","addressed":true}`))
	j := NewModelSteerJudge(m, log)

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s3"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "it is all done")
	if err != nil {
		t.Fatalf("Acknowledged: %v", err)
	}
	if len(acks) != 0 {
		t.Fatalf("an unquoted acceptance discharged the redirect: %+v", acks)
	}
}

// TestEachRedirectIsRuledOnSeparately: a run that answered one and ignored another has
// answered one. Asking about them together invites a single verdict over the pile, which
// is the reading that discharges the ignored one.
func TestEachRedirectIsRuledOnSeparately(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(
		llmtest.SayText(`{"quote":"","addressed":false}`),
		llmtest.SayText(`{"quote":"reverted the migration edit","addressed":true}`),
	)
	j := NewModelSteerJudge(m, log)

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s4"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead"), redirect("steer-2", "leave the migration alone")},
		"reverted the migration edit and finished the trail")
	if err != nil {
		t.Fatalf("Acknowledged: %v", err)
	}
	if len(acks) != 1 || acks[0].ID != "steer-2" {
		t.Fatalf("acknowledgements = %+v, want only the redirect that was answered", acks)
	}
	if len(judgedEvents(t, log, "run-s4")) != 2 {
		t.Fatal("both judgements were not recorded")
	}
}

// TestAnEmptyAccountIsRefusedWithoutAskingAnyone: a run that said nothing cannot have said
// what it did, and paying for a model call to learn that is waste.
func TestAnEmptyAccountIsRefusedWithoutAskingAnyone(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"anything","addressed":true}`))
	j := NewModelSteerJudge(m, log)

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s5"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "  \n ")
	if err != nil {
		t.Fatalf("Acknowledged: %v", err)
	}
	if len(acks) != 0 {
		t.Fatalf("an empty account discharged a redirect: %+v", acks)
	}
	if len(m.Requests()) != 0 {
		t.Fatalf("the model was asked about an account with nothing in it (%d calls)", len(m.Requests()))
	}
	if len(judgedEvents(t, log, "run-s5")) != 1 {
		t.Fatal("the refusal was not recorded")
	}
}

// TestAJudgeThatCannotBeReadIsNotAVerdict: a reply that does not parse means the judge is
// broken, not that the run ignored its operator. Recording the second would put a broken
// judge's failure onto the run's record as a fact about the run.
func TestAJudgeThatCannotBeReadIsNotAVerdict(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(llmtest.SayText("I think it was probably fine"))
	j := NewModelSteerJudge(m, log)

	_, err := j.Acknowledged(context.Background(), goalRes("run-s6"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "wrote the trail")
	if err == nil {
		t.Fatal("an unreadable reply was taken as a verdict")
	}
	if fault.Classify(err) != fault.Terminal {
		t.Fatalf("classified %q, want Terminal: retrying a judge that cannot be parsed buys nothing", fault.Classify(err))
	}
	if len(judgedEvents(t, log, "run-s6")) != 0 {
		t.Fatal("a judgement nobody could read was recorded as one")
	}
}

// TestAJudgeWithNothingWiredRefusesRatherThanPasses: neither a missing model nor a missing
// record is a reason to let a redirect go unanswered.
func TestAJudgeWithNothingWiredRefusesRatherThanPasses(t *testing.T) {
	steers := []goal.Steer{redirect("steer-1", "write to events instead")}

	nomodel := NewModelSteerJudge(nil, spine.NewMemoryLog())
	if _, err := nomodel.Acknowledged(context.Background(), goalRes("run-s7"), steerSpec(), goal.Status{}, steers, "wrote the trail"); err == nil {
		t.Fatal("a judge with no model discharged a redirect")
	}

	nolog := NewModelSteerJudge(llmtest.NewScripted(llmtest.SayText(`{"quote":"moved it","addressed":true}`)), nil)
	if _, err := nolog.Acknowledged(context.Background(), goalRes("run-s7"), steerSpec(), goal.Status{}, steers, "wrote the trail"); err == nil {
		t.Fatal("a judgement nobody could record was accepted")
	}
}

// TestACancelledJudgementIsTheCancellation: shutting the run down is not a finding that the
// run ignored its operator, and it must not be recorded as one.
func TestACancelledJudgementIsTheCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log := spine.NewMemoryLog()
	j := NewModelSteerJudge(&failingModel{err: errors.New("request cancelled")}, log)

	_, err := j.Acknowledged(ctx, goalRes("run-s9"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "wrote the trail")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want the cancellation", err)
	}
	if len(judgedEvents(t, log, "run-s9")) != 0 {
		t.Fatal("a shutdown was recorded as a verdict on the run")
	}
}

// TestAVerdictThatCannotBeRecordedFailsTheJudgement: a judgement nobody can show happened is
// what the record exists to prevent, so the write failing fails the judgement rather than
// discharging the redirect on the strength of an event that was never written.
func TestAVerdictThatCannotBeRecordedFailsTheJudgement(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"moved the writer onto the events table","addressed":true}`))
	j := NewModelSteerJudge(m, failingLog{spine.NewMemoryLog()})

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s10"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "moved the writer onto the events table")
	if err == nil {
		t.Fatalf("a judgement nobody could record discharged the redirect: %+v", acks)
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("classified %q, want Transient so the write is tried again", got)
	}
}

// TestAnEmptyAccountThatCannotBeRecordedFails: the same rule on the path that refuses without
// asking the model. Skipping the call is an economy, not a licence to skip the record.
func TestAnEmptyAccountThatCannotBeRecordedFails(t *testing.T) {
	j := NewModelSteerJudge(llmtest.NewScripted(), failingLog{spine.NewMemoryLog()})

	if _, err := j.Acknowledged(context.Background(), goalRes("run-s11"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, ""); err == nil {
		t.Fatal("a refusal nobody could record was accepted")
	}
}

// TestAReplyThatIsJSONAndNotAVerdictIsNotAVerdict: an object that does not parse into the
// verdict shape is the judge being broken, the same as no object at all.
func TestAReplyThatIsJSONAndNotAVerdictIsNotAVerdict(t *testing.T) {
	log := spine.NewMemoryLog()
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":["moved it"],"addressed":"yes"}`))
	j := NewModelSteerJudge(m, log)

	_, err := j.Acknowledged(context.Background(), goalRes("run-s12"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "wrote the trail")
	if err == nil {
		t.Fatal("a reply of the wrong shape was taken as a verdict")
	}
	if got := fault.Classify(err); got != fault.Terminal {
		t.Fatalf("classified %q, want Terminal: asking a broken judge again buys nothing", got)
	}
	if len(judgedEvents(t, log, "run-s12")) != 0 {
		t.Fatal("a judgement nobody could read was recorded as one")
	}
}

// TestNothingOutstandingAsksNobody: a run with no redirect against it has nothing to
// discharge, and reaching the judge at all would be a call to be told so.
func TestNothingOutstandingAsksNobody(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"anything","addressed":true}`))
	j := NewModelSteerJudge(m, spine.NewMemoryLog())

	acks, err := j.Acknowledged(context.Background(), goalRes("run-s13"), steerSpec(), goal.Status{}, nil, "wrote the trail")
	if err != nil || acks != nil {
		t.Fatalf("Acknowledged(no redirects) = %+v, %v, want nothing", acks, err)
	}
	if len(m.Requests()) != 0 {
		t.Fatalf("the model was asked about nothing (%d calls)", len(m.Requests()))
	}
}

// TestTheJudgeCanBeReframedAndCapped: both options exist so a caller can put the judgement on
// a cheaper tier, and both refuse the value that would silently disable them.
func TestTheJudgeCanBeReframedAndCapped(t *testing.T) {
	m := llmtest.NewScripted(llmtest.SayText(`{"quote":"moved it","addressed":true}`))
	j := NewModelSteerJudge(m, spine.NewMemoryLog(),
		WithSteerSystem("rule on the redirect"), WithSteerMaxTokens(64))

	if _, err := j.Acknowledged(context.Background(), goalRes("run-s14"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "moved it"); err != nil {
		t.Fatalf("Acknowledged: %v", err)
	}
	req := m.Requests()[0]
	if req.System != "rule on the redirect" || req.MaxTokens != 64 {
		t.Fatalf("the overrides did not reach the model: system=%q maxTokens=%d", req.System, req.MaxTokens)
	}

	// An empty framing or a non-positive cap is a caller mistake, and taking it would ship a
	// judge with no standing instruction or no room to answer.
	def := NewModelSteerJudge(llmtest.NewScripted(llmtest.SayText(`{"quote":"moved it","addressed":true}`)),
		spine.NewMemoryLog(), WithSteerSystem("  "), WithSteerMaxTokens(0))
	if def.system != defaultSteerSystem || def.maxTokens != 512 {
		t.Fatalf("an empty override replaced the default: system=%q maxTokens=%d", def.system, def.maxTokens)
	}
}

// TestAModelThatCouldNotBeAskedIsClassifiedByTheCaller: a transient failure retries, so a
// blip does not stop a run that may well have complied.
func TestAModelThatCouldNotBeAskedIsClassifiedByTheCaller(t *testing.T) {
	m := &failingModel{err: fault.New(fault.Transient, "model_unreachable", "the cheap tier is unreachable")}
	j := NewModelSteerJudge(m, spine.NewMemoryLog())

	_, err := j.Acknowledged(context.Background(), goalRes("run-s8"), steerSpec(), goal.Status{},
		[]goal.Steer{redirect("steer-1", "write to events instead")}, "wrote the trail")
	if err == nil {
		t.Fatal("a model that could not be asked was taken as an acknowledgement")
	}
	if got := fault.Classify(err); got != fault.Transient {
		t.Fatalf("classified %q, want Transient so it retries", got)
	}
}
