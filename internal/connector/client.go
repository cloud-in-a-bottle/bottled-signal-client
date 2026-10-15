package connector

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// Client is the bridge's connection to the account through signal-cli.
type Client struct {
	c     *Connector
	login *bridgev2.UserLogin
	acct  *signalcli.Account
	// The account's ACI, which is also the login's ID.
	aci string

	mu sync.Mutex
	// Stops the event stream; nil while disconnected.
	stop context.CancelFunc
}

var (
	_ bridgev2.NetworkAPI                         = (*Client)(nil)
	_ bridgev2.EditHandlingNetworkAPI             = (*Client)(nil)
	_ bridgev2.ReactionHandlingNetworkAPI         = (*Client)(nil)
	_ bridgev2.RedactionHandlingNetworkAPI        = (*Client)(nil)
	_ bridgev2.ReadReceiptHandlingNetworkAPI      = (*Client)(nil)
	_ bridgev2.TypingHandlingNetworkAPI           = (*Client)(nil)
	_ bridgev2.ViewLimitedMediaHandlingNetworkAPI = (*Client)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI      = (*Client)(nil)
	_ bridgev2.GhostDMCreatingNetworkAPI          = (*Client)(nil)
	_ bridgev2.ContactListingNetworkAPI           = (*Client)(nil)
	_ bridgev2.UserSearchingNetworkAPI            = (*Client)(nil)
)

func newClient(c *Connector, login *bridgev2.UserLogin, acct *signalcli.Account) *Client {
	return &Client{c: c, login: login, acct: acct, aci: string(login.ID)}
}

// Account is the account's signal-cli calls, for the app's own features (profile, linked devices, ...).
func (cl *Client) Account() *signalcli.Account {
	return cl.acct
}

// ACI is the account's own ACI.
func (cl *Client) ACI() string {
	return cl.aci
}

func (cl *Client) Connect(ctx context.Context) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.stop != nil {
		return
	}
	ctx, cl.stop = context.WithCancel(context.WithoutCancel(ctx))
	go cl.run(ctx)
}

func (cl *Client) Disconnect() {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if cl.stop != nil {
		cl.stop()
		cl.stop = nil
	}
}

func (cl *Client) run(ctx context.Context) {
	log := cl.login.Log.With().Str("component", "signal").Logger()
	ctx = log.WithContext(ctx)
	cl.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnecting})
	if err := cl.c.RPC.WaitUp(ctx); err != nil {
		return
	}
	var accounts []signalcli.ListedAccount
	for {
		var err error
		if accounts, err = cl.c.RPC.ListAccounts(ctx); err == nil {
			break
		}
		// Eg the daemon restarting just after it answered.
		cl.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateUnknownError, Message: err.Error()})
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Second):
		}
	}
	found := false
	for _, a := range accounts {
		found = found || a.Number == cl.acct.Number
	}
	if !found {
		cl.login.BridgeState.Send(status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Message:    "signal-cli no longer has " + cl.acct.Number + "; register it again",
		})
		return
	}
	synced := false
	cl.c.RPC.FollowEvents(ctx, func(r signalcli.Receive) {
		cl.handleReceive(ctx, r)
	}, func(err error) {
		if err != nil {
			cl.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateTransientDisconnect, Message: err.Error()})
			return
		}
		cl.login.BridgeState.Send(status.BridgeState{StateEvent: status.StateConnected})
		if !synced {
			synced = true
			go cl.syncChats(ctx)
		}
	}, log)
}

