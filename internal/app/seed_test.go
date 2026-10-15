package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/importer"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signaltest"
	"github.com/rs/zerolog"
)

// TestSeedDevData fills a data directory with sample chats for looking at the UI, when DEV_SEED_DIR is set (`just
// seed`): messages as they arrive from signal-cli, and an imported backup.
func TestSeedDevData(t *testing.T) {
	dir := os.Getenv("DEV_SEED_DIR")
	if dir == "" {
		t.Skip("DEV_SEED_DIR not set")
	}
	h := signaltest.StartIn(t, dir, nil)
	h.LogIn()
	m, err := importer.NewManager(filepath.Join(dir, "temp", "imports"), filepath.Join(dir, "data", "attachments"),
		h.Svc, h.Store, h.Files, h.Broker, zerolog.Nop())
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(signaltest.SampleBackup(t))
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
	h.Eventually("the import", func() bool { return m.Status().State == importer.StateDone })

	ts := signaltest.At(200)
	styled := "Saturday it is. ￼, bring the good snacks"
	h.Push(&signalcli.Envelope{SourceUUID: signaltest.AliceACI, SourceDevice: 1, Timestamp: int64(ts), DataMessage: &signalcli.DataMessage{
		Timestamp: int64(ts), Message: &styled,
		GroupInfo:  &signalcli.GroupInfo{GroupID: signaltest.GroupID, Type: "DELIVER"},
		Mentions:   []signalcli.Mention{{UUID: signaltest.BobACI, Start: 16, Length: 1}},
		TextStyles: []signalcli.TextStyle{{Style: "BOLD", Start: 29, Length: 4}, {Style: "SPOILER", Start: 34, Length: 6}},
	}})
	if err = os.WriteFile(filepath.Join(h.SignalDir, "attachments", "seed.png"), mustRead(t, "../static/apple-touch-icon.png"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Push(&signalcli.Envelope{SourceUUID: signaltest.AliceACI, SourceDevice: 1, Timestamp: int64(ts) + 1000, DataMessage: &signalcli.DataMessage{
		Timestamp: int64(ts) + 1000, ViewOnce: true,
		Attachments: []signalcli.Attachment{{ID: "seed.png", ContentType: "image/png", Filename: "icon.png"}},
	}})
	h.WaitForMessages(signaltest.DMKey(signaltest.AliceACI), 7)
}

func mustRead(t *testing.T, path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
