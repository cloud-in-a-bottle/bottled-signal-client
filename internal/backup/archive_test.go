package backup

import (
	"archive/zip"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
)

var testACI = [16]byte{0x65, 0x9a, 0xa5, 0xf4, 0xa2, 0x8d, 0xfc, 0xc1, 0x1e, 0xa1, 0xb9, 0x97, 0x53, 0x7a, 0x3d, 0x95}

// zipDir zips dir's contents, with member names under prefix and dir's own paths, returning the zip's path.
func zipDir(t *testing.T, dir string, rename func(name string) string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "upload.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		name := filepath.ToSlash(rel)
		if rename != nil {
			if name = rename(name); name == "" {
				return nil
			}
		}
		w, err := zw.Create(name)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, err = w.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	return out
}

type written struct {
	dir     string
	frames  []*backuppb.Frame
	pointer *backuppb.FilePointer
	file    []byte
}

// writeBackup writes a backup with a few frames and one attachment into a fresh dir.
func writeBackup(t *testing.T, now time.Time) written {
	t.Helper()
	dir := t.TempDir()
	w, err := NewWriter(dir, testRecoveryKey, testACI, &backuppb.BackupInfo{Version: 1, BackupTimeMs: 1}, now)
	if err != nil {
		t.Fatal(err)
	}
	// Larger than a CBC chunk, and not a multiple of the block size.
	file := bytes.Repeat([]byte("attachment bytes "), 70_000)
	loc, err := w.AddAttachment(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	pointer := &backuppb.FilePointer{ContentType: proto.String("image/png"), LocatorInfo: loc}
	var frames []*backuppb.Frame
	for i := range 3000 {
		item := &backuppb.ChatItem{ChatId: 1, AuthorId: 2, DateSent: uint64(i)}
		item.Item = &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
			Text:        &backuppb.Text{Body: strings.Repeat("message ", i%50)},
			Attachments: []*backuppb.MessageAttachment{{Pointer: pointer}},
		}}
		frames = append(frames, &backuppb.Frame{Item: &backuppb.Frame_ChatItem{ChatItem: item}})
	}
	for _, f := range frames {
		if err = w.WriteFrame(f); err != nil {
			t.Fatal(err)
		}
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	return written{dir: dir, frames: frames, pointer: pointer, file: file}
}

func readAllFrames(t *testing.T, a *Archive, key string) []*backuppb.Frame {
	t.Helper()
	var last float64
	r, err := a.Frames(key, func(f float64) { last = f })
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Info().GetVersion() != 1 || r.Info().GetBackupTimeMs() != 1 {
		t.Fatalf("info %v", r.Info())
	}
	var out []*backuppb.Frame
	for {
		f, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		out = append(out, f)
	}
	if last < 0.99 {
		t.Errorf("progress ended at %f", last)
	}
	return out
}

