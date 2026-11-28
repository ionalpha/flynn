package goal

import (
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/ionalpha/flynn/fault"
	"github.com/ionalpha/flynn/resource"
)

// Confinement is the directory a goal's steps run in and the most they may do. A runtime
// records its own on each goal it submits and runs a step only if it still [Confinement.Covers]
// what the goal recorded, so another assembly cannot run the goal somewhere else or with more
// authority.
type Confinement struct {
	// Workdir is the absolute working directory, empty when the host supplied the tools or
	// sandbox without naming one.
	Workdir string `json:"workdir,omitempty"`
	// Grant is the actions admitted, ignored when Unrestricted.
	Grant        []string `json:"grant,omitempty"`
	Unrestricted bool     `json:"unrestricted,omitempty"`
}

// Covers reports whether an executor confined to c may run a goal that recorded rec. The
// directories must be the same one on disk and c must admit nothing rec did not. Two nils
// match; one nil, or a directory that cannot be read, is refused.
func (c *Confinement) Covers(rec *Confinement) error {
	switch {
	case c == nil && rec == nil:
		return nil
	case rec == nil:
		return errors.New("the goal records no working directory or grant (it predates the record, or was not submitted by a confined run)")
	case c == nil:
		return errors.New("the goal records a working directory and grant, and this executor declares neither")
	}
	if err := sameDir(c.Workdir, rec.Workdir); err != nil {
		return err
	}
	if rec.Unrestricted {
		return nil
	}
	if c.Unrestricted {
		return errors.New("this run is unrestricted and the goal's grant is not")
	}
	for _, a := range c.Grant {
		if !slices.Contains(rec.Grant, a) {
			return fmt.Errorf("this run grants %q, which the goal's grant does not", a)
		}
	}
	return nil
}

func sameDir(have, rec string) error {
	if have == "" || rec == "" {
		if have == rec {
			return nil
		}
		return fmt.Errorf("the goal records working directory %q and this run has %q", rec, have)
	}
	want, err := os.Stat(rec)
	if err != nil {
		return fmt.Errorf("the goal's working directory cannot be resolved: %w", err)
	}
	got, err := os.Stat(have)
	if err != nil {
		return fmt.Errorf("this run's working directory cannot be resolved: %w", err)
	}
	if !os.SameFile(want, got) {
		return fmt.Errorf("the goal records working directory %q and this run has %q", rec, have)
	}
	return nil
}

// CheckConfinement refuses goal r, terminally, unless have covers what r recorded.
func CheckConfinement(have *Confinement, r resource.Resource) error {
	spec, err := DecodeSpec(r)
	if err != nil {
		return fault.Wrap(fault.Terminal, "goal_spec_decode", err)
	}
	if err := have.Covers(spec.Confinement); err != nil {
		return fault.Wrap(fault.Terminal, "goal_confinement", fmt.Errorf("goal %s may not run here: %w", r.Name, err))
	}
	return nil
}
