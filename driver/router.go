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

// ModelResolver turns a model identifier into a client. It is consulted only for a goal
// that names a model other than the default.
type ModelResolver func(id string) (llm.Model, error)

// Router drives each goal through the loop and model its spec selects, caching one loop
// per (driver, model) pair. As both goal.StepExecutor and goal.StopEvaluator, one Router
// runs a parent on the default loop and a child on a specialist's, each judged by its
// own convergence test. The per-goal prompt and grant are applied inside the loop, so
// they do not key the cache.
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

// RouterConfig configures a Router. Base holds the ingredients every loop shares; its
// Model is ignored because the Router sets the model per goal.
type RouterConfig struct {
	Registry      *Registry
	Base          Spec
	DefaultModel  llm.Model
	DefaultDriver string
	ResolveModel  ModelResolver
}

// NewRouter builds a Router. A nil registry uses [Default].
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

// Met judges the goal by the convergence test of the loop its spec selects.
func (r *Router) Met(ctx context.Context, spec goal.Spec, status goal.Status) (bool, string, error) {
	loop, err := r.loopFor(spec)
	if err != nil {
		return false, "", err
	}
	return loop.stop.Met(ctx, spec, status)
}

// loopFor returns the cached loop for the goal's (driver, model) pair, building it on
// first use.
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
