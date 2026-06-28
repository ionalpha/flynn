package driver

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/goal"
	"github.com/ionalpha/flynn/llm"
	"github.com/ionalpha/flynn/resource"
)

// ModelResolver turns a model identifier into a model client. The agent wires the
// provider resolver; a test injects a fake. It is consulted only for a goal that
// names a model other than the default.
type ModelResolver func(id string) (llm.Model, error)

// Router drives each goal through the loop and model its spec selects, building and
// caching one loop per (driver, model) pair. It implements both goal.StepExecutor
// and goal.StopEvaluator, so a single Router handed to the runtime drives goals of
// differing loops with the right convergence test for each: a parent on the default
// loop and a delegated child on a specialist's loop and model. A goal that names no
// driver or model uses the host defaults, so a single-loop run is unchanged.
//
// The per-goal system prompt and capability grant are applied inside the loop
// (from the goal), so they vary within one built loop; only the driver and model,
// which determine which loop object to use, key the cache.
type Router struct {
	registry      *Registry
	base          Spec // ingredients shared by every loop; Model is set per goal
	defaultModel  llm.Model
	defaultDriver string
	resolveModel  ModelResolver

	mu    sync.Mutex
	built map[routeKey]builtLoop
}

type routeKey struct{ driver, model string }

type builtLoop struct {
	exec goal.StepExecutor
	stop goal.StopEvaluator
}

// RouterConfig configures a Router. Base carries the ingredients every loop shares
// (tools, default system, default grant, sandbox, reporter, brake, fan-out); its
// Model is ignored, the Router sets the model per goal.
type RouterConfig struct {
	Registry      *Registry
	Base          Spec
	DefaultModel  llm.Model
	DefaultDriver string
	ResolveModel  ModelResolver
}

// NewRouter builds a Router. A nil registry uses the built-in Default registry.
func NewRouter(cfg RouterConfig) *Router {
	r := &Router{
		registry:      cfg.Registry,
		base:          cfg.Base,
		defaultModel:  cfg.DefaultModel,
		defaultDriver: cfg.DefaultDriver,
		resolveModel:  cfg.ResolveModel,
		built:         map[routeKey]builtLoop{},
	}
	if r.registry == nil {
		r.registry = Default()
	}
	return r
}

// Execute drives one step of the goal through the loop its spec selects.
func (r *Router) Execute(ctx context.Context, g resource.Resource) (json.RawMessage, error) {
	spec, err := goal.DecodeSpec(g)
	if err != nil {
		return nil, fault.Wrap(fault.Terminal, "router_spec_decode", err)
	}
	loop, err := r.loopFor(spec)
	if err != nil {
		return nil, err
	}
	return loop.exec.Execute(ctx, g)
}

// Met converges the goal through the loop its spec selects, so a goal driven by an
// alternate loop is judged by that loop's convergence test.
func (r *Router) Met(ctx context.Context, spec goal.Spec, status goal.Status) (bool, string, error) {
	loop, err := r.loopFor(spec)
	if err != nil {
		return false, "", err
	}
	return loop.stop.Met(ctx, spec, status)
}

// loopFor resolves the goal's driver and model, building and caching the loop so a
// goal stepped many times reuses one loop object. The (driver, model) pair keys the
// cache; the per-goal prompt and grant vary inside the loop.
func (r *Router) loopFor(spec goal.Spec) (builtLoop, error) {
	key := routeKey{driver: spec.Driver, model: spec.Model}
	r.mu.Lock()
	defer r.mu.Unlock()
	if loop, ok := r.built[key]; ok {
		return loop, nil
	}

	driverName := spec.Driver
	if driverName == "" {
		driverName = r.defaultDriver // empty here too -> registry's default in Resolve
	}
	drv, err := r.registry.Resolve(driverName)
	if err != nil {
		return builtLoop{}, err
	}
	model := r.defaultModel
	if spec.Model != "" {
		if r.resolveModel == nil {
			return builtLoop{}, fault.New(fault.Terminal, "router_no_model_resolver",
				"router: goal names model "+spec.Model+" but no model resolver is configured")
		}
		m, merr := r.resolveModel(spec.Model)
		if merr != nil {
			return builtLoop{}, fault.Wrap(fault.Terminal, "router_model_resolve", merr)
		}
		model = m
	}

	s := r.base
	s.Model = model
	exec, stop, err := drv.Build(s)
	if err != nil {
		return builtLoop{}, err
	}
	loop := builtLoop{exec: exec, stop: stop}
	r.built[key] = loop
	return loop, nil
}

var (
	_ goal.StepExecutor  = (*Router)(nil)
	_ goal.StopEvaluator = (*Router)(nil)
)
