package importer

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/msgconv"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// recipient is someone (or a group) the backup refers to by its own IDs.
type recipient struct {
	// A person's ACI; for the account itself, its own.
	aci  string
	self bool
	name string
	// For groups.
	group   *backuppb.Group
	groupID string
}

// run is one import of a backup.
type run struct {
	m        *Manager
	progress func(fraction float64, counts Counts)
	counts   Counts

	sink       *Sink
	archive    *backup.Archive
	recipients map[uint64]recipient
	chats      map[uint64]*Chat
	names      map[string]string
	fraction   float64
	now        time.Time
}

func (r *run) importZip(ctx context.Context, zipPath, recoveryKey string) error {
	var err error
	if r.sink, err = NewSink(ctx, r.m.svc, r.m.st, r.m.broker); err != nil {
		return err
	}
	r.sink.OnBatch = func() { r.counts = r.sink.Counts; r.progress(r.fraction, r.counts) }
	r.recipients = make(map[uint64]recipient)
	r.chats = make(map[uint64]*Chat)
	r.names = make(map[string]string)
	r.now = time.Now()
	if r.archive, err = backup.Open(zipPath); err != nil {
		return err
	}
	defer r.archive.Close()
	frames, err := r.archive.Frames(recoveryKey, func(f float64) { r.fraction = f })
	if err != nil {
		return err
	}
	defer frames.Close()
	if !frames.IsFor(uuid.MustParse(r.sink.Self)) {
		// Its sent messages would show as this account's, and its chats as this account's chats.
		return userError{errors.New("this backup is from a different Signal account than the one registered here")}
	}
	info := frames.Info()
	zerolog.Ctx(ctx).Info().Str("app_version", info.GetCurrentAppVersion()).
		Time("made_at", time.UnixMilli(int64(info.GetBackupTimeMs()))).Msg("Importing backup")
	for n := 0; ; n++ {
		frame, err := frames.Next()
		if errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return err
		}
		switch item := frame.GetItem().(type) {
		case *backuppb.Frame_Recipient:
			r.addRecipient(item.Recipient)
		case *backuppb.Frame_Chat:
			r.addChat(item.Chat)
		case *backuppb.Frame_ChatItem:
			if err = r.addItem(ctx, item.ChatItem); err != nil {
				return err
			}
		}
		if n%500 == 0 {
			r.counts = r.sink.Counts
			r.progress(r.fraction, r.counts)
		}
	}
	for _, c := range r.chats {
		if err = r.sink.Flush(ctx, c); err != nil {
			return err
		}
	}
	r.counts = r.sink.Counts
	return nil
}

func aciString(raw []byte) string {
	id, err := uuid.FromBytes(raw)
	if err != nil {
		return ""
	}
	return id.String()
}

func joinName(given, family string) string {
	switch {
	case given == "":
		return family
	case family == "":
		return given
	}
	return given + " " + family
}

func (r *run) addRecipient(rec *backuppb.Recipient) {
	switch d := rec.GetDestination().(type) {
	case *backuppb.Recipient_Self:
		r.recipients[rec.GetId()] = recipient{aci: r.sink.Self, self: true}
	case *backuppb.Recipient_Contact:
		c := d.Contact
		aci := aciString(c.GetAci())
		if aci == "" {
			// Someone known only by phone number: they've never been on Signal with this account, or not since
			// ACIs exist. Nothing they're in can be told apart from them reliably.
			return
		}
		name := joinName(c.GetNickname().GetGiven(), c.GetNickname().GetFamily())
		if name == "" {
			name = joinName(c.GetSystemGivenName(), c.GetSystemFamilyName())
		}
		if name == "" {
			name = joinName(c.GetProfileGivenName(), c.GetProfileFamilyName())
		}
		if name == "" && c.GetE164() != 0 {
			name = fmt.Sprintf("+%d", c.GetE164())
		}
		r.recipients[rec.GetId()] = recipient{aci: aci, self: aci == r.sink.Self, name: name}
		r.names[aci] = name
	case *backuppb.Recipient_Group:
		key := d.Group.GetMasterKey()
		if len(key) != 32 {
			return
		}
		r.recipients[rec.GetId()] = recipient{
			group: d.Group, groupID: base64.StdEncoding.EncodeToString(backup.GroupIDFromMasterKey(key)),
		}
	}
	// Distribution lists (for stories), the release notes channel and call links have no place here.
}

