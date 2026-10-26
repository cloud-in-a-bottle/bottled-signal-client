package signaltest

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/app"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// The account and the people it knows: Alice and Bob, who are with it in the group "Weekend plans".
const (
	SelfACI    = "11111111-1111-4111-8111-111111111111"
	SelfNumber = "+15550000001"
	AliceACI   = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	AliceNum   = "+15550000002"
	BobACI     = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
)

// The group's master key, and its ID as signal-cli writes it.
var (
	GroupMasterKey = []byte("0123456789abcdef0123456789abcdef")
	GroupID        = base64.StdEncoding.EncodeToString(backup.GroupIDFromMasterKey(GroupMasterKey))
)

// RecoveryKey is the account's recovery key (Account Entropy Pool), once WriteAccountFiles has written it.
const RecoveryKey = "abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmnopqrstuvwxyz01"

type Harness struct {
	T      *testing.T
	Ctx    context.Context
	Daemon *Daemon
	Svc    *bridge.Service
	Store  *store.Store
	Files  *files.Store
	Broker *events.Broker
	Conn   *connector.Connector
	Login  *bridgev2.UserLogin
	// Laid out like the app's: data/signal-cli, data/attachments, temp, sqlite.
	Dir       string
	SignalDir string
}

// New starts the bridge against a fake signal-cli that has the account registered, and logs in to it.
func New(t *testing.T) *Harness {
	h := Start(t)
	h.LogIn()
	return h
}

// Start starts the bridge against a fake signal-cli that has the account registered, without logging in.
func Start(t *testing.T) *Harness {
	return StartWith(t, nil)
}

// StartWith is Start, with prepare called on the paths of the main database and the attachments folder before the
// store opens them.
func StartWith(t *testing.T, prepare func(mainDB, attachments string)) *Harness {
	return StartIn(t, t.TempDir(), prepare)
}

