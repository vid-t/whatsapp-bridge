// Package store opens the bridge's SQLite database using the cgo-free
// ncruces/go-sqlite3 driver. Encryption at rest (Adiantum VFS) is enabled
// when a hex key is supplied; without a key the file is plain SQLite.
package store

import (
	"database/sql"
	"fmt"
	"regexp"

	"github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
)

var hexKeyRe = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// pragmas run on every pooled connection, after the key has been applied.
// Order matters: journal_mode reads the file header, which needs the key.
var pragmas = []string{
	"PRAGMA busy_timeout = 5000",
	"PRAGMA journal_mode = wal",
	"PRAGMA synchronous = normal",
	"PRAGMA foreign_keys = on",
	"PRAGMA temp_store = memory",
}

// Open opens (and creates if needed) the database at path. hexKey is either
// empty (no encryption) or 64 hex characters (256-bit Adiantum key). The key
// is applied on every pooled connection through the driver init hook before
// any other statement, and never appears in the URI.
func Open(path, hexKey string) (*sql.DB, error) {
	if hexKey != "" && !hexKeyRe.MatchString(hexKey) {
		return nil, fmt.Errorf("store: hex key must be 64 hex characters")
	}
	dsn := "file:" + path
	if hexKey != "" {
		dsn += "?vfs=adiantum"
	}
	init := func(c *sqlite3.Conn) error {
		if hexKey != "" {
			if err := c.Exec("PRAGMA hexkey = '" + hexKey + "'"); err != nil {
				return fmt.Errorf("apply key: %w", err)
			}
		}
		for _, p := range pragmas {
			if err := c.Exec(p); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
		}
		// Full-text search is a loadable module in this driver; register it
		// on every connection so FTS5 virtual tables are usable.
		return fts5.Register(c)
	}
	db, err := driver.Open(dsn, init)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	// SQLite + WAL is happiest with a single writer; keep the pool small.
	db.SetMaxOpenConns(4)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping %s: %w", path, err)
	}
	return db, nil
}
