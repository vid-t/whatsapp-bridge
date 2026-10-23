package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenPlain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.db")
	db, err := Open(p, "")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE t (x TEXT); INSERT INTO t VALUES ('hello')`); err != nil {
		t.Fatal(err)
	}
	var mode string
	if err := db.QueryRow(`PRAGMA journal_mode`).Scan(&mode); err != nil || mode != "wal" {
		t.Fatalf("journal_mode = %q, err = %v; want wal", mode, err)
	}
}

func TestOpenEncrypted(t *testing.T) {
	p := filepath.Join(t.TempDir(), "enc.db")
	key := strings.Repeat("ab", 32)
	db, err := Open(p, key)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE secret (x TEXT); INSERT INTO secret VALUES ('plaintext-marker-xyz')`); err != nil {
		t.Fatal(err)
	}
	// Force WAL contents into the main file so the on-disk check is meaningful.
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(raw, []byte("SQLite format 3")) {
		t.Fatal("encrypted file has a plaintext SQLite header")
	}
	if bytes.Contains(raw, []byte("plaintext-marker-xyz")) || bytes.Contains(raw, []byte("CREATE TABLE")) {
		t.Fatal("encrypted file contains plaintext")
	}

	if _, err := Open(p, strings.Repeat("cd", 32)); err == nil {
		t.Fatal("wrong key opened the database")
	}
	if _, err := Open(p, ""); err == nil {
		t.Fatal("missing key opened an encrypted database")
	}

	db, err = Open(p, key)
	if err != nil {
		t.Fatalf("reopen with right key: %v", err)
	}
	defer db.Close()
	var got string
	if err := db.QueryRow(`SELECT x FROM secret`).Scan(&got); err != nil || got != "plaintext-marker-xyz" {
		t.Fatalf("got %q, err %v", got, err)
	}
}

func TestOpenRejectsBadKey(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "x.db"), "not-hex"); err == nil {
		t.Fatal("expected error for malformed key")
	}
}
