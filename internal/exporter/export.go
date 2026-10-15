package exporter

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

const (
	// Signal rejects message bodies over 128 KiB in a backup.
	maxBodyBytes = 128 * 1024
	// A backup may not have disappearing messages with less than a day left: the phone would only have to delete them
	// straight away.
	minExpiryLeft = 24 * time.Hour
	appVersion    = "bottled-signal-client"
	selfRecipient = 1
)

// Counts is what an export wrote.
type Counts struct {
	Messages    int `json:"messages"`
	Skipped     int `json:"skipped"`
	Attachments int `json:"attachments"`
}

// export writes the store as a backup a phone can restore with the account's recovery key.
type export struct {
	m        *Manager
	secrets  signalcli.Secrets
	w        *backup.Writer
	now      time.Time
	progress func(Counts)
	counts   Counts

	// Backup recipient IDs, by ACI (people) and by conversation ID (each chat's recipient).
	people map[string]uint64
	chats  map[int64]exportChat
}

type exportChat struct {
	recipient uint64
	conv      store.ExportConversation
	// Who the account's messages in it went to (besides the account itself).
	members []string
}

func (e *export) run(ctx context.Context, dir string) (string, error) {
	login := e.m.svc.CurrentLogin(ctx)
	if login == nil {
		return "", ErrNotLoggedIn
	}
	cl := login.Client.(*connector.Client)
	var err error
	if e.secrets, err = signalcli.ReadSecrets(e.m.signalDir, cl.Account().Number); err != nil {
		return "", err
	}
	aci, err := uuid.Parse(e.secrets.ACI)
	if err != nil {
		return "", fmt.Errorf("the account's ACI: %w", err)
	}
	mediaRoot := e.secrets.MediaRootBackupKey
	if mediaRoot == nil {
		mediaRoot = make([]byte, 32)
		_, _ = rand.Read(mediaRoot)
	}
	info := &backuppb.BackupInfo{
		Version: 1, BackupTimeMs: uint64(e.now.UnixMilli()), MediaRootBackupKey: mediaRoot,
		CurrentAppVersion: appVersion, FirstAppVersion: appVersion,
	}
	if e.w, err = backup.NewWriter(dir, e.secrets.RecoveryKey, aci, info, e.now); err != nil {
		return "", err
	}
	if err = e.write(ctx, cl); err != nil {
		_ = e.w.Close()
		return "", err
	}
	if err = e.w.Close(); err != nil {
		return "", err
	}
	return e.w.BackupDir(), nil
}

func (e *export) write(ctx context.Context, cl *connector.Client) error {
	people, err := e.m.st.ExportPeople(ctx)
	if err != nil {
		return err
	}
	conversations, err := e.m.st.ExportConversations(ctx)
	if err != nil {
		return err
	}
	selfName := ""
	for _, p := range people {
		if p.IsSelf {
			selfName = p.Name
		}
	}
	if err = e.frame(&backuppb.Frame{Item: &backuppb.Frame_Account{Account: e.account(selfName)}}); err != nil {
		return err
	}
	if err = e.writeRecipients(ctx, cl, people); err != nil {
		return err
	}
	if err = e.writeChats(ctx, cl, conversations); err != nil {
		return err
	}
	return e.writeItems(ctx)
}

func (e *export) frame(f *backuppb.Frame) error {
	return e.w.WriteFrame(f)
}

func (e *export) account(name string) *backuppb.AccountData {
	given, family, _ := strings.Cut(name, " ")
	return &backuppb.AccountData{
		ProfileKey: e.secrets.ProfileKey, GivenName: given, FamilyName: family,
		AccountSettings: &backuppb.AccountData_AccountSettings{
			ReadReceipts: true, TypingIndicators: true, LinkPreviews: true,
			PhoneNumberSharingMode:  backuppb.AccountData_NOBODY,
			DefaultSentMediaQuality: backuppb.AccountData_STANDARD,
			AppTheme:                backuppb.AccountData_SYSTEM,
			CallsUseLessDataSetting: backuppb.AccountData_NEVER,
		},
	}
}

func aciBytes(aci string) []byte {
	id, err := uuid.Parse(aci)
	if err != nil {
		return nil
	}
	return id[:]
}

