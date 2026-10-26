package importer_test

import (
	"encoding/base64"
	"os"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/importer"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signaltest"
)

const alice = signaltest.AliceACI

func newManager(t *testing.T, h *signaltest.Harness) *importer.Manager {
	m, err := importer.NewManager(t.TempDir(), h.Dir, h.Svc, h.Store, h.Files, h.Broker, zerolog.New(zerolog.NewTestWriter(t)))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// runImport uploads a zip (in two chunks) and imports it, returning the final status.
func runImport(t *testing.T, h *signaltest.Harness, m *importer.Manager, zipPath, recoveryKey string) importer.Status {
	t.Helper()
	data, err := os.ReadFile(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(int64(len(data))); err != nil {
		t.Fatal(err)
	}
	half := len(data) / 2
	if err = m.Append(0, strings.NewReader(string(data[:half]))); err != nil {
		t.Fatal(err)
	}
	if err = m.Append(int64(half), strings.NewReader(string(data[half:]))); err != nil {
		t.Fatal(err)
	}
	if err = m.Run(h.Ctx, recoveryKey); err != nil {
		t.Fatal(err)
	}
	var s importer.Status
	h.Eventually("the import", func() bool {
		s = m.Status()
		return s.State == importer.StateDone || s.State == importer.StateFailed
	})
	return s
}

func TestImportBackup(t *testing.T) {
	h := signaltest.New(t)
	// A message that arrived live before the import is in the backup too.
	h.Push(signaltest.Text(alice, int64(signaltest.At(0)), "hi from the backup", ""))
	h.WaitForMessages(signaltest.DMKey(alice), 1)

	m := newManager(t, h)
	zipPath := signaltest.SampleBackup(t)
	s := runImport(t, h, m, zipPath, signaltest.RecoveryKey)
	if s.State != importer.StateDone {
		t.Fatalf("import: %+v", s)
	}
	if s.Counts.Messages != 9 || s.Counts.Skipped != 1 || s.Counts.Attachments != 1 {
		t.Fatalf("counts: %+v", s.Counts)
	}

	msgs := h.Messages(signaltest.DMKey(alice))
	var bodies []string
	for _, msg := range msgs {
		bodies = append(bodies, msg.Kind+":"+msg.Body)
	}
	// The photo's text is its caption.
	want := []string{"message:hi from the backup", "message:my reply", "message:second version", "message:",
		"message:", "update:Your safety number with Alice changed"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("messages:\n%v\nwant\n%v", bodies, want)
	}
	if !msgs[1].Outgoing || msgs[1].Status != "read" {
		t.Fatalf("own message: %+v", msgs[1])
	}
	if msgs[2].EditedAt != int64(signaltest.At(40)) || msgs[2].SentAt != int64(signaltest.At(30)) {
		t.Fatalf("edited message: %+v", msgs[2])
	}
	if a := msgs[3].Attachments; len(a) != 1 || a[0].Status != "present" || a[0].Filename != "beach.jpg" || a[0].Caption != "look" {
		t.Fatalf("attachment: %+v", a)
	}
	file, err := h.Store.AttachmentFile(h.Ctx, msgs[3].Attachments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := h.Files.Read(file.SHA256); err != nil || string(data) != "a photo" {
		t.Fatalf("attachment file: %q, %v", data, err)
	}
	if !msgs[4].Deleted {
		t.Fatalf("deleted message: %+v", msgs[4])
	}
	list, err := h.Store.ListConversations(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]store.ConversationSummary{}
	for _, c := range list {
		byName[c.Name] = c
	}
	// The safety number notice is quiet: the chat's preview is still the last message.
	if c := byName["Alice"]; c.Preview != "Message deleted" {
		t.Fatalf("Alice's chat: %+v", c)
	}
	for _, name := range []string{"Weekend plans", "Book club", "Note to Self", "Carol"} {
		if _, ok := byName[name]; !ok {
			t.Fatalf("no chat %q in %+v", name, list)
		}
	}

	group := h.Messages(signaltest.GroupKey())
	if len(group) != 3 || group[1].Body != "@Alice bring snacks" || !strings.Contains(group[1].FormattedBody, "<strong>bring</strong>") {
		t.Fatalf("group: %+v", group)
	}
	if group[1].Quote == nil || group[1].Quote.MessageID != group[0].ID {
		t.Fatalf("quote: %+v", group[1].Quote)
	}
	if group[2].Kind != "update" || group[2].AuthorName != "Bob" || group[2].Body != "named the group “Weekend plans”" {
		t.Fatalf("group update: %+v", group[2])
	}

	// The group signal-cli doesn't know keeps its master key, for exports.
	oldGroupID := base64.StdEncoding.EncodeToString(backup.GroupIDFromMasterKey(signaltest.OldGroupKey))
	portal, err := h.Svc.Bridge().GetExistingPortalByKey(h.Ctx, signalid.GroupPortalKey(oldGroupID))
	if err != nil || portal == nil || portal.Metadata.(*connector.PortalMetadata).MasterKey != base64.StdEncoding.EncodeToString(signaltest.OldGroupKey) {
		t.Fatalf("old group portal: %+v, %v", portal, err)
	}

	// Importing the same backup again changes nothing.
	again := runImport(t, h, m, zipPath, signaltest.RecoveryKey)
	if again.State != importer.StateDone || again.Counts.Messages != 0 {
		t.Fatalf("second import: %+v", again)
	}
	if n := len(h.Messages(signaltest.DMKey(alice))); n != len(msgs) {
		t.Fatalf("messages after importing again: %d, was %d", n, len(msgs))
	}
}

func TestImportWithWrongKey(t *testing.T) {
	h := signaltest.New(t)
	m := newManager(t, h)
	s := runImport(t, h, m, signaltest.SampleBackup(t), strings.Repeat("a", 64))
	if s.State != importer.StateFailed || !s.WrongKey {
		t.Fatalf("status: %+v", s)
	}
	// The upload is kept, to try again with the right key.
	if err := m.Run(h.Ctx, signaltest.RecoveryKey); err != nil {
		t.Fatal(err)
	}
	h.Eventually("the import", func() bool { return m.Status().State == importer.StateDone })
}