// StartIn is StartWith, with the data in dir (laid out as `just run` expects).
func StartIn(t *testing.T, dir string, prepare func(mainDB, attachments string)) *Harness {
	d := NewDaemon(t)
	d.Accounts = []signalcli.ListedAccount{{Number: SelfNumber, ACI: SelfACI}}
	d.Contacts = []signalcli.Contact{
		{UUID: SelfACI, Number: SelfNumber, Profile: &signalcli.ContactProfile{GivenName: "Me", FamilyName: "Myself"}},
		{UUID: AliceACI, Number: AliceNum, Profile: &signalcli.ContactProfile{GivenName: "Alice"}},
		{UUID: BobACI, Profile: &signalcli.ContactProfile{GivenName: "Bob"}},
	}
	d.Groups = []signalcli.Group{{
		ID: GroupID, Name: "Weekend plans", IsMember: true,
		Members: []signalcli.GroupMember{{UUID: SelfACI, IsAdmin: true}, {UUID: AliceACI}, {UUID: BobACI}},
	}}
	signalDir := filepath.Join(dir, "data", "signal-cli")
	tempDir := filepath.Join(dir, "temp")
	for _, sub := range []string{filepath.Join(signalDir, "attachments"), tempDir, filepath.Join(dir, "sqlite")} {
		if err := os.MkdirAll(sub, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	log := zerolog.New(zerolog.NewTestWriter(t)).Level(zerolog.InfoLevel)
	if prepare != nil {
		prepare(filepath.Join(dir, "sqlite", "main.db"), filepath.Join(dir, "data", "attachments"))
	}
	st, err := store.Open(filepath.Join(dir, "sqlite", "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs, err := files.New(filepath.Join(dir, "data", "attachments"))
	if err != nil {
		t.Fatal(err)
	}
	broker := events.NewBroker()
	conn := connector.New(signalcli.NewClient(d.URL()), "signal-cli-not-used-in-tests", signalDir, tempDir)
	svc, err := bridge.New(app.BridgeOptions(conn), filepath.Join(dir, "sqlite", "bridge.db"), st, fs, broker, log)
	if err != nil {
		t.Fatal(err)
	}
	ctx := log.WithContext(context.Background())
	if err = svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Stop)
	return &Harness{
		T: t, Ctx: ctx, Daemon: d, Svc: svc, Store: st, Files: fs, Broker: broker, Conn: conn, Dir: dir,
		SignalDir: signalDir,
	}
}

// LogIn logs in to the account signal-cli has, and waits for the connection.
func (h *Harness) LogIn() {
	h.T.Helper()
	step, err := h.Svc.StartLogin(h.Ctx, connector.FlowExisting)
	if err != nil || step.Type != bridgev2.LoginStepTypeComplete {
		h.T.Fatalf("login: %+v, %v", step, err)
	}
	h.Login = step.CompleteParams.UserLogin
	h.Eventually("connected", func() bool { return h.Svc.Account(h.Ctx).State == "CONNECTED" })
	// The first sync (the profile, the groups) runs once connected.
	h.Eventually("the group from the first sync", func() bool {
		_, err := h.Store.ConversationByRoom(h.Ctx, string(localmx.RoomIDFor(signalid.GroupPortalKey(GroupID))))
		return err == nil
	})
}

// Eventually waits for check to hold.
func (h *Harness) Eventually(what string, check func() bool) {
	h.T.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			h.T.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func DMKey(aci string) networkid.PortalKey {
	return signalid.DMPortalKey(aci, signalid.LoginID(SelfACI))
}

func GroupKey() networkid.PortalKey {
	return signalid.GroupPortalKey(GroupID)
}

// ConversationID is the conversation of a chat, which must exist.
func (h *Harness) ConversationID(key networkid.PortalKey) int64 {
	h.T.Helper()
	id, err := h.Store.ConversationByRoom(h.Ctx, string(localmx.RoomIDFor(key)))
	if err != nil {
		h.T.Fatalf("conversation for %v: %v", key, err)
	}
	return id
}

// Messages are a chat's messages, oldest first, or none if the chat doesn't exist.
func (h *Harness) Messages(key networkid.PortalKey) []store.Message {
	h.T.Helper()
	id, err := h.Store.ConversationByRoom(h.Ctx, string(localmx.RoomIDFor(key)))
	if err != nil {
		return nil
	}
	page, err := h.Store.MessagesPage(h.Ctx, id, nil, 500)
	if err != nil {
		h.T.Fatal(err)
	}
	return page.Messages
}

// WaitForMessages waits until a chat has n messages, and returns them.
func (h *Harness) WaitForMessages(key networkid.PortalKey, n int) []store.Message {
	h.T.Helper()
	var msgs []store.Message
	h.Eventually("messages", func() bool {
		msgs = h.Messages(key)
		return len(msgs) == n
	})
	return msgs
}

// WaitFor waits until check accepts a chat's messages.
func (h *Harness) WaitFor(key networkid.PortalKey, check func([]store.Message) bool) []store.Message {
	h.T.Helper()
	var msgs []store.Message
	deadline := time.Now().Add(5 * time.Second)
	for {
		msgs = h.Messages(key)
		if check(msgs) {
			return msgs
		}
		if time.Now().After(deadline) {
			h.T.Fatalf("timed out; messages: %+v", msgs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Push delivers an envelope from signal-cli.
func (h *Harness) Push(env *signalcli.Envelope) {
	h.Daemon.Push(signalcli.Receive{Account: SelfNumber, Envelope: env})
}

// Text is a text message from someone, one-to-one, or in a group if groupID is set.
func Text(from string, ts int64, body, groupID string) *signalcli.Envelope {
	d := &signalcli.DataMessage{Timestamp: ts, Message: &body}
	if groupID != "" {
		d.GroupInfo = &signalcli.GroupInfo{GroupID: groupID, Type: "DELIVER"}
	}
	return &signalcli.Envelope{SourceUUID: from, SourceDevice: 1, Timestamp: ts, DataMessage: d}
}
