package backup

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
)

// Writer writes a SignalBackups folder (one snapshot plus files/) that a phone can restore with the account's
// recovery key, in the layout the Signal apps produce.
type Writer struct {
	root     string
	snapshot string
	files    string
	bk       []byte
	backupID []byte

	mainPath string
	out      *os.File
	enc      *cbcWriter
	gz       *gzip.Writer
	buf      *bufio.Writer
}

// NewWriter creates dir/SignalBackups/signal-backup-<UTC time>/ and dir/SignalBackups/files/, and writes info as the
// main file's first frame. The backup ID is derived from the recovery key and the account's ACI, as iOS checks.
func NewWriter(dir, recoveryKey string, aci [16]byte, info *backuppb.BackupInfo, now time.Time) (*Writer, error) {
	key, err := NormalizeRecoveryKey(recoveryKey)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(dir, "SignalBackups")
	w := &Writer{
		root:     root,
		snapshot: filepath.Join(root, "signal-backup-"+now.UTC().Format("2006-01-02-15-04-05")),
		files:    filepath.Join(root, "files"),
		bk:       backupKey(key),
	}
	w.backupID = backupIDFromACI(w.bk, aci)
	for _, d := range []string{w.snapshot, w.files} {
		if err = os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	hmacKey, aesKey := messageBackupKeys(w.bk, w.backupID)
	w.mainPath = filepath.Join(w.snapshot, "main")
	if w.out, err = os.Create(w.mainPath + ".partial"); err != nil {
		return nil, err
	}
	if w.enc, err = newCBCWriter(w.out, aesKey, hmacKey); err != nil {
		_ = w.out.Close()
		return nil, err
	}
	w.gz = gzip.NewWriter(w.enc)
	w.buf = bufio.NewWriterSize(w.gz, chunk)
	if err = w.writeRecord(info); err != nil {
		_ = w.out.Close()
		return nil, err
	}
	return w, nil
}

func (w *Writer) BackupDir() string {
	return w.root
}

func (w *Writer) writeRecord(m proto.Message) error {
	data, err := proto.Marshal(m)
	if err != nil {
		return err
	}
	if _, err = w.buf.Write(binary.AppendUvarint(nil, uint64(len(data)))); err != nil {
		return err
	}
	_, err = w.buf.Write(data)
	return err
}

func (w *Writer) WriteFrame(f *backuppb.Frame) error {
	return w.writeRecord(f)
}

// AddAttachment encrypts what src holds into files/<xx>/<media name>, as the apps' local backups do, and returns the
// locator for its FilePointer. The transit key is required to be present but only used for CDN downloads, which
// local restores don't do, so it's random.
func (w *Writer) AddAttachment(src io.Reader) (*backuppb.FilePointer_LocatorInfo, error) {
	localKey := make([]byte, 64)
	transitKey := make([]byte, 64)
	if _, err := rand.Read(localKey); err != nil {
		return nil, err
	}
	if _, err := rand.Read(transitKey); err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp(w.files, ".partial-")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	// localKey is AES(32) ‖ HMAC(32), the reverse of the main file's key order.
	enc, err := newCBCWriter(tmp, localKey[:32], localKey[32:])
	if err != nil {
		_ = tmp.Close()
		return nil, err
	}
	digest := sha256.New()
	size, err := io.Copy(io.MultiWriter(enc, digest), src)
	if err == nil {
		err = enc.Close()
	}
	if err = errors.Join(err, tmp.Close()); err != nil {
		return nil, err
	}
	if size > int64(^uint32(0)) {
		return nil, fmt.Errorf("attachment of %d bytes is too large for a backup", size)
	}
	hash := digest.Sum(nil)
	name := LocalMediaName(hash, localKey)
	dest := filepath.Join(w.files, name[:2], name)
	if err = os.MkdirAll(filepath.Dir(dest), 0o700); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp.Name(), dest); err != nil {
		return nil, err
	}
	return &backuppb.FilePointer_LocatorInfo{
		Key:            transitKey,
		IntegrityCheck: &backuppb.FilePointer_LocatorInfo_PlaintextHash{PlaintextHash: hash},
		Size:           uint32(size),
		LocalKey:       localKey,
	}, nil
}

// Close finishes the main file (compressing, encrypting and MACing the rest) and writes the metadata.
func (w *Writer) Close() error {
	err := w.buf.Flush()
	if err == nil {
		err = w.gz.Close()
	}
	if err == nil {
		err = w.enc.Close()
	}
	if err == nil {
		err = w.out.Sync()
	}
	if err = errors.Join(err, w.out.Close()); err != nil {
		return err
	}
	if err = os.Rename(w.mainPath+".partial", w.mainPath); err != nil {
		return err
	}
	return w.writeMetadata()
}

func (w *Writer) writeMetadata() error {
	iv := make([]byte, 12)
	if _, err := rand.Read(iv); err != nil {
		return err
	}
	encrypted, err := ctr(localMetadataKey(w.bk), iv, w.backupID)
	if err != nil {
		return err
	}
	data, err := proto.Marshal(&backuppb.Metadata{
		Version:  1,
		BackupId: &backuppb.Metadata_EncryptedBackupId{Iv: iv, EncryptedId: encrypted},
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(w.snapshot, "metadata"), data, 0o600)
}
