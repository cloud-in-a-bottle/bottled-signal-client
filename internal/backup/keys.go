// Package backup reads and writes Signal's local ("on-device") backups, the format shared by iOS, Android and Desktop
// since 8.30: a SignalBackups folder holding snapshots (an encrypted, gzipped stream of protobuf frames, plus metadata)
// and a shared files/ folder of encrypted attachments.
//
// Constants and layouts follow libsignal (rust/account-keys/src/backup.rs, rust/message-backup/src/key.rs and
// frame.rs) and the apps' local-backup code; the tests check them against libsignal's published vectors.
package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"

	"golang.org/x/crypto/hkdf"
)

// ErrWrongRecoveryKey means the key is malformed or doesn't decrypt the backup; the same upload can be retried with
// another key.
var ErrWrongRecoveryKey = errors.New("wrong recovery key")

// InvalidBackupError is a problem with the uploaded file itself.
type InvalidBackupError struct{ msg string }

func (e InvalidBackupError) Error() string { return e.msg }

func invalid(format string, args ...any) error {
	return InvalidBackupError{msg: fmt.Sprintf(format, args...)}
}

func hkdfBytes(ikm, info []byte, length int) []byte {
	out := make([]byte, length)
	if _, err := io.ReadFull(hkdf.New(sha256.New, ikm, nil, info), out); err != nil {
		panic(err)
	}
	return out
}

// NormalizeRecoveryKey accepts the key as either app shows it. iOS displays it uppercase with "0" as "=" and "O" as
// "#" so they can't be confused (Signal-iOS DisplayableAccountEntropyPool); the key itself is lowercase letters and
// digits.
func NormalizeRecoveryKey(text string) (string, error) {
	key := strings.Join(strings.Fields(text), "")
	key = strings.NewReplacer("-", "", "=", "0", "#", "o").Replace(key)
	key = strings.ToLower(key)
	if len(key) != 64 || strings.Trim(key, "abcdefghijklmnopqrstuvwxyz0123456789") != "" {
		return "", fmt.Errorf("%w: the recovery key should be 64 letters and digits", ErrWrongRecoveryKey)
	}
	return key, nil
}

func backupKey(recoveryKey string) []byte {
	return hkdfBytes([]byte(recoveryKey), []byte("20240801_SIGNAL_BACKUP_KEY"), 32)
}

func backupIDFromACI(bk []byte, aci [16]byte) []byte {
	return hkdfBytes(bk, append([]byte("20241024_SIGNAL_BACKUP_ID:"), aci[:]...), 16)
}

func localMetadataKey(bk []byte) []byte {
	return hkdfBytes(bk, []byte("20241011_SIGNAL_LOCAL_BACKUP_METADATA_KEY"), 32)
}

// messageBackupKeys are the main file's (hmac, aes) keys.
func messageBackupKeys(bk, backupID []byte) (hmacKey, aesKey []byte) {
	k := hkdfBytes(bk, append([]byte("20241007_SIGNAL_BACKUP_ENCRYPT_MESSAGE_BACKUP:"), backupID...), 64)
	return k[:32], k[32:]
}

// LocalMediaName is an attachment's file name in files/: the hex SHA-256 of its plaintext hash and local key.
func LocalMediaName(plaintextHash, localKey []byte) string {
	h := sha256.New()
	h.Write(plaintextHash)
	h.Write(localKey)
	return hex.EncodeToString(h.Sum(nil))
}

// shoSqueeze is libsignal poksho's ShoHmacSha256: new(label), absorb_and_ratchet(data), squeeze(length).
func shoSqueeze(label, data []byte, length int) []byte {
	absorbAndRatchet := func(cv, input []byte) []byte {
		mac := hmac.New(sha256.New, cv)
		mac.Write(input)
		mac.Write([]byte{0})
		return mac.Sum(nil)
	}
	cv := absorbAndRatchet(make([]byte, 32), label)
	cv = absorbAndRatchet(cv, data)
	var out []byte
	for i := uint64(0); len(out) < length; i++ {
		mac := hmac.New(sha256.New, cv)
		mac.Write(binary.BigEndian.AppendUint64(nil, i))
		mac.Write([]byte{1})
		out = mac.Sum(out)
	}
	return out[:length]
}

// GroupIDFromMasterKey is the group identifier (as signal-cli reports it, base64) of a group, derived like zkgroup's
// GroupSecretParams.
func GroupIDFromMasterKey(masterKey []byte) []byte {
	return shoSqueeze([]byte("Signal_ZKGroup_20200424_GroupMasterKey_GroupSecretParams_DeriveFromMasterKey"), masterKey, 32)
}
