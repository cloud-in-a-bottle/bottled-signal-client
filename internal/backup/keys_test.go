package backup

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Public test vector from libsignal (rust/message-backup/src/key.rs), not a real account's key.
const testRecoveryKey = "dtjs858asj6tv0jzsqrsmj0ubp335pisj98e9ssnss8myoc08drhtcktyawvx45l" // gitleaks:allow

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestKeyDerivationMatchesLibsignalVectors(t *testing.T) {
	var aci [16]byte
	copy(aci[:], mustHex(t, "659aa5f4a28dfcc11ea1b997537a3d95"))
	hmacKey, aesKey := messageBackupKeys(backupKey(testRecoveryKey), backupIDFromACI(backupKey(testRecoveryKey), aci))
	if got := hex.EncodeToString(hmacKey); got != "f425e22a607c529717e1e1b29f9fe139f9d1c7e7d01e371c7753c544a3026649" {
		t.Errorf("hmac key %s", got)
	}
	if got := hex.EncodeToString(aesKey); got != "e143f4ad5668d8bfed2f88562f0693f53bda2c0e55c9d71730f30e24695fd6df" {
		t.Errorf("aes key %s", got)
	}
}

func TestGroupIDMatchesZkgroupVector(t *testing.T) {
	// node/ts/test/ZKGroup-test.ts "has a string representation for GroupIdentifier".
	id := GroupIDFromMasterKey(mustHex(t, "6465666768696a6b6c6d6e6f707172737475767778797a7b7c7d7e7f80818283"))
	if got := base64.StdEncoding.EncodeToString(id); got != "hOJWcwVI+LoJBpsiPszBM/WZ+YJ+3HCE+JIeSnDNnkw=" {
		t.Errorf("group ID %s", got)
	}
}

func TestRecoveryKeyNormalization(t *testing.T) {
	var groups []string
	upper := strings.ToUpper(testRecoveryKey)
	for i := 0; i < 64; i += 4 {
		groups = append(groups, upper[i:i+4])
	}
	if got, err := NormalizeRecoveryKey(strings.Join(groups, " ")); err != nil || got != testRecoveryKey {
		t.Errorf("spaced key: %q, %v", got, err)
	}
	if _, err := NormalizeRecoveryKey("too short"); !errors.Is(err, ErrWrongRecoveryKey) {
		t.Errorf("short key: %v", err)
	}
	// iOS shows "0" as "=" and "O" as "#" (vectors from Signal-iOS DisplayableAccountEntropyPoolTest).
	raw := "0o0ooaa" + strings.Repeat("a", 57)
	for _, shown := range []string{"=#=##AA" + strings.Repeat("A", 57), "=#0oOaA" + strings.Repeat("a", 57)} {
		if got, err := NormalizeRecoveryKey(shown); err != nil || got != raw {
			t.Errorf("%s: %q, %v", shown, got, err)
		}
	}
}
