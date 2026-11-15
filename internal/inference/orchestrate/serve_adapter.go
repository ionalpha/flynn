package orchestrate

import (
	"context"

	"github.com/ionalpha/flynn/internal/inference/serve"
)

// LaunchLevel is how far a launch shrinks a model's footprint. It climbs as recovery
// escalates, so a model that keeps running out of memory is retried smaller.
type LaunchLevel int

const (
	// LaunchFull serves the model at its normal footprint.
	LaunchFull LaunchLevel = iota
	// LaunchDegraded serves with a smaller context window.
	LaunchDegraded
	// LaunchMinimal serves at the smallest viable footprint, including CPU only.
	LaunchMinimal
)

// LaunchFunc starts a model by id at a level. It is injected because resolving,
// provisioning and serving a catalog model is wired above this package.
type LaunchFunc func(ctx context.Context, modelID string, level LaunchLevel) error

// FootprintFunc returns a model's resident device memory in bytes, from the catalog's
// known size.
type FootprintFunc func(modelID string) int64

// ServeAdapter implements [Server] over a serve.Manager. A model is reported active
// while it is decoding, so the scheduler never evicts in-flight work.
type ServeAdapter struct {
	mgr       *serve.Manager
	launch    LaunchFunc
	footprint FootprintFunc
}

// NewServeAdapter builds an adapter over mgr. A nil launch makes Launch a no-op, and a
// nil footprint reads every model as zero-size.
func NewServeAdapter(mgr *serve.Manager, launch LaunchFunc, footprint FootprintFunc) *ServeAdapter {
	return &ServeAdapter{mgr: mgr, launch: launch, footprint: footprint}
}

// Resident lists the serving models with the inputs the scheduler needs. A runtime whose
// load stats cannot be read counts as idle, so memory can still be freed.
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

// Launch starts a model through the injected launcher at the given level.
func (a *ServeAdapter) Launch(ctx context.Context, modelID string, level LaunchLevel) error {
	if a.launch == nil {
		return nil
	}
	return a.launch(ctx, modelID, level)
}

// Evict stops the model's server. Stopping a model that is not running is a no-op.
func (a *ServeAdapter) Evict(_ context.Context, modelID string) error {
	_, err := a.mgr.Stop(modelID)
	return err
}

var _ Server = (*ServeAdapter)(nil)