func (r *run) addChat(frame *backuppb.Chat) {
	who, ok := r.recipients[frame.GetRecipientId()]
	if !ok {
		return
	}
	c := &Chat{Pinned: frame.PinnedOrder != nil, Archived: frame.GetArchived()}
	if until := frame.GetMuteUntilMs(); until > uint64(r.now.UnixMilli()) {
		c.MutedUntil = -1
		if until < 1<<62 {
			c.MutedUntil = int64(until)
		}
	}
	members := &bridgev2.ChatMemberList{IsFull: true, MemberMap: bridgev2.ChatMemberMap{}}
	members.MemberMap.Set(bridgev2.ChatMember{EventSender: r.sink.Sender(r.sink.Self), Membership: event.MembershipJoin})
	switch {
	case who.groupID != "":
		c.Key = signalid.GroupPortalKey(who.groupID)
		c.MasterKey = who.group.GetMasterKey()
		snapshot := who.group.GetSnapshot()
		for _, m := range snapshot.GetMembers() {
			if aci := aciString(m.GetUserId()); aci != "" {
				members.MemberMap.Set(bridgev2.ChatMember{EventSender: r.sink.Sender(aci), Membership: event.MembershipJoin})
			}
		}
		c.Fallback = &bridgev2.ChatInfo{
			Name: ptr.Ptr(snapshot.GetTitle().GetTitle()), Topic: ptr.Ptr(snapshot.GetDescription().GetDescriptionText()),
			Type: ptr.Ptr(database.RoomTypeDefault), Members: members,
		}
	case who.aci != "":
		c.Key = r.sink.DMKey(who.aci)
		c.Fallback = &bridgev2.ChatInfo{Type: ptr.Ptr(database.RoomTypeDM), Members: members}
		if who.self {
			c.Fallback.Name = ptr.Ptr("Note to Self")
		} else {
			members.MemberMap.Set(bridgev2.ChatMember{EventSender: r.sink.Sender(who.aci), Membership: event.MembershipJoin})
			members.OtherUserID = signalid.UserID(who.aci)
		}
	default:
		return
	}
	r.chats[frame.GetId()] = c
}

func (r *run) addItem(ctx context.Context, item *backuppb.ChatItem) error {
	c := r.chats[item.GetChatId()]
	author, ok := r.recipients[item.GetAuthorId()]
	if c == nil || !ok || author.aci == "" || item.GetDirectionalDetails() == nil {
		return nil
	}
	if timer := item.GetExpiresInMs(); timer > 0 && item.ExpireStartDate != nil &&
		item.GetExpireStartDate()+timer <= uint64(r.now.UnixMilli()) {
		// Already disappeared: its sender wanted it gone.
		return nil
	}
	if update := item.GetUpdateMessage(); update != nil {
		return r.sink.AddUpdates(ctx, c, author.aci, int64(item.GetDateSent()), r.updateLines(author, update))
	}
	sentAt := []int64{int64(item.GetDateSent())}
	for _, rev := range item.GetRevisions() {
		sentAt = append(sentAt, int64(rev.GetDateSent()))
	}
	if exists, err := r.sink.Exists(ctx, c, author.aci, sentAt...); err != nil {
		return err
	} else if exists {
		r.sink.Counts.Skipped++
		return nil
	}
	msg := r.convert(ctx, author, item)
	if msg == nil {
		return nil
	}
	r.sink.NameGhost(ctx, author.aci, author.name)
	if c.Key.Receiver != "" && c.Key.ID != "" {
		// A one-to-one chat: name the other person too, even if all their messages are already here.
		r.sink.NameGhost(ctx, string(c.Key.ID), r.names[string(c.Key.ID)])
	}
	return r.sink.Add(ctx, c, msg)
}

// person names mentioned people as the backup does, for links to their ghosts.
func (r *run) person(aci string) msgconv.Person {
	name := r.names[aci]
	if aci == r.sink.Self {
		name = r.sink.SelfName()
	}
	if name == "" {
		name = "Unknown"
	}
	return msgconv.Person{Name: name, MXID: r.sink.GhostMXID(aci)}
}