// syncChats brings the groups the account is in, and everyone's names, up to date: signal-cli has them from its own
// syncs with Signal's servers, which it does without being asked.
func (cl *Client) syncChats(ctx context.Context) {
	log := zerolog.Ctx(ctx)
	if err := cl.refreshProfile(ctx); err != nil {
		log.Err(err).Msg("Failed to refresh own profile")
	}
	groups, err := cl.acct.Groups(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list groups")
		return
	}
	for _, g := range groups {
		if !g.IsMember || g.IsBlocked || !signalid.IsGroupID(g.ID) {
			continue
		}
		cl.login.QueueRemoteEvent(&simplevent.ChatResync{
			EventMeta: simplevent.EventMeta{
				Type: bridgev2.RemoteEventChatResync, PortalKey: signalid.GroupPortalKey(g.ID), CreatePortal: true,
			},
			ChatInfo: cl.groupInfo(ctx, g),
		})
	}
	contacts, err := cl.acct.Contacts(ctx)
	if err != nil {
		log.Err(err).Msg("Failed to list contacts")
		return
	}
	for _, contact := range contacts {
		if !signalid.IsACI(contact.UUID) || contact.UUID == cl.aci {
			continue
		}
		ghost, err := cl.c.br.GetExistingGhostByID(ctx, signalid.UserID(contact.UUID))
		if err != nil {
			log.Err(err).Msg("Failed to get ghost")
		} else if ghost != nil {
			ghost.UpdateInfo(ctx, cl.userInfo(ctx, contact))
		}
	}
}

// refreshProfile shows the account's profile name as the login's name.
func (cl *Client) refreshProfile(ctx context.Context) error {
	contacts, err := cl.acct.Contacts(ctx, cl.acct.Number)
	if err != nil {
		return err
	}
	name := cl.acct.Number
	if len(contacts) == 1 && contacts[0].Profile != nil {
		if n := joinName(contacts[0].Profile.GivenName, contacts[0].Profile.FamilyName); n != "" {
			name = n
		}
	}
	if cl.login.RemoteName == name {
		return nil
	}
	cl.login.RemoteName = name
	cl.login.RemoteProfile.Name = name
	return cl.login.Save(ctx)
}

func (cl *Client) IsLoggedIn() bool {
	return true
}

// LogoutRemote leaves signal-cli's account alone: it's the number's primary device, and unregistering it would cut
// the number off from Signal. Logging in again (the "existing" flow) picks it back up.
func (cl *Client) LogoutRemote(ctx context.Context) {
	cl.Disconnect()
}

func (cl *Client) IsThisUser(ctx context.Context, userID networkid.UserID) bool {
	return string(userID) == cl.aci
}

func (cl *Client) sender(aci string) bridgev2.EventSender {
	return bridgev2.EventSender{
		IsFromMe:    aci == cl.aci,
		SenderLogin: signalid.LoginID(aci),
		Sender:      signalid.UserID(aci),
	}
}

// target is where a portal's messages go on Signal.
func (cl *Client) target(portalID networkid.PortalID) (signalcli.Target, error) {
	chat, err := signalid.ParsePortalID(portalID)
	switch {
	case err != nil:
		return signalcli.Target{}, err
	case chat.GroupID != "":
		return signalcli.Target{GroupID: chat.GroupID}, nil
	case chat.ACI == cl.aci:
		return signalcli.Target{NoteToSelf: true}, nil
	}
	return signalcli.Target{Recipient: chat.ACI}, nil
}

// checkSent turns a send that reached nobody into an error. A group send that reached some members is a success, as
// in Signal's apps (the others are retried by Signal's own mechanisms when they come back).
func (cl *Client) checkSent(ctx context.Context, res signalcli.SendResult) error {
	var failures []string
	for _, r := range res.Results {
		if r.Type != "SUCCESS" {
			failures = append(failures, fmt.Sprintf("%s: %s", r.RecipientAddress.UUID, r.Type))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	zerolog.Ctx(ctx).Warn().Strs("failures", failures).Int("recipients", len(res.Results)).Msg("Send failed for some recipients")
	if len(failures) == len(res.Results) {
		reason := strings.ToLower(strings.ReplaceAll(strings.TrimSuffix(res.Results[0].Type, "_FAILURE"), "_", " "))
		return fmt.Errorf("couldn't deliver the message (%s)", reason)
	}
	return nil
}

func joinName(parts ...string) string {
	var nonEmpty []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	return strings.Join(nonEmpty, " ")
}

func ms(ts int64) time.Time {
	return time.UnixMilli(ts)
}

// SetProfileName changes the account's profile name, which others see.
func (cl *Client) SetProfileName(ctx context.Context, given, family string) error {
	if err := cl.acct.UpdateProfile(ctx, given, family); err != nil {
		return err
	}
	return cl.refreshProfile(ctx)
}
