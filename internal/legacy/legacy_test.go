package legacy

import (
	"crypto/sha256"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signaltest"
)

// The previous version's schema (src/server/db/schema.sql before the move to Go).
//
//go:embed testdata/schema.sql
var oldSchema string

// writeOldDatabase writes a database as the previous version left it, with its attachment files.
func writeOldDatabase(t *testing.T, path, attachments string) {
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(oldSchema); err != nil {
		t.Fatal(err)
	}
	photo := []byte("an old photo")
	sum := sha256.Sum256(photo)
	sha := hex.EncodeToString(sum[:])
	if err = os.MkdirAll(filepath.Join(attachments, sha[:2]), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(attachments, sha[:2], sha), photo, 0o600); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	statements := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO recipients (id, kind, aci, e164, profile_name, updated_at) VALUES (1, 'self', ?, ?, 'Me', 0)",
			[]any{signaltest.SelfACI, signaltest.SelfNumber}},
		{"INSERT INTO recipients (id, kind, aci, profile_name, contact_name, updated_at) VALUES (2, 'contact', ?, 'Alice P', 'Alice', 0)",
			[]any{signaltest.AliceACI}},
		{"INSERT INTO recipients (id, kind, aci, profile_name, updated_at) VALUES (3, 'contact', ?, 'Bob', 0)", []any{signaltest.BobACI}},
		{"INSERT INTO recipients (id, kind, group_id, group_master_key, group_title, updated_at) VALUES (4, 'group', ?, ?, 'Weekend plans', 0)",
			[]any{signaltest.GroupID, base64.StdEncoding.EncodeToString(signaltest.GroupMasterKey)}},
		{"INSERT INTO recipients (id, kind, e164, updated_at) VALUES (5, 'contact', '+15559990000', 0)", nil},
		{"INSERT INTO group_members VALUES (4, 1, 'admin'), (4, 2, 'member'), (4, 3, 'member')", nil},
		{"INSERT INTO conversations (id, recipient_id, pinned, last_read_at, last_message_at) VALUES (1, 2, 1, ?, ?)", []any{now - 50_000, now}},
		{"INSERT INTO conversations (id, recipient_id, last_message_at) VALUES (2, 4, ?)", []any{now}},
		{"INSERT INTO conversations (id, recipient_id, last_message_at) VALUES (3, 5, ?)", []any{now}},
		// Alice's chat: a message with a mention and a style, a reply that she read, an edited message, a photo, a
		// deleted message, a reaction and an update.
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, body, body_ranges, source)
			VALUES (1, 1, 2, ?, ?, 'text', '￼ look', '[{"start":0,"length":1,"mentionAci":"` + signaltest.BobACI + `"},{"start":2,"length":4,"style":"BOLD"}]', 'live')`,
			[]any{now - 60_000, now - 60_000}},
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, body, quote_author_id, quote_sent_at, source)
			VALUES (2, 1, 1, ?, ?, 'text', 'nice', 2, ?, 'sent')`, []any{now - 55_000, now - 55_000, now - 60_000}},
		{"INSERT INTO receipts VALUES (2, 2, 'read', ?)", []any{now - 54_000}},
		{"INSERT INTO reactions VALUES (2, 2, '❤️', ?)", []any{now - 53_000}},
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, body, edited_at, source)
			VALUES (3, 1, 2, ?, ?, 'text', 'version two', ?, 'live')`, []any{now - 40_000, now - 40_000, now - 30_000}},
		{"INSERT INTO message_edits VALUES (3, 0, 'version one', NULL, ?)", []any{now - 40_000}},
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, source)
			VALUES (4, 1, 2, ?, ?, 'text', 'backup')`, []any{now - 20_000, now - 20_000}},
		{`INSERT INTO attachments (message_id, ordinal, content_type, filename, size, file_sha256, status)
			VALUES (4, 0, 'image/jpeg', 'old.jpg', 12, ?, 'present')`, []any{sha}},
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, deleted_at, source)
			VALUES (5, 1, 1, ?, ?, 'text', ?, 'sent')`, []any{now - 10_000, now - 10_000, now - 9_000}},
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, update_payload, source)
			VALUES (6, 1, 2, ?, ?, 'update', '{"type":"expirationTimer","seconds":86400}', 'live')`, []any{now - 5_000, now - 5_000}},
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, body, source)
			VALUES (7, 2, 3, ?, ?, 'text', 'group hello', 'live')`, []any{now - 4_000, now - 4_000}},
		// Someone known only by number can't be told apart from anyone else: left out.
		{`INSERT INTO messages (id, conversation_id, author_id, sent_at, received_at, kind, body, source)
			VALUES (8, 3, 5, ?, ?, 'text', 'who am I', 'live')`, []any{now - 3_000, now - 3_000}},
	}
	for _, s := range statements {
		if _, err = db.Exec(s.query, s.args...); err != nil {
			t.Fatalf("%s: %v", s.query, err)
		}
	}
}

func TestMigratePreviousVersion(t *testing.T) {
	var mainDB string
	h := signaltest.StartWith(t, func(path, attachments string) {
		mainDB = path
		writeOldDatabase(t, path, attachments)
		pending, err := SetAside(path)
		if err != nil || !pending {
			t.Fatalf("set aside: %v, %v", pending, err)
		}
	})
	// The account the previous version registered is taken over without logging in.
	ImportWhenLoggedIn(h.Ctx, mainDB, h.Svc, h.Conn.RPC, h.Store, h.Files, h.Broker, zerolog.New(zerolog.NewTestWriter(t)))
	if h.Svc.CurrentLogin(h.Ctx) == nil {
		t.Fatal("not logged in")
	}

	msgs := h.Messages(signaltest.DMKey(signaltest.AliceACI))
	var lines []string
	for _, m := range msgs {
		lines = append(lines, m.Kind+":"+m.Body)
	}
	want := "message:@Bob look|message:nice|message:version two|message:|message:|update:set messages to disappear after 1 day"
	if strings.Join(lines, "|") != want {
		t.Fatalf("messages:\n%s\nwant\n%s", strings.Join(lines, "|"), want)
	}
	if !strings.Contains(msgs[0].FormattedBody, "<strong>look</strong>") || msgs[0].AuthorName != "Alice" {
		t.Fatalf("formatted message: %+v", msgs[0])
	}
	if m := msgs[1]; m.Status != "read" || m.Quote == nil || m.Quote.MessageID != msgs[0].ID || len(m.Reactions) != 1 {
		t.Fatalf("reply: %+v", m)
	}
	if m := msgs[2]; m.EditedAt == 0 {
		t.Fatalf("edited message: %+v", m)
	}
	if a := msgs[3].Attachments; len(a) != 1 || a[0].Status != "present" || a[0].Filename != "old.jpg" {
		t.Fatalf("photo: %+v", a)
	}
	if !msgs[4].Deleted {
		t.Fatalf("deleted message: %+v", msgs[4])
	}
	if group := h.Messages(signaltest.GroupKey()); len(group) != 1 || group[0].Body != "group hello" || group[0].AuthorName != "Bob" {
		t.Fatalf("group: %+v", group)
	}
	list, err := h.Store.ListConversations(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	pinned := false
	for _, c := range list {
		pinned = pinned || (c.Name == "Alice" && c.Pinned)
	}
	if !pinned {
		t.Fatalf("Alice's chat should be pinned: %+v", list)
	}

	// The old tables are gone, so nothing is imported twice.
	pending, err := SetAside(mainDB)
	if err != nil || pending {
		t.Fatalf("after the migration: %v, %v", pending, err)
	}
}
