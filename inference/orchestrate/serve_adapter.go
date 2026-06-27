package orchestrate

import (
	"context"

	"github.com/ionalpha/flynn/inference/serve"
)

// LaunchFunc starts a model by id, resolving and serving it. It is injected because launching
// a catalog model (resolve, provision, build a plan, serve) is wired above this package; the
// orchestrator only needs the verb.
type LaunchFunc func(ctx context.Context, modelID string) error

// FootprintFunc returns the device memory a model occupies when resident, in bytes. It is
// injected from the model catalog, the source of a model's known size.
type FootprintFunc func(modelID string) int64

// ServeAdapter implements Server over a serve.Manager: it observes the resident set from the
// manager's records and live load stats, launches through an injected launcher, and evicts by
// stopping the manager's server. A model is reported active when it is currently decoding, so
// the scheduler never evicts in-flight work.
type ServeAdapter struct {
	mgr       *serve.Manager
	launch    LaunchFunc
	footprint FootprintFunc
}

// NewServeAdapter builds an adapter over mgr. launch starts a model by id and footprint
// reports its resident size; both come from the layer that owns the catalog and the serve
// plan. A nil launch makes Launch a no-op, and a nil footprint reads every model as zero-size.
func NewServeAdapter(mgr *serve.Manager, launch LaunchFunc, footprint FootprintFunc) *ServeAdapter {
	return &ServeAdapter{mgr: mgr, launch: launch, footprint: footprint}
}

// Resident lists the currently serving models with the inputs the scheduler needs. The memory
// footprint comes from the injected catalog lookup; whether a model is actively decoding comes
// from its live load stats, so an unreadable runtime is treated as idle (the scheduler may
// evict it) rather than as busy, which is the safe default for freeing memory.
func (a *ServeAdapter) Resident(ctx context.Context) ([]Resident, error) {
	recs, err := a.mgr.Status(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Resident, 0, len(recs))
	for _, r := range recs {
		active := false
		if s, statErr := a.mgr.Stats(ctx, r.ModelID); statErr == nil && s.Known {
			active = s.RequestsRunning > 0
		}
		out = append(out, Resident{
			ModelID:   r.ModelID,
			Footprint: a.footprintOf(r.ModelID),
			Active:    active,
			LastUsed:  r.StartedAt,
		})
	}
	return out, nil
}

func (a *ServeAdapter) footprintOf(id string) int64 {
	if a.footprint == nil {
		return 0
	}
	return a.footprint(id)
}

// Launch starts a model by id through the injected launcher.
func (a *ServeAdapter) Launch(ctx context.Context, modelID string) error {
	if a.launch == nil {
		return nil
	}
	return a.launch(ctx, modelID)
}

// Evict stops the model's server. Stopping a model that is not running is not an error, so a
// redundant evict is a safe no-op.
func (a *ServeAdapter) Evict(_ context.Context, modelID string) error {
	_, err := a.mgr.Stop(modelID)
	return err
}

var _ Server = (*ServeAdapter)(nil)
