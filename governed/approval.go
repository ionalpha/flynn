package governed

import (
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/ionalpha/flynn/allowance"
	"github.com/ionalpha/flynn/approval"
	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/driver"
	"github.com/ionalpha/flynn/ids"
	"github.com/ionalpha/flynn/internal/spinesink"
	"github.com/ionalpha/flynn/mission"
	"github.com/ionalpha/flynn/spine"
)

// approvalKeyID names the per-run approval key. The authority is the live decision at
// the prompt, so a key outliving the run would claim more than that.
const approvalKeyID = "operator"

// approvalStack is the gate, the signer and the host id both bind to. It is installed on
// every run, and an empty policy admits everything, so approval never depends on a caller
// remembering it.
type approvalStack struct {
	gate    *approval.Gate
	signer  approval.Signer
	host    string
	actions []string
	// sink lets the wrapped prompter record on the same stream as the gate's verdicts.
	sink *spinesink.ApprovalSink
}

// newApprovalStack records decisions onto stream. The verifier makes an approval
// single-use, in-date and host-bound. The signer signs exactly what the gate rebuilds.
//
// The keyring trusts only the run's key, so the signature binds the decision to the
// action without proving who made it. Separation of duties supplies approvers out of
// band; this is its one-approver case.
func newApprovalStack(actions []string, log spine.Log, stream string) (*approvalStack, error) {
	policy := ApprovalPolicy(actions)
	host := approvalHost(os.Hostname)
	signer, keyring, err := approvalIdentity(ids.Entropy(nil))
	if err != nil {
		return nil, err
	}
	verifier := approval.NewVerifier(keyring, approval.NewMemStore(), approval.WithHost(host))
	sink := spinesink.NewApproval(log, stream)
	gate := approval.NewGate(policy, verifier,
		approval.WithGateHost(host),
		approval.WithSink(sink))
	return &approvalStack{gate: gate, signer: signer, host: host, actions: GatedActions(policy), sink: sink}, nil
}

// approvalIdentity returns a fresh signer and a keyring trusting exactly its key, built
// together so they cannot disagree. Failing entropy is an error, because a weakly random
// key would claim a binding it lacks.
func approvalIdentity(entropy io.Reader) (approval.Signer, *approval.Keyring, error) {
	keyring := approval.NewKeyring()
	signer, pub, err := approval.GenerateEd25519Signer(approvalKeyID, entropy)
	if err == nil {
		// Checked, because a keyring missing the only key refuses every approval.
		err = keyring.Add(approvalKeyID, pub)
	}
	if err != nil {
		return nil, nil, err
	}
	return signer, keyring, nil
}

// approvalHost binds approvals to this machine. An unnamed host falls back to a
// constant, because the verifier reads "" as "any host".
func approvalHost(hostname func() (string, error)) string {
	name, err := hostname()
	if err != nil || strings.TrimSpace(name) == "" {
		return "localhost"
	}
	return name
}

// ApprovalPolicy requires one signature per named action. Blanks drop and duplicates
// collapse, so a repeated flag is not a quorum. The default is empty, because a default
// pause would deadlock every non-interactive run; the grant and sandbox already bound
// every action.
func ApprovalPolicy(actions []string) approval.Requirements {
	req := approval.Requirements{}
	for _, a := range actions {
		if name := strings.TrimSpace(a); name != "" {
			req[name] = 1
		}
	}
	return req
}

// checkActionNames refuses a policy naming an action outside the grant. Such a name
// gates nothing, so the posture would list a gate no action reaches.
func (c *config) checkActionNames(grant capability.Grant) error {
	for _, p := range []struct {
		option string
		names  []string
	}{
		{"WithApproval", GatedActions(ApprovalPolicy(c.approve))},
		{"WithAllowance", sortedNames(outsidePolicy(c.outside))},
	} {
		var unknown []string
		for _, name := range p.names {
			if !grant.Allows(name) {
				unknown = append(unknown, name)
			}
		}
		if len(unknown) > 0 {
			return fmt.Errorf("governed: %s names %s, which this run's grant does not include (it offers %s)",
				p.option, strings.Join(unknown, ", "), strings.Join(grant.Actions(), ", "))
		}
	}
	return nil
}

func sortedNames(set allowance.Actions) []string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// GatedActions sorts the actions req pauses, for the line a run prints at start.
func GatedActions(req approval.Requirements) []string {
	names := make([]string, 0, len(req))
	for name := range req {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// options installs the gate, and the prompter if there is one. With a nil prompter the
// NeedsApproval refusal reaches the model unchanged, so a run with nobody to ask stays
// refused.
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

// spec is the stack for a Driver-built run. It is nil only when approval is waived.
func (s *approvalStack) spec(prompter mission.ApprovalPrompter) *driver.Approval {
	if s == nil {
		return nil
	}
	return &driver.Approval{Gate: s.gate, Prompter: prompter, Signer: s.signer, Host: s.host}
}