func TestRoundTrip(t *testing.T) {
	b := writeBackup(t, time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC))
	if _, err := os.Stat(filepath.Join(b.dir, "SignalBackups", "signal-backup-2026-10-01-12-00-00", "main")); err != nil {
		t.Fatal(err)
	}
	a, err := Open(zipDir(t, b.dir, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	// As iOS shows the key.
	shown := strings.ToUpper(strings.NewReplacer("0", "=", "o", "#").Replace(testRecoveryKey))
	got := readAllFrames(t, a, shown)
	if len(got) != len(b.frames) {
		t.Fatalf("%d frames, want %d", len(got), len(b.frames))
	}
	for i := range got {
		if !proto.Equal(got[i], b.frames[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
	var buf bytes.Buffer
	found, err := a.WriteAttachment(b.pointer, &buf)
	if err != nil || !found || !bytes.Equal(buf.Bytes(), b.file) {
		t.Fatalf("attachment: found %v, err %v, %d bytes", found, err, buf.Len())
	}
	found, err = a.WriteAttachment(&backuppb.FilePointer{ContentType: proto.String("video/mp4")}, io.Discard)
	if found || err != nil {
		t.Errorf("pointer without locator: %v %v", found, err)
	}
}

func TestWrongKey(t *testing.T) {
	b := writeBackup(t, time.Now())
	a, err := Open(zipDir(t, b.dir, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	other := "a" + testRecoveryKey[1:]
	if _, err = a.Frames(other, nil); !errors.Is(err, ErrWrongRecoveryKey) {
		t.Errorf("wrong key: %v", err)
	}
	if _, err = a.Frames("nope", nil); !errors.Is(err, ErrWrongRecoveryKey) {
		t.Errorf("malformed key: %v", err)
	}
}

func TestAttachmentTampering(t *testing.T) {
	b := writeBackup(t, time.Now())
	var attachment string
	_ = filepath.WalkDir(filepath.Join(b.dir, "SignalBackups", "files"), func(path string, d fs.DirEntry, err error) error {
		if !d.IsDir() {
			attachment = path
		}
		return nil
	})
	data, err := os.ReadFile(attachment)
	if err != nil {
		t.Fatal(err)
	}
	data[100] ^= 1
	if err = os.WriteFile(attachment, data, 0o600); err != nil {
		t.Fatal(err)
	}
	a, err := Open(zipDir(t, b.dir, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if found, err := a.WriteAttachment(b.pointer, io.Discard); !found || err == nil {
		t.Errorf("tampered attachment: found %v, err %v", found, err)
	}
}

func TestSnapshotSelection(t *testing.T) {
	older := writeBackup(t, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	newer := writeBackup(t, time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))
	// Both snapshots in one SignalBackups folder, plus an incomplete "-tmp" one that sorts last, with both files/.
	dir := t.TempDir()
	for _, b := range []written{older, newer} {
		if err := os.CopyFS(dir, os.DirFS(b.dir)); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(dir, "SignalBackups", "signal-backup-2026-03-01-00-00-00-tmp")
	if err := os.CopyFS(tmp, os.DirFS(filepath.Join(older.dir, "SignalBackups", "signal-backup-2026-01-01-00-00-00"))); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(string) string{
		"nested":       func(n string) string { return "Backups/" + n },
		"root":         nil,
		"macos junk":   func(n string) string { return n },
		"no files dir": func(n string) string { return strings.Replace(n, "SignalBackups/files/", "elsewhere/", 1) },
	}
	for name, rename := range cases {
		t.Run(name, func(t *testing.T) {
			zipPath := zipDir(t, dir, rename)
			if name == "macos junk" {
				zipPath = addMember(t, zipPath, "__MACOSX/SignalBackups/signal-backup-2099-01-01-00-00-00/metadata", "__MACOSX/SignalBackups/signal-backup-2099-01-01-00-00-00/main")
			}
			a, err := Open(zipPath)
			if err != nil {
				t.Fatal(err)
			}
			defer a.Close()
			if !strings.HasSuffix(a.snapshot, "signal-backup-2026-02-01-00-00-00/") {
				t.Errorf("chose %s", a.snapshot)
			}
			if len(readAllFrames(t, a, testRecoveryKey)) != len(newer.frames) {
				t.Error("wrong frame count")
			}
			found, err := a.WriteAttachment(newer.pointer, io.Discard)
			if err != nil || found != (name != "no files dir") {
				t.Errorf("attachment: found %v, err %v", found, err)
			}
		})
	}
	// files/ inside the snapshot folder, as when only a snapshot was zipped.
	t.Run("files in snapshot", func(t *testing.T) {
		zipPath := zipDir(t, newer.dir, func(n string) string {
			if after, ok := strings.CutPrefix(n, "SignalBackups/files/"); ok {
				return "snap/files/" + after
			}
			return strings.Replace(n, "SignalBackups/signal-backup-2026-02-01-00-00-00/", "snap/", 1)
		})
		a, err := Open(zipPath)
		if err != nil {
			t.Fatal(err)
		}
		defer a.Close()
		if found, err := a.WriteAttachment(newer.pointer, io.Discard); !found || err != nil {
			t.Errorf("attachment: found %v, err %v", found, err)
		}
	})
}

// addMember copies a zip, adding empty members.
func addMember(t *testing.T, zipPath string, names ...string) string {
	t.Helper()
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	out := filepath.Join(t.TempDir(), "with-extra.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for _, m := range r.File {
		if err = zw.Copy(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range names {
		if _, err = zw.Create(n); err != nil {
			t.Fatal(err)
		}
	}
	if err = errors.Join(zw.Close(), f.Close()); err != nil {
		t.Fatal(err)
	}
	return out
}

func zipOf(t *testing.T, members map[string][]byte) string {
	t.Helper()
	dir := t.TempDir()
	for name, data := range members {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return zipDir(t, dir, nil)
}

func TestRejectsOtherFiles(t *testing.T) {
	notZip := filepath.Join(t.TempDir(), "x.zip")
	if err := os.WriteFile(notZip, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := zipOf(t, map[string][]byte{"signal-2024-01-01.backup": []byte("x")})
	empty := zipOf(t, map[string][]byte{"readme.txt": []byte("x")})
	for name, path := range map[string]string{"not a zip": notZip, "legacy": legacy, "no snapshot": empty} {
		if _, err := Open(path); !errors.As(err, new(InvalidBackupError)) {
			t.Errorf("%s: %v", name, err)
		} else if name == "legacy" && !strings.Contains(err.Error(), "legacy Android") {
			t.Errorf("legacy: %v", err)
		}
	}

	b := writeBackup(t, time.Now())
	var metaPath string
	_ = filepath.WalkDir(b.dir, func(path string, d fs.DirEntry, err error) error {
		if d.Name() == "metadata" {
			metaPath = path
		}
		return nil
	})
	metadata, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	cloud := zipOf(t, map[string][]byte{
		"s/metadata": metadata,
		"s/main":     append([]byte("SBACKUP\x01"), bytes.Repeat([]byte{1}, 100)...),
	})
	a, err := Open(cloud)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	if _, err = a.Frames(testRecoveryKey, nil); !errors.As(err, new(InvalidBackupError)) || !strings.Contains(err.Error(), "cloud") {
		t.Errorf("cloud backup: %v", err)
	}
}

func TestBackupIsForItsAccount(t *testing.T) {
	dir := t.TempDir()
	w, err := NewWriter(dir, testRecoveryKey, testACI, &backuppb.BackupInfo{Version: 1}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	a, err := Open(zipDir(t, dir, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	r, err := a.Frames(testRecoveryKey, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	other := testACI
	other[0]++
	if !r.IsFor(testACI) || r.IsFor(other) {
		t.Fatal("backup's account not told apart")
	}
}
