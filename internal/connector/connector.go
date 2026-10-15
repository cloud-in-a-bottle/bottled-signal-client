// Package connector is a bridgev2 network connector for Signal that drives signal-cli, which is the account's primary
// device: it registers the number, and keeps the keys and sessions. signal-cli keeps no message history, so the bridge
// (and the store behind it) is the only copy of the messages.
package connector

import (
	"context"
	"sync"

	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// Signal's own limit for attachments is 100MB.
const MaxFileSize = 100 * 1024 * 1024

type Connector struct {
	br  *bridgev2.Bridge
	RPC *signalcli.Client
	// The signal-cli binary and its data directory, for the commands that don't go through the daemon.
	Binary  string
	DataDir string
	// Where outgoing attachments are written for signal-cli to read.
	TempDir string

	// Logins in progress; the daemon may only have one registration pending per number.
	loginMu sync.Mutex
}

var (
	_ bridgev2.NetworkConnector            = (*Connector)(nil)
	_ bridgev2.IdentifierValidatingNetwork = (*Connector)(nil)
)

func New(rpc *signalcli.Client, binary, dataDir, tempDir string) *Connector {
	return &Connector{RPC: rpc, Binary: binary, DataDir: dataDir, TempDir: tempDir}
}

func (c *Connector) Init(br *bridgev2.Bridge) {
	c.br = br
}

func (c *Connector) Start(ctx context.Context) error {
	return nil
}

func (c *Connector) GetName() bridgev2.BridgeName {
	return bridgev2.BridgeName{
		DisplayName:      "Signal",
		NetworkURL:       "https://signal.org",
		NetworkID:        "signal",
		BeeperBridgeType: "github.com/cloud-in-a-bottle/bottled-signal-client",
	}
}

// UserLoginMetadata is what the bridge keeps for the account.
type UserLoginMetadata struct {
	// The phone number signal-cli knows the account by.
	Number string `json:"number"`
}

// PortalMetadata is what the bridge keeps for a chat.
type PortalMetadata struct {
	// A group's master key (base64), which a backup needs to restore the group. From signal-cli, or from an imported
	// backup for groups signal-cli doesn't know (eg ones left before this server took over).
	MasterKey string `json:"master_key,omitempty"`
}

func (c *Connector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal:    func() any { return &PortalMetadata{} },
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

func (c *Connector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{
		DisappearingMessages: true,
		// Signal apps mark a chat read when you send in it.
		ImplicitReadReceipts: true,
	}
}

func (c *Connector) GetConfig() (string, any, configupgrade.Upgrader) {
	return "", nil, configupgrade.NoopUpgrader
}

func (c *Connector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}

func (c *Connector) ValidateUserID(id networkid.UserID) bool {
	return signalid.IsACI(string(id))
}

func (c *Connector) LoadUserLogin(ctx context.Context, login *bridgev2.UserLogin) error {
	meta := login.Metadata.(*UserLoginMetadata)
	login.Client = newClient(c, login, c.RPC.Account(meta.Number))
	return nil
}
