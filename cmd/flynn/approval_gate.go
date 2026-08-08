package main

import (
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// approvalKeyID names the key the run mints its own approvals under. A standalone
// install has one approver, the operator sitting at the terminal, and the key that
// stands for them is generated per run: the authority being represented is the live
// decision at the prompt, not a durable credential, so a key that outlives the run would
// claim more than the arrangement actually establishes.
const approvalKeyID = "operator"

// approvalStack is the shipped human-approval path: the gate that pauses a privileged
// action, the signer that mints the approval the operator's decision authorizes, and the
// host id both are bound to. A nil stack means no action on this run requires approval,
// which is the default.
type approvalStack struct {
	gate   *approval.Gate
	signer approval.Signer
	host   string
}

// newApprovalStack builds the approval path for a run whose policy lists actions, or
// returns nil when it lists none. Decisions are recorded onto stream on log.
//
// The pieces fit together the way the packages intend and are worth naming, because each
// one is what makes the next mean anything. The policy decides which actions pause. The
// verifier checks a presented approval against a keyring, a nonce store and a clock, so
// an approval is single-use, in-date and for this host. The signer mints one when the
// operator allows, over exactly the action, scope, principal and target the gate will
// rebuild, so the signature covers what is being authorized rather than a summary of it.
// The sink writes the decision to the run's own stream, so the record of who allowed what
// is sealed with everything else the run did.
//
// The keyring holds exactly one public key: the run's own. That is the honest shape of a
// standalone install, where the approver and the operator are the same person and the
// signature's job is binding the decision to the action rather than proving who made it.
// A deployment with real separation of duties supplies approvers out of band, which is
// what the keyring is for, and this is the n=1 case of it rather than a different design.
func newApprovalStack(actions []string, log spine.Log, stream string) (*approvalStack, error) {
	policy := approvalPolicy(actions)
	if len(policy) == 0 {
		return nil, nil
	}
	host := approvalHost(os.Hostname)
	signer, keyring, err := approvalIdentity(ids.Entropy(nil))
	if err != nil {
		return nil, err
	}
	verifier := approval.NewVerifier(keyring, approval.NewMemStore(), approval.WithHost(host))
	gate := approval.NewGate(policy, verifier,
		approval.WithGateHost(host),
		approval.WithSink(spinesink.NewApproval(log, stream)))
	return &approvalStack{gate: gate, signer: signer, host: host}, nil
}

// approvalIdentity mints the run's approver: a fresh keypair from entropy, and a keyring
// trusting exactly its public half. The signer and the keyring are built together and
// returned together because they are two views of one identity, and a keyring that trusted
// a key the signer does not hold would refuse every approval the run minted.
//
// A failing entropy source is a hard error rather than a fallback. There is no weaker key
// worth minting: an approval is a signature over what was authorized, and one signed with
// degraded randomness would claim a binding it does not have.
func approvalIdentity(entropy io.Reader) (approval.Signer, *approval.Keyring, error) {
	keyring := approval.NewKeyring()
	signer, pub, err := approval.GenerateEd25519Signer(approvalKeyID, entropy)
	if err == nil {
		// A key that was just generated is always the right size, so this does not fail in
		// practice. It is checked rather than discarded because a keyring that quietly
		// dropped the run's only approver would refuse every approval the run minted, and
		// the run would read as one where the operator's decisions never took effect.
		err = keyring.Add(approvalKeyID, pub)
	}
	if err != nil {
		return nil, nil, err
	}
	return signer, keyring, nil
}

// approvalHost is the host id the gate stamps on the envelope it requires and the signer
// binds a minted approval to, so an approval for one machine cannot authorize an action on
// another. A host that will not name itself falls back to a constant rather than to the
// empty string, which the verifier reads as "valid on any host": a machine with a broken
// hostname would otherwise widen every approval it minted. Within one run the gate and the
// signer see the same value either way, which is all the binding needs.
func approvalHost(hostname func() (string, error)) string {
	name, err := hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "localhost"
	}
	return name
}

