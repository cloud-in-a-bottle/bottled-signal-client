package exporter

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/importer"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signaltest"
)

func importSample(t *testing.T, h *signaltest.Harness, zipPath string) importer.Counts {
	t.Helper()
	m, err := importer.NewManager(t.TempDir(), h.Dir, h.Svc, h.Store, h.Files, h.Broker, zerolog.New(zerolog.NewTestWriter(t)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()
	if err = m.Start(info.Size()); err != nil {
		t.Fatal(err)
	}
	if err = m.Append(0, f); err != nil {
		t.Fatal(err)
	}
	if err = m.Run(h.Ctx, signaltest.RecoveryKey); err != nil {
		t.Fatal(err)
	}
	h.Eventually("the import", func() bool { return m.Status().State == importer.StateDone || m.Status().State == importer.StateFailed })
	if s := m.Status(); s.State != importer.StateDone {
		t.Fatalf("import: %+v", s)
	}
	return m.Status().Counts
}

// export runs an export and returns its zip's path.
func runExport(t *testing.T, h *signaltest.Harness) (string, Status) {
	t.Helper()
	m, err := NewManager(filepath.Join(t.TempDir(), "exports"), h.SignalDir, h.Svc, h.Store, h.Files, h.Broker, zerolog.New(zerolog.NewTestWriter(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Eventually("the export", func() bool { return m.Status().State != StateExporting })
	s := m.Status()
	if s.State != StateDone || s.RecoveryKey != signaltest.RecoveryKey {
		t.Fatalf("export: %+v", s)
	}
	dir, done, err := m.download(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	if err = m.Delete(); !errors.Is(err, ErrDownloading) {
		t.Fatalf("deleting during a download: %v", err)
	}
	out := filepath.Join(t.TempDir(), "SignalBackups.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	if err = writeZip(dir, f); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return out, s
}

func readFrames(t *testing.T, zipPath string) (*backup.Archive, []*backuppb.Frame) {
	t.Helper()
	a, err := backup.Open(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = a.Close() })
	r, err := a.Frames(signaltest.RecoveryKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var frames []*backuppb.Frame
	for {
		f, err := r.Next()
		if errors.Is(err, io.EOF) {
			return a, frames
		} else if err != nil {
			t.Fatal(err)
		}
		frames = append(frames, f)
	}
}

func TestExportRoundTrip(t *testing.T) {
	h := signaltest.New(t)
	h.WriteAccountFiles()
	imported := importSample(t, h, signaltest.SampleBackup(t))
	zipPath, status := runExport(t, h)
	if status.Counts.Messages != imported.Messages || status.Counts.Attachments != 1 {
		t.Fatalf("exported %+v, imported %+v", status.Counts, imported)
	}

	a, frames := readFrames(t, zipPath)
	var account *backuppb.AccountData
	var items []*backuppb.ChatItem
	groups := 0
	for _, f := range frames {
		switch {
		case f.GetAccount() != nil:
			account = f.GetAccount()
		case f.GetRecipient().GetGroup() != nil:
			groups++
		case f.GetChatItem() != nil:
			items = append(items, f.GetChatItem())
		}
	}
	if account == nil || account.GetGivenName() != "Me" {
		t.Fatalf("account: %+v", account)
	}
	// Both groups go in: the one signal-cli knows, and the one whose key came from the imported backup.
	if groups != 2 {
		t.Fatalf("%d groups", groups)
	}
	var edited, photo *backuppb.ChatItem
	deleted := 0
	for _, it := range items {
		switch {
		case len(it.GetRevisions()) > 0:
			edited = it
		case len(it.GetStandardMessage().GetAttachments()) > 0:
			photo = it
		case it.GetRemoteDeletedMessage() != nil:
			deleted++
		}
	}
	if edited == nil || edited.GetDateSent() != signaltest.At(40) || edited.GetStandardMessage().GetText().GetBody() != "second version" ||
		edited.GetRevisions()[0].GetDateSent() != signaltest.At(30) || edited.GetRevisions()[0].GetStandardMessage().GetText().GetBody() != "first version" {
		t.Fatalf("edited message: %v", edited)
	}
	if deleted != 1 {
		t.Fatalf("%d deleted messages", deleted)
	}
	var file bytes.Buffer
	if photo == nil {
		t.Fatal("no photo")
	} else if found, err := a.WriteAttachment(photo.GetStandardMessage().GetAttachments()[0].GetPointer(), &file); !found || err != nil || file.String() != "a photo" {
		t.Fatalf("photo: %q, %v, %v", file.String(), found, err)
	}

	// A phone restoring it gets the same history: importing it into a new server gives the same chats.
	fresh := signaltest.New(t)
	reimported := importSample(t, fresh, zipPath)
	if reimported.Messages != imported.Messages {
		t.Fatalf("reimported %+v, imported %+v", reimported, imported)
	}
	bodies := func(h *signaltest.Harness) []string {
		var out []string
		for _, m := range h.Messages(signaltest.DMKey(signaltest.AliceACI)) {
			if m.Kind == "message" {
				out = append(out, m.Body)
			}
		}
		return out
	}
	if a, b := bodies(h), bodies(fresh); !slices.Equal(a, b) {
		t.Fatalf("after the round trip:\n%v\nwas\n%v", b, a)
	}
}

func TestExportNeedsTheKeys(t *testing.T) {
	h := signaltest.New(t)
	m, err := NewManager(filepath.Join(t.TempDir(), "exports"), h.SignalDir, h.Svc, h.Store, h.Files, h.Broker, zerolog.New(zerolog.NewTestWriter(t)))
	if err != nil {
		t.Fatal(err)
	}
	if err = m.Start(h.Ctx); err != nil {
		t.Fatal(err)
	}
	h.Eventually("the export", func() bool { return m.Status().State != StateExporting })
	if s := m.Status(); s.State != StateFailed {
		t.Fatalf("export without signal-cli's keys: %+v", s)
	}
}

func TestExportUsesSignalsTimestampForEdits(t *testing.T) {
	h := signaltest.New(t)
	h.WriteAccountFiles()
	h.Push(signaltest.Text(signaltest.AliceACI, time.Now().UnixMilli(), "hi", ""))
	h.WaitForMessages(signaltest.DMKey(signaltest.AliceACI), 1)
	if _, err := h.Svc.Send(h.Ctx, h.ConversationID(signaltest.DMKey(signaltest.AliceACI)), "typo", 0, nil); err != nil {
		t.Fatal(err)
	}
	msgs := h.WaitFor(signaltest.DMKey(signaltest.AliceACI), func(m []store.Message) bool { return len(m) == 2 && m[1].Status == "sent" })
	if err := h.Svc.Edit(h.Ctx, msgs[1].ID, "fixed"); err != nil {
		t.Fatal(err)
	}
	zipPath, _ := runExport(t, h)
	_, frames := readFrames(t, zipPath)
	for _, f := range frames {
		if f.GetChatItem().GetStandardMessage().GetText().GetBody() == "fixed" {
			// The fake signal-cli's second send (the edit) has timestamp 1_800_000_000_002.
			if got := f.GetChatItem().GetDateSent(); got != 1_800_000_000_002 {
				t.Fatalf("edited message exported as sent at %d", got)
			}
			return
		}
	}
	t.Fatal("edited message not exported")
}
