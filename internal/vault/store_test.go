package vault

// The sealed file on disk: deleting from it, and every way writing or reading it can
// fail. Delete clears both backends, because a secret left in one of them is a secret
// the user believes is gone. The failure paths all report rather than degrade: a
// malformed file is an error, not an empty vault a save would then overwrite.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ionalpha/flynn/secret"
)

func TestStoreDeleteSealedFile(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	// Deleting before any vault file exists is a no-op, not an error, and must not
	// create a file (which would otherwise demand a passphrase for a vault that
	// holds nothing).
	if err := s.Delete(ctx, "ABSENT"); err != nil {
		t.Fatalf("delete with no vault file: %v", err)
	}
	if s.fileExists() {
		t.Fatal("deleting from a nonexistent vault created one")
	}

	if err := s.Set(ctx, "A", secret.New("va")); err != nil {
		t.Fatal(err)
	}
	if err := s.Set(ctx, "B", secret.New("vb")); err != nil {
		t.Fatal(err)
	}

	// Deleting a key the vault does not hold leaves the file untouched.
	before, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, "NOPE"); err != nil {
		t.Fatalf("delete of an absent key: %v", err)
	}
	after, err := os.ReadFile(s.file)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("deleting an absent key rewrote the sealed file")
	}

	// Deleting a held key removes it and leaves its neighbours intact.
	if err := s.Delete(ctx, "A"); err != nil {
		t.Fatalf("delete of a held key: %v", err)
	}
	if _, err := s.Lookup(ctx, "A"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("after delete: got %v, want ErrNotFound", err)
	}
	got, err := s.Lookup(ctx, "B")
	if err != nil || got.Expose() != "vb" {
		t.Fatalf("neighbour after delete: got %q err %v, want %q", got.Expose(), err, "vb")
	}
}

// TestStoreDeleteClearsBothBackends is the anti-resurrection rule: a credential
// removed from the keychain must not survive in a stale sealed-file copy and come
// back on the next lookup from a host where the keychain is unavailable.
func TestStoreDeleteClearsBothBackends(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	pass := WithPassphrase(fixedPass("pw"))

	// Seed the sealed file (keychain down), then delete with the keychain up.
	if err := New(dir, WithKeyring(downKeyring{}), pass).Set(ctx, "K", secret.New("stale")); err != nil {
		t.Fatal(err)
	}
	kr := fakeKeyring{"K": "fresh"}
	if err := New(dir, WithKeyring(kr), pass).Delete(ctx, "K"); err != nil {
		t.Fatal(err)
	}
	if _, ok := kr["K"]; ok {
		t.Fatal("Delete left the credential in the keychain")
	}

	// The sealed-file copy must be gone too, so a later run on a keychain-less host
	// does not resurrect it.
	if _, err := New(dir, WithKeyring(downKeyring{}), pass).Lookup(ctx, "K"); !errors.Is(err, secret.ErrNotFound) {
		t.Fatalf("the sealed file resurrected a deleted credential: got %v, want ErrNotFound", err)
	}
}

// TestSealedFileErrorsPropagate pins that an unreadable or unopenable vault is
// reported on every path that touches it, never quietly treated as an empty vault.
// Swallowing the error would let a Set overwrite a vault it could not read, dropping
// every credential already in it.
func TestSealedFileErrorsPropagate(t *testing.T) {
	ctx := context.Background()

	ops := map[string]func(*Store) error{
		"Lookup": func(s *Store) error { _, err := s.Lookup(ctx, "K"); return err },
		"Set":    func(s *Store) error { return s.Set(ctx, "K", secret.New("v")) },
		"Delete": func(s *Store) error { return s.Delete(ctx, "K") },
	}

	t.Run("wrong passphrase", func(t *testing.T) {
		dir := t.TempDir()
		if err := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("right"))).
			Set(ctx, "K", secret.New("v")); err != nil {
			t.Fatal(err)
		}
		for name, op := range ops {
			t.Run(name, func(t *testing.T) {
				s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("wrong")))
				if err := op(s); !errors.Is(err, ErrBadPassphrase) {
					t.Fatalf("got %v, want ErrBadPassphrase", err)
				}
			})
		}
	})

	t.Run("no passphrase supplier", func(t *testing.T) {
		dir := t.TempDir()
		// Seal a real vault so the file exists and must be opened.
		if err := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw"))).
			Set(ctx, "K", secret.New("v")); err != nil {
			t.Fatal(err)
		}
		for name, op := range ops {
			t.Run(name, func(t *testing.T) {
				s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(nil))
				if err := op(s); !errors.Is(err, ErrNoPassphrase) {
					t.Fatalf("got %v, want ErrNoPassphrase", err)
				}
			})
		}
	})

	t.Run("empty passphrase", func(t *testing.T) {
		dir := t.TempDir()
		s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("")))
		if err := s.Set(ctx, "K", secret.New("v")); !errors.Is(err, ErrNoPassphrase) {
			t.Fatalf("an empty passphrase must not seal a vault: got %v, want ErrNoPassphrase", err)
		}
		if s.fileExists() {
			t.Fatal("a rejected passphrase still wrote a vault file")
		}
	})

	t.Run("passphrase prompt fails", func(t *testing.T) {
		boom := errors.New("terminal closed")
		s := New(t.TempDir(), WithKeyring(downKeyring{}), WithPassphrase(func(bool) (secret.Text, error) {
			return secret.Text{}, boom
		}))
		if err := s.Set(ctx, "K", secret.New("v")); !errors.Is(err, boom) {
			t.Fatalf("got %v, want the prompt error", err)
		}
	})

	t.Run("unreadable vault path", func(t *testing.T) {
		// A directory where the vault file should be: os.ReadFile fails with
		// something other than ErrNotExist, which must surface rather than read as
		// "no vault yet".
		dir := t.TempDir()
		s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))
		if err := os.MkdirAll(s.file, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := s.Lookup(ctx, "K")
		if err == nil {
			t.Fatal("a vault path that is a directory should not read as an empty vault")
		}
		if errors.Is(err, secret.ErrNotFound) {
			t.Fatalf("an unreadable vault was reported as an absent credential: %v", err)
		}
		if !strings.Contains(err.Error(), "read sealed file") {
			t.Fatalf("got %v, want a read-sealed-file error", err)
		}
	})
}

