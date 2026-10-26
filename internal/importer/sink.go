package importer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// Messages are imported a chat's batch at a time.
const batchSize = 200

// Sink is where imported history goes: into chats made as the bridge would make them, with messages stored as if
// they'd arrived from Signal. Backups and the previous app's database both import through it.
type Sink struct {
	svc    *bridge.Service
	st     *store.Store
	broker *events.Broker
	login  *bridgev2.UserLogin
	// The account's own ACI.
	Self   string
	Counts Counts
	// Called after each batch, eg to report progress.
	OnBatch func()
	// Ghosts already named.
	named map[string]bool
}

// Chat is a chat history is imported into.
type Chat struct {
	Key networkid.PortalKey
	// Describes the chat for making it here if signal-cli can't (eg a group left long ago).
	Fallback *bridgev2.ChatInfo
	Pinned   bool
	Archived bool
	// Unix milliseconds, -1 for muted until unmuted, or 0.
	MutedUntil int64
	// For groups: the master key, which exported backups need.
	MasterKey []byte

	conversationID int64
	failed         bool
	pending        []*bridge.ImportedMessage
}

func NewSink(ctx context.Context, svc *bridge.Service, st *store.Store, broker *events.Broker) (*Sink, error) {
	login := svc.CurrentLogin(ctx)
	if login == nil {
		return nil, ErrNotLoggedIn
	}
	return &Sink{svc: svc, st: st, broker: broker, login: login, Self: string(login.ID), named: make(map[string]bool)}, nil
}

func (s *Sink) Sender(aci string) bridgev2.EventSender {
	return bridgev2.EventSender{IsFromMe: aci == s.Self, Sender: signalid.UserID(aci), SenderLogin: signalid.LoginID(aci)}
}

// DMKey is the key of the one-to-one chat with aci.
func (s *Sink) DMKey(aci string) networkid.PortalKey {
	return signalid.DMPortalKey(aci, s.login.ID)
}

// SelfName is the account's own name, for mentions of it.
func (s *Sink) SelfName() string {
	return s.login.RemoteName
}

// GhostMXID is a person's Matrix ID on the bridge, for links to them in formatted text.
func (s *Sink) GhostMXID(aci string) id.UserID {
	return s.svc.Bridge().Matrix.FormatGhostMXID(signalid.UserID(aci))
}

// ensure makes the chat here if it isn't yet, as it was where the history comes from. It reports false if that
// failed: the chat's history is left out.
func (s *Sink) ensure(ctx context.Context, c *Chat) bool {
	if c.conversationID != 0 || c.failed {
		return !c.failed
	}
	existing, err := s.svc.Bridge().GetExistingPortalByKey(ctx, c.Key)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to look up a chat")
		c.failed = true
		return false
	}
	// A chat that's here already keeps its settings (they may have changed since the backup).
	isNew := existing == nil || existing.MXID == ""
	conversationID, err := s.svc.ImportChat(ctx, c.Key, c.Fallback)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("chat", string(c.Key.ID)).Msg("Couldn't make a chat for imported history; leaving it out")
		c.failed = true
		return false
	}
	c.conversationID = conversationID
	s.Counts.Chats++
	if len(c.MasterKey) == 32 {
		if err = s.saveMasterKey(ctx, c); err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to save the group's master key")
		}
	}
	if !isNew {
		return true
	}
	res, err := s.st.Tx(ctx, func(tx *store.Tx) error {
		if c.Pinned {
			if err := tx.SetConversationPinned(conversationID, true); err != nil {
				return err
			}
		}
		if c.Archived {
			if err := tx.SetConversationArchived(conversationID, true); err != nil {
				return err
			}
		}
		if c.MutedUntil != 0 {
			return tx.SetConversationMutedUntil(conversationID, c.MutedUntil)
		}
		return nil
	})
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to apply the chat's settings")
	}
	s.publish(res)
	return true
}

func (s *Sink) saveMasterKey(ctx context.Context, c *Chat) error {
	portal, err := s.svc.Bridge().GetExistingPortalByKey(ctx, c.Key)
	if err != nil || portal == nil {
		return err
	}
	meta := portal.Metadata.(*connector.PortalMetadata)
	if meta.MasterKey != "" {
		return nil
	}
	meta.MasterKey = base64.StdEncoding.EncodeToString(c.MasterKey)
	return portal.Save(ctx)
}

