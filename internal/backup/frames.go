package backup

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
)

var remoteBackupMagic = []byte("SBACKUP\x01")

// maxFrame bounds a single frame, so a corrupt length can't make the reader allocate gigabytes.
const maxFrame = 64 << 20

// FrameReader streams a snapshot's frames.
type FrameReader struct {
	// For telling whose backup it is.
	backupKey, backupID []byte
	src                 io.ReadCloser
	gz                  *gzip.Reader
	br                  *bufio.Reader
	info                *backuppb.BackupInfo
}

// progressReader reports how much of a file has been read.
type progressReader struct {
	r        io.Reader
	read     int64
	size     int64
	progress func(float64)
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	p.read += int64(n)
	if p.progress != nil && n > 0 {
		p.progress(float64(p.read) / float64(p.size))
	}
	return n, err
}

// Frames verifies and decrypts the snapshot's main file with the recovery key. progress gets the fraction (0..1) of
// the main file read so far.
func (a *Archive) Frames(recoveryKey string, progress func(float64)) (*FrameReader, error) {
	key, err := NormalizeRecoveryKey(recoveryKey)
	if err != nil {
		return nil, err
	}
	metaFile, mainFile := a.member(a.snapshot+"metadata"), a.member(a.snapshot+"main")
	raw, err := readAll(metaFile.Open)
	if err != nil {
		return nil, err
	}
	var meta backuppb.Metadata
	if err = proto.Unmarshal(raw, &meta); err != nil {
		return nil, invalid("the backup's metadata is unreadable")
	}
	if meta.Version != 1 {
		return nil, invalid("unsupported backup metadata version %d", meta.Version)
	}
	bk := backupKey(key)
	backupID, err := ctr(localMetadataKey(bk), meta.GetBackupId().GetIv(), meta.GetBackupId().GetEncryptedId())
	if err != nil {
		return nil, invalid("the backup's metadata is invalid: %v", err)
	}
	hmacKey, aesKey := messageBackupKeys(bk, backupID)

	head := make([]byte, len(remoteBackupMagic))
	if f, err := mainFile.Open(); err != nil {
		return nil, err
	} else {
		_, _ = io.ReadFull(f, head)
		_ = f.Close()
	}
	if bytes.Equal(head, remoteBackupMagic) {
		return nil, invalid("this is a Signal-hosted (cloud) backup, not an on-device backup")
	}
	size := int64(mainFile.UncompressedSize64)
	if err = verifyMAC(mainFile.Open, size, hmacKey); errors.Is(err, errBadMAC) {
		return nil, fmt.Errorf("%w: the recovery key doesn't match this backup", ErrWrongRecoveryKey)
	} else if err != nil {
		return nil, err
	}
	src, err := mainFile.Open()
	if err != nil {
		return nil, err
	}
	plain, err := newCBCReader(&progressReader{r: src, size: size, progress: progress}, size, aesKey)
	if err != nil {
		_ = src.Close()
		return nil, err
	}
	gz, err := gzip.NewReader(plain)
	if err != nil {
		_ = src.Close()
		return nil, invalid("the backup isn't compressed as expected: %v", err)
	}
	r := &FrameReader{src: src, gz: gz, br: bufio.NewReaderSize(gz, chunk), backupKey: bk, backupID: backupID}
	first, err := r.next()
	if err != nil {
		_ = r.Close()
		if errors.Is(err, io.EOF) {
			return nil, invalid("the backup is empty")
		}
		return nil, err
	}
	r.info = &backuppb.BackupInfo{}
	if err = proto.Unmarshal(first, r.info); err != nil {
		_ = r.Close()
		return nil, invalid("the backup's header is unreadable")
	}
	if r.info.Version != 1 {
		_ = r.Close()
		return nil, invalid("unsupported backup version %d", r.info.Version)
	}
	return r, nil
}

func readAll(open func() (io.ReadCloser, error)) ([]byte, error) {
	f, err := open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// ctr is AES-256-CTR with a 12-byte IV followed by a 32-bit counter starting at zero.
func ctr(key, iv, data []byte) ([]byte, error) {
	if len(iv) != 12 {
		return nil, fmt.Errorf("IV has %d bytes, not 12", len(iv))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(data))
	cipher.NewCTR(block, append(bytes.Clone(iv), 0, 0, 0, 0)).XORKeyStream(out, data)
	return out, nil
}

func (r *FrameReader) Info() *backuppb.BackupInfo {
	return r.info
}

// next reads one varint-length-delimited record: io.EOF at a clean end.
func (r *FrameReader) next() ([]byte, error) {
	length, err := binary.ReadUvarint(r.br)
	if errors.Is(err, io.EOF) {
		return nil, io.EOF
	} else if errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, invalid("backup ends in the middle of a record")
	} else if err != nil {
		return nil, fmt.Errorf("reading the backup: %w", err)
	}
	if length > maxFrame {
		return nil, invalid("backup has a record of %d bytes, too large to be real", length)
	}
	buf := make([]byte, length)
	if _, err = io.ReadFull(r.br, buf); errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, invalid("backup ends in the middle of a record")
	} else if err != nil {
		return nil, fmt.Errorf("reading the backup: %w", err)
	}
	return buf, nil
}

// Next returns the next frame, or io.EOF after the last one.
func (r *FrameReader) Next() (*backuppb.Frame, error) {
	raw, err := r.next()
	if err != nil {
		return nil, err
	}
	var f backuppb.Frame
	if err = proto.Unmarshal(raw, &f); err != nil {
		return nil, invalid("backup has an unreadable record: %v", err)
	}
	return &f, nil
}

func (r *FrameReader) Close() error {
	return errors.Join(r.gz.Close(), r.src.Close())
}

// IsFor reports whether the backup was made by the account with this ACI: the apps derive a backup's ID from the
// account's recovery key and ACI.
func (r *FrameReader) IsFor(aci [16]byte) bool {
	return hmac.Equal(r.backupID, backupIDFromACI(r.backupKey, aci))
}