// approvalPolicy turns the operator's list of action names into the gate's policy, one
// signature per listed action. Blank entries are dropped and duplicates collapse, so
// `--require-approval shell --require-approval shell` is one requirement rather than a
// quorum of two: raising the quorum is a real thing to want and repeating a flag is not
// how anyone means to ask for it.
//
// An empty result is the default and means no action requires a person. The standing
// controls on a run are its capability grant and its sandbox, which apply to every action
// on every path including the ones no operator is watching; approval is the second gate
// above them, for the actions a particular operator wants to see before they happen. A
// default that paused something would also be a default that deadlocks every
// non-interactive run, which has no one to ask.
func approvalPolicy(actions []string) approval.Requirements {
	req := approval.Requirements{}
	for _, a := range actions {
		if name := strings.TrimSpace(a); name != "" {
			req[name] = 1
		}
	}
	return req
}

// options returns the mission options that install the stack on an executor: the gate
// that pauses a listed action, and the prompter that resolves the pause when the entry
// point has one to offer.
//
// A nil prompter is the non-interactive answer and it is a decision, not an omission.
// With no one to ask, the waist's NeedsApproval rejection surfaces to the model
// unchanged and the action stays refused: a run that cannot reach a person does not get
// to proceed as though it had. The model sees the refusal and can adapt or stop, which is
// the same shape every other governance refusal takes.
func (s *approvalStack) options(prompter mission.ApprovalPrompter) []mission.Option {
	if s == nil {
		return nil
	}
	opts := []mission.Option{mission.WithApproval(s.gate)}
	if prompter != nil {
		opts = append(opts, mission.WithApprovalPrompter(prompter, s.signer, s.host))
	}
	return opts
}

// spec returns the loop-agnostic form of the stack, for a run assembled through a Driver
// rather than by calling mission.NewExecutor directly. A nil stack yields nil, so the
// Router's base spec carries no approval and every loop it builds is ungated, which is
// the default.
func (s *approvalStack) spec(prompter mission.ApprovalPrompter) *driver.Approval {
	if s == nil {
		return nil
	}
	return &driver.Approval{Gate: s.gate, Prompter: prompter, Signer: s.signer, Host: s.host}
}

// gateSetup is what an entry point asks for from the two gates that sit above the run's
// capability grant: the actions that need a person to authorize them and the prompter that
// asks them, and the actions whose effects leave the workspace and cannot be undone. It
// travels as one value so an assembly signature grows by one parameter rather than by
// several that must always agree.
//
// A zero value is a run with neither gate, which is the default. The approval fields are
// meaningfully independent of each other: actions with no prompter is the non-interactive
// run that refuses rather than proceeds, and a prompter with no actions is an interactive
// session where nothing has been asked to pause.
//
// The two gates ask different questions and are deliberately not folded together. Approval
// asks a person now, which needs someone reachable; an allowance was written down before
// the run started, for a run nobody will be watching, and is answered by editing the goal
// rather than by replying to a prompt.
type gateSetup struct {
	approve  []string
	prompter mission.ApprovalPrompter
	// outside names the actions that reach outside the workspace irreversibly. One of them
	// runs only if the goal declares it, and a run that reaches an undeclared one is paused
	// with the ask rather than handed a refusal it would look for a way around.
	outside []string
}

// stringList is a flag.Value that accumulates every occurrence of a repeatable flag,
// so `--require-approval shell --require-approval net.fetch` names two actions rather
// than the second overwriting the first.
type stringList struct{ values []string }

// String renders the accumulated values for the flag package's usage output.
func (s *stringList) String() string {
	if s == nil {
		return ""
	}
	return strings.Join(s.values, ",")
}

// Set appends one occurrence. A blank value is kept out rather than turned into an
// action named the empty string, which would list a requirement nothing can satisfy.
func (s *stringList) Set(v string) error {
	if v = strings.TrimSpace(v); v != "" {
		s.values = append(s.values, v)
	}
	return nil
}

// gatedActions is the sorted list of actions a policy pauses, for the line a run prints
// when it starts under approval. A run whose behaviour changed should say so before it
// surprises someone by stopping halfway.
func gatedActions(req approval.Requirements) []string {
	names := make([]string, 0, len(req))
	for name := range req {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}
