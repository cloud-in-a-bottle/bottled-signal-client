package signaltest

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"

	// Registers the sqlite3 driver.
	_ "github.com/mattn/go-sqlite3"
)

// WriteAccountFiles writes signal-cli's own files for the account, with the keys a backup needs (signal-cli's
// JSON-RPC API doesn't give them; the exporter reads them from these): its recovery key, profile key and group keys.
func (h *Harness) WriteAccountFiles() {
	h.T.Helper()
	dir := filepath.Join(h.SignalDir, "data")
	if err := os.MkdirAll(filepath.Join(dir, "123456.d"), 0o700); err != nil {
		h.T.Fatal(err)
	}
	write := func(name string, v any) {
		data, err := json.Marshal(v)
		if err != nil {
			h.T.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			h.T.Fatal(err)
		}
	}
	write("accounts.json", map[string]any{"accounts": []map[string]string{{"number": SelfNumber, "path": "123456"}}})
	write("123456", map[string]any{
		"accountEntropyPool": RecoveryKey,
		"profileKey":         base64.StdEncoding.EncodeToString([]byte("profile key profile key profile.")),
		"aciAccountData":     map[string]string{"serviceId": SelfACI},
	})
	db, err := sql.Open("sqlite3", filepath.Join(dir, "123456.d", "account.db"))
	if err != nil {
		h.T.Fatal(err)
	}
	defer db.Close()
	groupID, err := base64.StdEncoding.DecodeString(GroupID)
	if err != nil {
		h.T.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE group_v2 (group_id BLOB, master_key BLOB)"); err != nil {
		h.T.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO group_v2 VALUES (?, ?)", groupID, GroupMasterKey); err != nil {
		h.T.Fatal(err)
	}
}