// writeRecipients writes the account itself and everyone with an ACI. People get IDs from 2 up, in the store's order.
func (e *export) writeRecipients(ctx context.Context, cl *connector.Client, people []store.ExportPerson) error {
	e.people = map[string]uint64{cl.ACI(): selfRecipient}
	if err := e.frame(&backuppb.Frame{Item: &backuppb.Frame_Recipient{Recipient: &backuppb.Recipient{
		Id: selfRecipient, Destination: &backuppb.Recipient_Self{Self: &backuppb.Self{}},
	}}}); err != nil {
		return err
	}
	next := uint64(selfRecipient + 1)
	for _, p := range people {
		if p.IsSelf || !signalid.IsACI(p.RemoteID) {
			continue
		}
		contact := &backuppb.Contact{
			Aci:          aciBytes(p.RemoteID),
			Registration: &backuppb.Contact_Registered_{Registered: &backuppb.Contact_Registered{}},
		}
		if given, family, _ := strings.Cut(p.Name, " "); given != "" {
			contact.ProfileGivenName = ptr.Ptr(given)
			if family != "" {
				contact.ProfileFamilyName = ptr.Ptr(family)
			}
		}
		if number := e.phoneNumber(ctx, p.RemoteID); number != 0 {
			contact.E164 = ptr.Ptr(number)
		}
		e.people[p.RemoteID] = next
		if err := e.frame(&backuppb.Frame{Item: &backuppb.Frame_Recipient{Recipient: &backuppb.Recipient{
			Id: next, Destination: &backuppb.Recipient_Contact{Contact: contact},
		}}}); err != nil {
			return err
		}
		next++
	}
	return nil
}

// phoneNumber is a person's number, if the bridge knows it.
func (e *export) phoneNumber(ctx context.Context, aci string) uint64 {
	ghost, err := e.m.svc.Bridge().GetExistingGhostByID(ctx, signalid.UserID(aci))
	if err != nil || ghost == nil {
		return 0
	}
	for _, identifier := range ghost.Identifiers {
		if number, ok := strings.CutPrefix(identifier, "tel:+"); ok {
			if n, err := strconv.ParseUint(number, 10, 64); err == nil {
				return n
			}
		}
	}
	return 0
}

// writeChats writes a recipient for each group and a chat for each conversation that has one. Groups get recipient
// IDs after the people, and chats share their conversation's ID.
func (e *export) writeChats(ctx context.Context, cl *connector.Client, conversations []store.ExportConversation) error {
	e.chats = make(map[int64]exportChat)
	next := uint64(len(e.people) + 1)
	pinned := uint32(0)
	masterKeys, err := signalcli.GroupMasterKeys(e.m.signalDir, cl.Account().Number)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Couldn't read signal-cli's group keys; using the ones saved with the chats")
	}
	for _, c := range conversations {
		if c.ReadOnly || c.Deleted || c.Kind == store.KindSpace {
			continue
		}
		var members []string
		for _, m := range c.Members {
			if m.Membership == string(event.MembershipJoin) && m.RemoteID != cl.ACI() && signalid.IsACI(m.RemoteID) {
				members = append(members, m.RemoteID)
			}
		}
		var recipientID uint64
		switch c.Kind {
		case store.KindDM:
			recipientID = e.people[c.ThreadKey]
		case store.KindGroup:
			group, err := e.group(ctx, c, members, masterKeys)
			if err != nil {
				return err
			} else if group == nil {
				zerolog.Ctx(ctx).Warn().Str("group", c.ThreadKey).Msg("Leaving out a group without its master key")
				continue
			}
			recipientID = next
			next++
			if err = e.frame(&backuppb.Frame{Item: &backuppb.Frame_Recipient{Recipient: &backuppb.Recipient{
				Id: recipientID, Destination: &backuppb.Recipient_Group{Group: group},
			}}}); err != nil {
				return err
			}
		}
		if recipientID == 0 {
			continue
		}
		chat := &backuppb.Chat{Id: uint64(c.ID), RecipientId: recipientID, Archived: c.Archived}
		if c.Pinned {
			pinned++
			chat.PinnedOrder = ptr.Ptr(pinned)
		}
		if c.DisappearTimerMS > 0 {
			chat.ExpirationTimerMs = ptr.Ptr(uint64(c.DisappearTimerMS))
			chat.ExpireTimerVersion = 1
		}
		if c.MutedUntil == -1 {
			chat.MuteUntilMs = ptr.Ptr(uint64(1<<63 - 1))
		} else if c.MutedUntil > e.now.UnixMilli() {
			chat.MuteUntilMs = ptr.Ptr(uint64(c.MutedUntil))
		}
		if err = e.frame(&backuppb.Frame{Item: &backuppb.Frame_Chat{Chat: chat}}); err != nil {
			return err
		}
		e.chats[c.ID] = exportChat{recipient: recipientID, conv: c, members: members}
	}
	return nil
}

