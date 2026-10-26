package legacy

import (
	"context"
	"database/sql"
	"encoding/base64"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/importer"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// ImportWhenLoggedIn imports the set-aside history once the account is logged in to the bridge, then drops the old
// tables. The previous version registered the account with signal-cli, which still has it; if it's the only one there,
// the bridge logs in to it straight away (as the "existing account" login would), so upgrading needs nothing from
// the user.
func ImportWhenLoggedIn(ctx context.Context, dbPath string, svc *bridge.Service, rpc *signalcli.Client, st *store.Store, fs *files.Store, broker *events.Broker, log zerolog.Logger) {
	ctx = log.WithContext(ctx)
	if err := takeOverAccount(ctx, svc, rpc); err != nil {
		log.Err(err).Msg("Couldn't log in to the previous version's account; log in with the account already on this server")
	}
	for svc.CurrentLogin(ctx) == nil {
		select {
		case <-ctx.Done():
			return
		case <-time.After(10 * time.Second):
		}
	}
	log.Info().Msg("Importing the previous version's history")
	counts, err := migrate(ctx, dbPath, svc, st, fs, broker)
	if err != nil {
		log.Err(err).Msg("Importing the previous version's history failed; it's kept, to try again on the next start")
		return
	}
	log.Info().Interface("counts", counts).Msg("Imported the previous version's history")
}

func takeOverAccount(ctx context.Context, svc *bridge.Service, rpc *signalcli.Client) error {
	if svc.CurrentLogin(ctx) != nil {
		return nil
	}
	if err := rpc.WaitUp(ctx); err != nil {
		return err
	}
	accounts, err := rpc.ListAccounts(ctx)
	if err != nil || len(accounts) != 1 {
		return err
	}
	step, err := svc.StartLogin(ctx, connector.FlowExisting)
	if err != nil {
		return err
	} else if step.Type != bridgev2.LoginStepTypeComplete {
		svc.CancelLogin()
		return fmt.Errorf("unexpected login step %s", step.StepID)
	}
	return nil
}

func migrate(ctx context.Context, dbPath string, svc *bridge.Service, st *store.Store, fs *files.Store, broker *events.Broker) (importer.Counts, error) {
	db, err := open(dbPath)
	if err != nil {
		return importer.Counts{}, err
	}
	defer db.Close()
	sink, err := importer.NewSink(ctx, svc, st, broker)
	if err != nil {
		return importer.Counts{}, err
	}
	m := &migration{db: db, sink: sink, fs: fs}
	if err = m.loadRecipients(); err != nil {
		return sink.Counts, err
	}
	if err = m.loadConversations(); err != nil {
		return sink.Counts, err
	}
	if err = m.messages(ctx); err != nil {
		return sink.Counts, err
	}
	for _, c := range m.chats {
		if err = sink.Flush(ctx, c.chat); err != nil {
			return sink.Counts, err
		}
	}
	return sink.Counts, drop(db)
}

type legacyRecipient struct {
	kind     string
	aci      string
	groupID  string
	name     string
	members  []string
	masterKy []byte
}

type legacyChat struct {
	chat       *importer.Chat
	who        legacyRecipient
	lastReadAt int64
}

type migration struct {
	db         *sql.DB
	sink       *importer.Sink
	fs         *files.Store
	recipients map[int64]legacyRecipient
	chats      map[int64]*legacyChat
}

func (m *migration) loadRecipients() error {
	m.recipients = make(map[int64]legacyRecipient)
	rows, err := m.db.Query(`SELECT id, kind, coalesce(aci, ''), coalesce(group_id, ''), coalesce(group_master_key, ''),
		coalesce(contact_name, profile_name, group_title, e164, '') FROM legacy_recipients`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var r legacyRecipient
		var masterKey string
		if err = rows.Scan(&id, &r.kind, &r.aci, &r.groupID, &masterKey, &r.name); err != nil {
			return err
		}
		if r.kind == "self" {
			r.aci = m.sink.Self
		}
		if masterKey != "" {
			if r.masterKy, err = base64.StdEncoding.DecodeString(masterKey); err != nil {
				return fmt.Errorf("group %s's master key: %w", r.groupID, err)
			}
		}
		m.recipients[id] = r
	}
	if err = rows.Err(); err != nil {
		return err
	}
	members, err := m.db.Query("SELECT group_recipient_id, member_recipient_id FROM legacy_group_members")
	if err != nil {
		return err
	}
	defer members.Close()
	for members.Next() {
		var group, member int64
		if err = members.Scan(&group, &member); err != nil {
			return err
		}
		if r, ok := m.recipients[group]; ok && m.recipients[member].aci != "" {
			r.members = append(r.members, m.recipients[member].aci)
			m.recipients[group] = r
		}
	}
	return members.Err()
}

func (m *migration) loadConversations() error {
	m.chats = make(map[int64]*legacyChat)
	rows, err := m.db.Query("SELECT id, recipient_id, archived, pinned, muted_until, last_read_at FROM legacy_conversations")
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, recipientID, mutedUntil, lastReadAt int64
		var archived, pinned bool
		if err = rows.Scan(&id, &recipientID, &archived, &pinned, &mutedUntil, &lastReadAt); err != nil {
			return err
		}
		who, ok := m.recipients[recipientID]
		if !ok {
			continue
		}
		c := &importer.Chat{Pinned: pinned, Archived: archived}
		if mutedUntil > time.Now().UnixMilli() {
			c.MutedUntil = mutedUntil
		}
		members := &bridgev2.ChatMemberList{IsFull: true, MemberMap: bridgev2.ChatMemberMap{}}
		members.MemberMap.Set(bridgev2.ChatMember{EventSender: m.sink.Sender(m.sink.Self), Membership: event.MembershipJoin})
		switch {
		case who.kind == "group" && signalid.IsGroupID(who.groupID):
			c.Key, c.MasterKey = signalid.GroupPortalKey(who.groupID), who.masterKy
			for _, aci := range who.members {
				members.MemberMap.Set(bridgev2.ChatMember{EventSender: m.sink.Sender(aci), Membership: event.MembershipJoin})
			}
			c.Fallback = &bridgev2.ChatInfo{Name: ptr.Ptr(who.name), Type: ptr.Ptr(database.RoomTypeDefault), Members: members}
		case signalid.IsACI(who.aci):
			c.Key = m.sink.DMKey(who.aci)
			c.Fallback = &bridgev2.ChatInfo{Type: ptr.Ptr(database.RoomTypeDM), Members: members}
			if who.aci == m.sink.Self {
				c.Fallback.Name = ptr.Ptr("Note to Self")
			} else {
				members.MemberMap.Set(bridgev2.ChatMember{EventSender: m.sink.Sender(who.aci), Membership: event.MembershipJoin})
				members.OtherUserID = signalid.UserID(who.aci)
			}
		default:
			// Someone known only by phone number, or a group without its ID.
			continue
		}
		m.chats[id] = &legacyChat{chat: c, who: who, lastReadAt: lastReadAt}
	}
	return rows.Err()
}

