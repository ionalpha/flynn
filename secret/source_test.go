package secret_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ionalpha/flynn/secret"
)

func TestEnvSourceResolves(t *testing.T) {
	t.Setenv("FLYNN_TEST_KEY", "sk-from-env")
	got, err := secret.EnvSource{}.Lookup(context.Background(), "FLYNN_TEST_KEY")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got.Expose() != "sk-from-env" {
		t.Fatalf("got %q, want the env value", got.Expose())
	}
}

func TestEnvSourceMissingIsNotFound(t *testing.T) {
	if _, err := (secret.EnvSource{}).Lookup(context.Background(), "FLYNN_TEST_UNSET_KEY"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("unset var: got %v, want ErrNotFound", err)
	}
}

func TestEnvSourceEmptyIsNotFound(t *testing.T) {
	t.Setenv("FLYNN_TEST_EMPTY", "")
	if _, err := (secret.EnvSource{}).Lookup(context.Background(), "FLYNN_TEST_EMPTY"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("empty var: got %v, want ErrNotFound", err)
	}
}
