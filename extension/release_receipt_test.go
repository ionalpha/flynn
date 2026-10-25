package extension

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ionalpha/flynn/fault"
)

// TestWriteReceiptReportsAnUnwritableReceipt checks that a receipt which cannot be
// stored is a terminal failure, not a silent skip: without it the next resolve has
// nothing to prove the install against and would treat a verified binary as unverified.
func TestWriteReceiptReportsAnUnwritableReceipt(t *testing.T) {
	dir := t.TempDir()
	// A directory where the receipt file belongs, so the atomic replace cannot land.
	if err := os.Mkdir(filepath.Join(dir, verifiedFile), 0o700); err != nil {
		t.Fatal(err)
	}

	err := ReleaseResolver{}.writeReceipt(dir, verified{Archive: "a.tar.gz"})

	var fe *fault.Error
	if !errors.As(err, &fe) || fault.CodeOf(err) != "extension_release_receipt" || fault.Classify(err) != fault.Terminal {
		t.Fatalf("err = %v, want a terminal extension_release_receipt fault", err)
	}
}
