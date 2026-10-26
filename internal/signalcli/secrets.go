package signalcli

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	// Registers the sqlite3 driver.
	_ "github.com/mattn/go-sqlite3"
)

// Secrets are keys signal-cli keeps for the account that its JSON-RPC API doesn't expose, needed to write backups a
// phone can restore.
type Secrets struct {
	ACI string
	// The 64-character Account Entropy Pool: the account's recovery key.
	RecoveryKey        string
	ProfileKey         []byte
	MediaRootBackupKey []byte
}

// ErrSecretsUnavailable means signal-cli doesn't (yet) have the account's keys.
var ErrSecretsUnavailable = errors.New("signal-cli doesn't have the account's keys")

type accountsFile struct {
	Accounts []struct {
		Number string `json:"number"`
		Path   string `json:"path"`
	} `json:"accounts"`
}

type accountFile struct {
	AccountEntropyPool string `json:"accountEntropyPool"`
	ProfileKey         string `json:"profileKey"`
	MediaRootBackupKey string `json:"mediaRootBackupKey"`
	ACIAccountData     struct {
		ServiceID string `json:"serviceId"`
	} `json:"aciAccountData"`
}

// accountPath finds the account's file: `<data-dir>/data/accounts.json` maps numbers to files in the same directory
// (signal-cli's AccountsStore and SignalAccount.Storage).
func accountPath(dataDir, number string) (string, error) {
	dir := filepath.Join(dataDir, "data")
	raw, err := os.ReadFile(filepath.Join(dir, "accounts.json"))
	if err != nil {
		return "", err
	}
	var accounts accountsFile
	if err = json.Unmarshal(raw, &accounts); err != nil {
		return "", fmt.Errorf("reading signal-cli's accounts.json: %w", err)
	}
	var found []string
	for _, a := range accounts.Accounts {
		if a.Number == number {
			found = append(found, a.Path)
		}
	}
	if len(found) != 1 {
		return "", fmt.Errorf("%w: signal-cli has %d accounts for %s", ErrSecretsUnavailable, len(found), number)
	}
	return filepath.Join(dir, found[0]), nil
}

// ReadSecrets reads the account's keys from signal-cli's own storage.
func ReadSecrets(dataDir, number string) (Secrets, error) {
	path, err := accountPath(dataDir, number)
	if err != nil {
		return Secrets{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Secrets{}, err
	}
	var account accountFile
	if err = json.Unmarshal(raw, &account); err != nil {
		return Secrets{}, fmt.Errorf("reading signal-cli's account file: %w", err)
	}
	if account.AccountEntropyPool == "" {
		return Secrets{}, fmt.Errorf("%w: it hasn't created a recovery key yet (it does on its first storage sync, "+
			"shortly after registering); try again in a minute", ErrSecretsUnavailable)
	}
	if account.ProfileKey == "" || account.ACIAccountData.ServiceID == "" {
		return Secrets{}, fmt.Errorf("%w: its account file has no profile key or ACI", ErrSecretsUnavailable)
	}
	s := Secrets{ACI: account.ACIAccountData.ServiceID, RecoveryKey: account.AccountEntropyPool}
	if s.ProfileKey, err = base64.StdEncoding.DecodeString(account.ProfileKey); err != nil {
		return Secrets{}, fmt.Errorf("decoding the profile key: %w", err)
	}
	if account.MediaRootBackupKey != "" {
		if s.MediaRootBackupKey, err = base64.StdEncoding.DecodeString(account.MediaRootBackupKey); err != nil {
			return Secrets{}, fmt.Errorf("decoding the media root backup key: %w", err)
		}
	}
	return s, nil
}

// GroupMasterKeys reads each group's master key, by base64 group ID, from signal-cli's account database. (Its API
// doesn't expose them.)
func GroupMasterKeys(dataDir, number string) (map[string][]byte, error) {
	path, err := accountPath(dataDir, number)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", "file:"+filepath.Join(path+".d", "account.db")+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query("SELECT group_id, master_key FROM group_v2")
	if err != nil {
		return nil, fmt.Errorf("reading signal-cli's groups: %w", err)
	}
	defer rows.Close()
	keys := make(map[string][]byte)
	for rows.Next() {
		var groupID, masterKey []byte
		if err = rows.Scan(&groupID, &masterKey); err != nil {
			return nil, err
		}
		keys[base64.StdEncoding.EncodeToString(groupID)] = masterKey
	}
	return keys, rows.Err()
}