type legacyMessage struct {
	id, conversationID, authorID, sentAt, receivedAt int64
	kind, body, bodyRanges, updatePayload            string
	quoteAuthorID, quoteSentAt                       sql.NullInt64
	editedAt, deletedAt, expiresAt                   sql.NullInt64
	expireTimerS                                     int64
	viewOnce                                         bool
}

func (m *migration) messages(ctx context.Context) error {
	rows, err := m.db.Query(`SELECT id, conversation_id, author_id, sent_at, received_at, kind, coalesce(body, ''),
			coalesce(body_ranges, ''), coalesce(update_payload, ''), quote_author_id, quote_sent_at, edited_at,
			deleted_at, expires_at, expire_timer_s, view_once
		FROM legacy_messages ORDER BY sent_at, id`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var lm legacyMessage
		if err = rows.Scan(&lm.id, &lm.conversationID, &lm.authorID, &lm.sentAt, &lm.receivedAt, &lm.kind, &lm.body,
			&lm.bodyRanges, &lm.updatePayload, &lm.quoteAuthorID, &lm.quoteSentAt, &lm.editedAt, &lm.deletedAt,
			&lm.expiresAt, &lm.expireTimerS, &lm.viewOnce); err != nil {
			return err
		}
		if err = m.message(ctx, lm); err != nil {
			return fmt.Errorf("message %d: %w", lm.id, err)
		}
	}
	return rows.Err()
}

func (m *migration) message(ctx context.Context, lm legacyMessage) error {
	lc, ok := m.chats[lm.conversationID]
	author := m.recipients[lm.authorID]
	if !ok || !signalid.IsACI(author.aci) {
		return nil
	}
	if lm.expiresAt.Valid && lm.expiresAt.Int64 <= time.Now().UnixMilli() {
		return nil
	}
	if lm.kind == "update" {
		return m.sink.AddUpdates(ctx, lc.chat, author.aci, lm.sentAt, m.updateLines(author, lm.updatePayload))
	}
	sentAt := []int64{lm.sentAt}
	latest := lm.sentAt
	if lm.editedAt.Valid {
		latest = lm.editedAt.Int64
		sentAt = append(sentAt, latest)
	}
	if exists, err := m.sink.Exists(ctx, lc.chat, author.aci, sentAt...); err != nil || exists {
		if exists {
			m.sink.Counts.Skipped++
		}
		return err
	}
	msg, err := m.convert(lm, author, latest)
	if err != nil {
		return err
	}
	if msg == nil {
		return nil
	}
	if lm.editedAt.Valid {
		msg.OtherIDs = []networkid.MessageID{signalid.MessageID(author.aci, lm.sentAt)}
		if err = m.revisions(lm, msg); err != nil {
			return err
		}
	}
	if err = m.reactions(lm, msg); err != nil {
		return err
	}
	if err = m.receipts(lm, msg, author, lc); err != nil {
		return err
	}
	m.sink.NameGhost(ctx, author.aci, author.name)
	if lc.who.kind != "group" {
		m.sink.NameGhost(ctx, lc.who.aci, lc.who.name)
	}
	return m.sink.Add(ctx, lc.chat, msg)
}