// NameGhost names someone, unless they have a name here already (which is newer).
func (s *Sink) NameGhost(ctx context.Context, aci, name string) {
	if name == "" || aci == s.Self || s.named[aci] {
		return
	}
	s.named[aci] = true
	ghost, err := s.svc.Bridge().GetGhostByID(ctx, signalid.UserID(aci))
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to get ghost")
		return
	}
	if ghost.Name == "" {
		ghost.UpdateInfo(ctx, &bridgev2.UserInfo{Name: ptr.Ptr(name)})
	}
}

// Exists reports whether the message aci sent at any of these times is here already. Checking before converting a
// message saves decrypting its attachments for nothing; importing checks again.
func (s *Sink) Exists(ctx context.Context, c *Chat, aci string, sentAt ...int64) (bool, error) {
	for _, ts := range sentAt {
		existing, err := s.svc.Bridge().DB.Message.GetFirstPartByID(ctx, c.Key.Receiver, signalid.MessageID(aci, ts))
		if err != nil || existing != nil {
			return existing != nil, err
		}
	}
	return false, nil
}

// Add queues a message, importing the chat's queue once it's a batch.
func (s *Sink) Add(ctx context.Context, c *Chat, msg *bridge.ImportedMessage) error {
	c.pending = append(c.pending, msg)
	if len(c.pending) >= batchSize {
		return s.Flush(ctx, c)
	}
	return nil
}

// Flush imports the chat's queued messages.
func (s *Sink) Flush(ctx context.Context, c *Chat) error {
	if len(c.pending) == 0 {
		return nil
	}
	msgs := c.pending
	c.pending = nil
	if !s.ensure(ctx, c) {
		return nil
	}
	counts, err := s.svc.ImportMessages(ctx, c.Key, msgs)
	if err != nil {
		return fmt.Errorf("importing into chat %s: %w", c.Key.ID, err)
	}
	s.Counts.Messages += counts.Imported
	s.Counts.Skipped += counts.Skipped
	if s.OnBatch != nil {
		s.OnBatch()
	}
	return nil
}

// Update is a chat update (a group change, a call, a safety number change, ...) as a line in the timeline.
type Update struct {
	// Who did it; empty for a line that stands on its own (msgtype "notice").
	AuthorACI string
	MsgType   string
	Body      string
	// Doesn't count as activity in the chat (Signal doesn't move the chat up the list for these either).
	Quiet bool
}

// AddUpdates stores updates made at sentAt by authorACI (who is part of what identifies them, so importing them
// again doesn't repeat them).
func (s *Sink) AddUpdates(ctx context.Context, c *Chat, authorACI string, sentAt int64, lines []Update) error {
	if len(lines) == 0 || !s.ensure(ctx, c) {
		return nil
	}
	res, err := s.st.Tx(ctx, func(tx *store.Tx) error {
		for i, line := range lines {
			var authorID int64
			var err error
			switch {
			case line.AuthorACI == s.Self:
				authorID, err = tx.SelfPerson()
			case line.AuthorACI != "":
				authorID, err = tx.Person(line.AuthorACI)
			default:
				authorID, err = tx.Person(store.BotRemoteID)
			}
			if err != nil {
				return err
			}
			h := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%s\x00%d\x00%d", c.Key.ID, c.Key.Receiver, authorACI, sentAt, i)))
			_, err = tx.PutPart(store.NewPart{
				ConversationID: c.conversationID,
				AuthorID:       authorID,
				EventID:        "$u" + base64.RawURLEncoding.EncodeToString(h[:])[:40],
				SentAt:         sentAt,
				Kind:           "update",
				MsgType:        line.MsgType,
				Body:           line.Body,
				Quiet:          line.Quiet,
				Source:         store.SourceBackfill,
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("storing an update: %w", err)
	}
	s.publish(res)
	return nil
}

func (s *Sink) publish(res store.TxResult) {
	for _, id := range res.Touched {
		s.broker.Publish("conversation", events.ConversationChanged{ConversationID: id})
	}
}
