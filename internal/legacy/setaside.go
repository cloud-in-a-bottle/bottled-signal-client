// Package legacy brings over the history kept by this app before it used the shared core (the Python app, with its
// own tables in the same database): its tables are set aside on startup, so the store can start, and imported once
// the account is logged in to the bridge.
package legacy

import (
	"database/sql"
	"errors"
	"fmt"

	// Registers the sqlite3 driver.
	_ "github.com/mattn/go-sqlite3"
)

// The previous app's tables, in an order that drops cleanly (referencing tables first).
var tables = []string{
	"raw_events", "receipts", "reactions", "message_edits", "attachments", "messages", "conversations",
	"group_members", "recipients", "kv",
}

const prefix = "legacy_"

func open(dbPath string) (*sql.DB, error) {
	return sql.Open("sqlite3", fmt.Sprintf("file:%s?_txlock=immediate&_busy_timeout=10000", dbPath))
}

func tableExists(db *sql.DB, name string) (bool, error) {
	var exists bool
	err := db.QueryRow("SELECT EXISTS (SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)", name).Scan(&exists)
	return exists, err
}

// SetAside renames the previous app's tables out of the store's way, if the database still has them, and reports
// whether there is history waiting to be imported.
func SetAside(dbPath string) (bool, error) {
	db, err := open(dbPath)
	if err != nil {
		return false, err
	}
	defer db.Close()
	if pending, err := tableExists(db, prefix+"recipients"); err != nil || pending {
		return pending, err
	}
	legacyRecipients, err := tableExists(db, "recipients")
	if err != nil || !legacyRecipients {
		return false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	// The full-text index and its triggers, and the indexes, have names the store's schema uses too.
	statements := []string{
		"DROP TRIGGER IF EXISTS messages_fts_insert", "DROP TRIGGER IF EXISTS messages_fts_delete",
		"DROP TRIGGER IF EXISTS messages_fts_update", "DROP TABLE IF EXISTS messages_fts",
		"DROP INDEX IF EXISTS messages_conversation_sent", "DROP INDEX IF EXISTS messages_expires",
		"DROP INDEX IF EXISTS attachments_sha",
	}
	for _, t := range tables {
		statements = append(statements, fmt.Sprintf("ALTER TABLE %s RENAME TO %s%s", t, prefix, t))
	}
	for _, stmt := range statements {
		if _, err = tx.Exec(stmt); err != nil {
			return false, errors.Join(fmt.Errorf("%s: %w", stmt, err), tx.Rollback())
		}
	}
	return true, tx.Commit()
}

// drop deletes the previous app's tables, once their history is imported.
func drop(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for _, t := range tables {
		if _, err = tx.Exec("DROP TABLE IF EXISTS " + prefix + t); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	return tx.Commit()
}
