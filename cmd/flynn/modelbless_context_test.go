package main

import (
	"math"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/internal/huggingface"
)

func TestClampContext(t *testing.T) {
	for _, tc := range []struct {
		in   uint64
		want int
	}{
		{0, 0},
		{131072, 131072},
		{math.MaxInt32, math.MaxInt32},
		{math.MaxInt32 + 1, math.MaxInt32},
		{math.MaxUint64, math.MaxInt32},
	} {
		if got := clampContext(tc.in); got != tc.want {
			t.Errorf("clampContext(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestResolveLicense(t *testing.T) {
	req := blessRequest{owner: "o", repo: "r", info: huggingface.Info{License: "apache-2.0"}}
	if got, err := resolveLicense(req); err != nil || got != "apache-2.0" {
		t.Fatalf("model card license: got %q, %v", got, err)
	}
	req.licenseOverride = "mit"
	if got, err := resolveLicense(req); err != nil || got != "mit" {
		t.Fatalf("override should win: got %q, %v", got, err)
	}
	req.info.License, req.licenseOverride = "", ""
	if _, err := resolveLicense(req); err == nil || !strings.Contains(err.Error(), "o/r declares no license") {
		t.Fatalf("a model with no license must be refused, got %v", err)
	}
}
