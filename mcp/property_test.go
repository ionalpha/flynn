package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"

	"github.com/ionalpha/flynn/capability"
	"github.com/ionalpha/flynn/dispatch"
	"github.com/ionalpha/flynn/mission"
)

// TestServerRoutingProperty is the rigor property: for any set of registered tools,
// any grant over them, and any sequence of calls, the server answers every request
// exactly once with a matching id, and each call's outcome follows the one rule that
// authority is decided at the waist: a registered tool the grant permits echoes its
// arguments, a registered tool the grant denies is an error result, and an unknown
// tool is an error result. Presence in the toolset never implies permission.
func TestServerRoutingProperty(t *testing.T) {
	rapid.Check(t, func(rt *rapid.T) {
		n := rapid.IntRange(1, 6).Draw(rt, "tools")
		names := make([]string, n)
		toolset := make([]mission.Tool, n)
		granted := make([]string, 0, n)
		grantedSet := map[string]bool{}
		for i := range n {
			names[i] = fmt.Sprintf("t%d", i)
			toolset[i] = echo(names[i])
			if rapid.Bool().Draw(rt, fmt.Sprintf("grant-%d", i)) {
				granted = append(granted, names[i])
				grantedSet[names[i]] = true
			}
		}

		d := dispatch.New(dispatch.WithAdmitter(capability.Admitter{}))
		srv := NewServer(d, toolset)
		ctx := capability.Into(context.Background(), capability.NewGrant(granted...))

		m := rapid.IntRange(1, 10).Draw(rt, "calls")
		reqs := make([]string, m)
		wantName := make([]string, m)
		wantArgs := make([]string, m)
		for i := range m {
			// index n selects an unknown tool; below n selects a registered one.
			pick := rapid.IntRange(0, n).Draw(rt, fmt.Sprintf("pick-%d", i))
			name := "unknown-tool"
			if pick < n {
				name = names[pick]
			}
			val := rapid.StringN(0, 12, 12).Draw(rt, fmt.Sprintf("arg-%d", i))
			args, _ := json.Marshal(map[string]string{"v": val})
			wantName[i] = name
			wantArgs[i] = string(args)
			reqs[i] = fmt.Sprintf(`{"jsonrpc":"2.0","id":%d,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, i+1, name, string(args))
		}

		resps := drive(ctx, t, srv, reqs...)
		if len(resps) != m {
			rt.Fatalf("want %d responses, got %d", m, len(resps))
		}
		for i, r := range resps {
			var id int
			_ = json.Unmarshal(r.ID, &id)
			if id != i+1 {
				rt.Fatalf("response %d has id %d", i, id)
			}
			var res callResult
			if err := json.Unmarshal(r.Result, &res); err != nil {
				rt.Fatalf("call %d result decode: %v", i, err)
			}
			registered := strings.HasPrefix(wantName[i], "t")
			permitted := registered && grantedSet[wantName[i]]
			switch {
			case permitted:
				if res.IsError {
					rt.Fatalf("permitted call %q was denied: %+v", wantName[i], res)
				}
				if res.Content[0].Text != wantArgs[i] {
					rt.Fatalf("permitted call did not echo args: got %q want %q", res.Content[0].Text, wantArgs[i])
				}
			default:
				if !res.IsError {
					rt.Fatalf("call %q should have been an error result (registered=%v permitted=%v)", wantName[i], registered, permitted)
				}
			}
		}
	})
}
