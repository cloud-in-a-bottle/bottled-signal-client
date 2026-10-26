package signaltest

import (
	"archive/zip"
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
)

// BackupBuilder writes an on-device backup as a phone would, for tests to import.
type BackupBuilder struct {
	t   *testing.T
	dir string
	w   *backup.Writer
}

// NewBackup starts a backup of the harness's account, with its recovery key.
func NewBackup(t *testing.T) *BackupBuilder {
	dir := t.TempDir()
	info := &backuppb.BackupInfo{Version: 1, BackupTimeMs: uint64(time.Now().UnixMilli()), MediaRootBackupKey: make([]byte, 32),
		CurrentAppVersion: "test", FirstAppVersion: "test"}
	w, err := backup.NewWriter(dir, RecoveryKey, uuid.MustParse(SelfACI), info, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return &BackupBuilder{t: t, dir: dir, w: w}
}

// Attachment adds a file, returning a pointer to it for a message.
func (b *BackupBuilder) Attachment(data []byte, contentType, filename string) *backuppb.FilePointer {
	b.t.Helper()
	loc, err := b.w.AddAttachment(bytes.NewReader(data))
	if err != nil {
		b.t.Fatal(err)
	}
	return &backuppb.FilePointer{ContentType: &contentType, FileName: &filename, LocatorInfo: loc}
}

func (b *BackupBuilder) Frames(frames ...*backuppb.Frame) {
	b.t.Helper()
	for _, f := range frames {
		if err := b.w.WriteFrame(f); err != nil {
			b.t.Fatal(err)
		}
	}
}

// Zip finishes the backup and zips its SignalBackups folder, as a person would to upload it.
func (b *BackupBuilder) Zip() string {
	b.t.Helper()
	if err := b.w.Close(); err != nil {
		b.t.Fatal(err)
	}
	return ZipDir(b.t, b.w.BackupDir())
}

// ZipDir zips a folder (with the folder itself at the zip's root) into a new file, returning its path.
func ZipDir(t *testing.T, dir string) string {
	t.Helper()
	out := filepath.Join(t.TempDir(), "backup.zip")
	f, err := os.Create(out)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	root := filepath.Dir(dir)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(w, src)
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