// TestNewVaultFlagIsSetOnlyForACreate proves the Passphrase callback learns whether
// it is creating a vault (so an interactive prompt can ask for confirmation) or
// opening an existing one (where a second prompt would be wrong).
func TestNewVaultFlagIsSetOnlyForACreate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	var seen []bool
	pass := WithPassphrase(func(newVault bool) (secret.Text, error) {
		seen = append(seen, newVault)
		return secret.New("pw"), nil
	})

	s := New(dir, WithKeyring(downKeyring{}), pass)
	if err := s.Set(ctx, "A", secret.New("va")); err != nil { // creates the vault
		t.Fatal(err)
	}
	if err := s.Set(ctx, "B", secret.New("vb")); err != nil { // opens, then rewrites
		t.Fatal(err)
	}
	if len(seen) == 0 || !seen[0] {
		t.Fatalf("the first seal should report a new vault, saw %v", seen)
	}
	for _, newVault := range seen[1:] {
		if newVault {
			t.Fatalf("an existing vault was reported as new, saw %v", seen)
		}
	}
}

// TestLoadFileRejectsMalformedContents covers the last decode step: a blob that
// authenticates (right passphrase, untampered) but does not hold the credential map
// must be reported, not returned as a nil map that reads as an empty vault.
func TestLoadFileRejectsMalformedContents(t *testing.T) {
	dir := t.TempDir()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	blob, err := seal([]byte("this is not the credential map"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.file, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = s.Lookup(context.Background(), "K")
	if err == nil || !strings.Contains(err.Error(), "malformed vault contents") {
		t.Fatalf("got %v, want a malformed-contents error", err)
	}
}

// TestSetIntoANullVault covers the nil-map guard in Set. A sealed blob whose
// payload is the JSON literal "null" decodes to a nil map with no error, so without
// the guard the very next assignment would panic. It is the one way a valid,
// correctly-sealed vault can still hand back a nil map.
func TestSetIntoANullVault(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := New(dir, WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	blob, err := seal([]byte("null"), []byte("pw"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.file, blob, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Set(ctx, "K", secret.New("v")); err != nil {
		t.Fatalf("setting into a null vault: %v", err)
	}
	got, err := s.Lookup(ctx, "K")
	if err != nil || got.Expose() != "v" {
		t.Fatalf("lookup after seeding a null vault: got %q err %v, want %q", got.Expose(), err, "v")
	}
}

// TestSaveFileCannotCreateItsDirectory pins that a vault whose parent directory
// cannot exist reports the failure instead of silently dropping the credential.
func TestSaveFileCannotCreateItsDirectory(t *testing.T) {
	root := t.TempDir()
	blocker := filepath.Join(root, "blocked")
	if err := os.WriteFile(blocker, []byte("i am a file, not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The vault would have to live inside a path component that is a regular file,
	// so MkdirAll cannot succeed.
	s := New(filepath.Join(blocker, "sub"), WithKeyring(downKeyring{}), WithPassphrase(fixedPass("pw")))

	if err := s.Set(context.Background(), "K", secret.New("v")); err == nil {
		t.Fatal("sealing into an uncreatable directory should fail")
	}
}
