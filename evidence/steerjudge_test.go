package evidence

import (
	"context"
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
