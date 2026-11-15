package store

import (
	"context"
	"testing"
)

// TestMigration0077OIDC (server#235): an existing account gains an empty email,
// identities are one per (issuer, subject) and one per account and provider, go
// with their account and outlive the admin who linked them, and pending sign-ins
// are one row per (issuer, subject), never reusing a dismissed row's id.
func TestMigration0077OIDC(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dsn, exec, closeRaw := openBefore(t, "0077")
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(1, 'alex', '', 'user', 't', 't')`)
	exec(`INSERT INTO users(id, username, password_hash, role, created_at, updated_at) VALUES(2, 'root', 'h', 'admin', 't', 't')`)
	closeRaw()

	db, err := Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var email string
	if err := db.writer.QueryRow(`SELECT email FROM users WHERE id = 1`).Scan(&email); err != nil || email != "" {
		t.Fatalf("email = %q, %v; want '' for an existing account", email, err)
	}
	if _, err := db.writer.Exec(`UPDATE users SET email = 'Alex@Example.org' WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM users WHERE email = 'alex@example.org' COLLATE NOCASE`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("case-insensitive email match: %d, %v", n, err)
	}

	const ident = `INSERT INTO user_identities(user_id, issuer, subject, link_method, linked_by, linked_at) VALUES(?, ?, ?, ?, ?, 't')`
	if _, err := db.writer.Exec(ident, 1, "https://idp", "sub-1", "admin", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(ident, 2, "https://idp", "sub-1", "admin", nil); err == nil {
		t.Fatal("a second account took the same (issuer, subject)")
	}
	if _, err := db.writer.Exec(ident, 1, "https://idp", "sub-2", "admin", nil); err == nil {
		t.Fatal("one account took two identities from the same issuer")
	}
	if _, err := db.writer.Exec(ident, 1, "https://other", "sub-1", "created", nil); err != nil {
		t.Fatalf("another issuer's identity: %v", err)
	}
	var seen int
	if err := db.writer.QueryRow(`SELECT groups_seen FROM user_identities WHERE subject = 'sub-1' AND issuer = 'https://idp'`).Scan(&seen); err != nil || seen != 0 {
		t.Fatalf("groups_seen = %d, %v; want 0 for a new identity", seen, err)
	}
	if _, err := db.writer.Exec(ident, 2, "https://idp", "sub-9", "guessed", nil); err == nil {
		t.Fatal("an unknown link_method was accepted")
	}
	// The linking admin goes: the identity stays, linked_by cleared.
	if _, err := db.writer.Exec(`DELETE FROM users WHERE id = 2`); err != nil {
		t.Fatal(err)
	}
	var linkedBy *int64
	if err := db.writer.QueryRow(`SELECT linked_by FROM user_identities WHERE subject = 'sub-1' AND issuer = 'https://idp'`).Scan(&linkedBy); err != nil || linkedBy != nil {
		t.Fatalf("linked_by = %v, %v; want NULL after the admin went", linkedBy, err)
	}
	// The account goes: its identities with it.
	if _, err := db.writer.Exec(`DELETE FROM users WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT COUNT(*) FROM user_identities`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("identities after the account went: %d, %v", n, err)
	}

	const pending = `INSERT INTO oidc_pending(issuer, subject, reason, first_seen_at, last_seen_at) VALUES('https://idp', ?, 'no_match', 't', 't')`
	if _, err := db.writer.Exec(pending, "p-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(pending, "p-1"); err == nil {
		t.Fatal("a second pending row for one identity")
	}
	var groups string
	var attempts int
	if err := db.writer.QueryRow(`SELECT groups, attempts FROM oidc_pending`).Scan(&groups, &attempts); err != nil || groups != "[]" || attempts != 1 {
		t.Fatalf("defaults: groups %q attempts %d, %v", groups, attempts, err)
	}
	// A dismissed row's id is never given to the next one: the newest row is
	// deleted, as dismissing it does, and the next sign-in gets a new id.
	var first, next int64
	if err := db.writer.QueryRow(`SELECT id FROM oidc_pending WHERE subject = 'p-1'`).Scan(&first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(`DELETE FROM oidc_pending WHERE id = ?`, first); err != nil {
		t.Fatal(err)
	}
	if _, err := db.writer.Exec(pending, "p-2"); err != nil {
		t.Fatal(err)
	}
	if err := db.writer.QueryRow(`SELECT id FROM oidc_pending WHERE subject = 'p-2'`).Scan(&next); err != nil || next <= first {
		t.Fatalf("id after a dismissal: %d (dismissed %d), %v", next, first, err)
	}
}