func (e *export) group(ctx context.Context, c store.ExportConversation, members []string, signalKeys map[string][]byte) (*backuppb.Group, error) {
	key := signalKeys[c.ThreadKey]
	if key == nil {
		portal, err := e.m.svc.Bridge().GetExistingPortalByKey(ctx, signalid.GroupPortalKey(c.ThreadKey))
		if err != nil {
			return nil, err
		}
		if portal != nil && portal.Metadata.(*connector.PortalMetadata).MasterKey != "" {
			if key, err = base64.StdEncoding.DecodeString(portal.Metadata.(*connector.PortalMetadata).MasterKey); err != nil {
				return nil, err
			}
		}
	}
	if len(key) != 32 {
		return nil, nil
	}
	admins, err := e.admins(ctx, c.RoomID)
	if err != nil {
		return nil, err
	}
	snapshot := &backuppb.Group_GroupSnapshot{}
	if c.Title != "" {
		snapshot.Title = &backuppb.Group_GroupAttributeBlob{Content: &backuppb.Group_GroupAttributeBlob_Title{Title: c.Title}}
	}
	for _, aci := range append([]string{e.secrets.ACI}, members...) {
		role := backuppb.Group_Member_DEFAULT
		if admins[aci] {
			role = backuppb.Group_Member_ADMINISTRATOR
		}
		snapshot.Members = append(snapshot.Members, &backuppb.Group_Member{UserId: aciBytes(aci), Role: role})
	}
	return &backuppb.Group{MasterKey: key, Whitelisted: true, Snapshot: snapshot}, nil
}

// admins are the group's admins, from the power levels the bridge gave them.
func (e *export) admins(ctx context.Context, roomID string) (map[string]bool, error) {
	state, err := e.m.st.RoomState(ctx, roomID, event.StatePowerLevels.Type)
	if err != nil {
		return nil, err
	}
	admins := make(map[string]bool)
	entry, ok := state[""]
	if !ok {
		return admins, nil
	}
	var pl event.PowerLevelsEventContent
	if err = json.Unmarshal([]byte(entry.Content), &pl); err != nil {
		return nil, err
	}
	for user, level := range pl.Users {
		if ghost, ok := localmx.ParseGhostMXID(user); ok && level >= 50 {
			admins[string(ghost)] = true
		} else if user == localmx.OwnerMXID && level >= 50 {
			admins[e.secrets.ACI] = true
		}
	}
	return admins, nil
}

func (e *export) writeItems(ctx context.Context) error {
	err := e.m.st.ExportMessages(ctx, func(m store.ExportMessage) error {
		item := e.item(ctx, m)
		if item == nil {
			e.counts.Skipped++
			return nil
		}
		e.counts.Messages++
		if (e.counts.Messages+e.counts.Skipped)%200 == 0 {
			e.progress(e.counts)
		}
		return e.frame(&backuppb.Frame{Item: &backuppb.Frame_ChatItem{ChatItem: item}})
	})
	return err
}

// sentAt is the timestamp Signal knows a message by: the one in the bridge's ID for it, which for an edited message is
// its last edit's (signal-cli's, which differs from the store's time for edits made here).
func (e *export) sentAt(ctx context.Context, m store.ExportMessage) uint64 {
	if part, err := e.m.svc.Bridge().DB.Message.GetPartByMXID(ctx, id.EventID(m.EventID)); err == nil && part != nil {
		if _, ts, err := signalid.ParseMessageID(part.ID); err == nil {
			return uint64(ts)
		}
	}
	if _, ts, err := signalid.ParseMessageID(networkid.MessageID(m.RemoteID)); err == nil && m.EditedAt == 0 {
		return uint64(ts)
	}
	if m.EditedAt != 0 {
		return uint64(m.EditedAt)
	}
	return uint64(m.SentAt)
}
