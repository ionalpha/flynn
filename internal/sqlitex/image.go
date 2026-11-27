package sqlitex

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"

	"github.com/ionalpha/flynn/internal/migrate"
)

// images holds, per migration set ([migrate.Digest]), the bytes of an empty database
// with that set applied.
//
// Replaying the migrations is most of the cost of opening a new store: each ALTER and
// DROP makes SQLite re-parse the whole schema, which takes hundreds of milliseconds
// under the race detector. A process that creates many stores (a test binary creates
// one per test) pays that once. [migrate.Run] still runs over every copy, so the
// checksums are verified as on any open and there is nothing left to apply. A copy
// keeps the applied_at times of the open that built the image.
//
// An image is only ever taken from a private ":memory:" database straight after its
// migrations ran, never from a file, which another process could have written rows
// into by the time it was read.
var images sync.Map // digest string -> []byte

// imageKey is the digest of the migration set when dsn names a database that would be
// created empty: ":memory:", or a plain path with no file at it. It is "" for an
// existing file, a URI, or a set [migrate.Run] will refuse to load.
func imageKey(dsn string, migrations fs.FS) string {
	if dsn != ":memory:" {
		if strings.HasPrefix(dsn, "file:") || strings.Contains(dsn, "?") {
			return ""
		}
		if _, err := os.Stat(dsn); !errors.Is(err, fs.ErrNotExist) {
			return ""
		}
	}
	key, err := migrate.Digest(migrations)
	if err != nil {
		return ""
	}
	return key
}

func cachedImage(key string) []byte {
	if v, ok := images.Load(key); ok {
		return v.([]byte)
	}
	return nil
}

// migratedImage returns the image for key, building it by migrating a ":memory:"
// database if no open in this process has yet. It returns nil if that fails, and the
// caller creates its database the ordinary way.
func migratedImage(ctx context.Context, migrations fs.FS, key string) []byte {
	db, err := open(ctx, ":memory:", migrations)
	if err != nil {
		return nil
	}
	_ = db.Close()
	return cachedImage(key)
}

// loadImage replaces the empty in-memory database on db's one connection with img.
func loadImage(ctx context.Context, db *sql.DB, img []byte) error {
	if err := rawConn(ctx, db, func(s serializer) error { return s.Deserialize(img) }); err != nil {
		return err
	}
	// The page cache size belongs to the database the connection had open, so the
	// loaded one starts at SQLite's default.
	_, err := db.ExecContext(ctx, "PRAGMA cache_size("+cacheSize+")")
	return err
}

// storeImage caches db's freshly migrated in-memory database under key. A failure
// only costs later opens their migrations, so it is not reported.
func storeImage(ctx context.Context, db *sql.DB, key string) {
	var img []byte
	err := rawConn(ctx, db, func(s serializer) error {
		var err error
		img, err = s.Serialize()
		return err
	})
	if err == nil && len(img) > 0 {
		images.LoadOrStore(key, img)
	}
}

// serializer is the part of modernc.org/sqlite's driver connection that copies a whole
// database in and out of memory.
type serializer interface {
	Serialize() ([]byte, error)
	Deserialize([]byte) error
}

// rawConn runs fn on db's driver connection. Open caps db at one connection, so this
// is the connection that holds an in-memory database.
func rawConn(ctx context.Context, db *sql.DB, fn func(serializer) error) error {
	c, err := db.Conn(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()
	return c.Raw(func(dc any) error {
		s, ok := dc.(serializer)
		if !ok {
			return fmt.Errorf("driver connection %T cannot serialize", dc)
		}
		return fn(s)
	})
}
