package connector

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// Group admins can rename the group and change its members; in the bridge's terms, moderators.
const adminPowerLevel = 50

func (cl *Client) GetChatInfo(ctx context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	chat, err := signalid.ParsePortalID(portal.ID)
	if err != nil {
		return nil, err
	}
	if chat.GroupID != "" {
		groups, err := cl.acct.Groups(ctx, chat.GroupID)
		if err != nil {
			return nil, err
		} else if len(groups) != 1 {
			return nil, fmt.Errorf("signal-cli doesn't know group %s", chat.GroupID)
		}
		return cl.groupInfo(ctx, groups[0]), nil
	}
	return cl.dmInfo(ctx, chat.ACI)
}

func disappearing(seconds int64) *database.DisappearingSetting {
	if seconds <= 0 {
		return &database.DisappearingSetting{}
	}
	return &database.DisappearingSetting{Type: event.DisappearingTypeAfterRead, Timer: time.Duration(seconds) * time.Second}
}

func (cl *Client) dmInfo(ctx context.Context, aci string) (*bridgev2.ChatInfo, error) {
	members := &bridgev2.ChatMemberList{
		IsFull:      true,
		OtherUserID: signalid.UserID(aci),
		MemberMap: bridgev2.ChatMemberMap{}.
			Set(bridgev2.ChatMember{EventSender: cl.sender(cl.aci), Membership: event.MembershipJoin}).
			Set(bridgev2.ChatMember{EventSender: cl.sender(aci), Membership: event.MembershipJoin}),
	}
	info := &bridgev2.ChatInfo{Type: ptr.Ptr(database.RoomTypeDM), Members: members}
	if aci == cl.aci {
		info.Name = ptr.Ptr("Note to Self")
		members.OtherUserID = ""
	}
	contacts, err := cl.acct.Contacts(ctx, aci)
	if err != nil {
		return nil, err
	}
	if len(contacts) == 1 {
		info.Disappear = disappearing(contacts[0].MessageExpirationTime)
	}
	return info, nil
}

func (cl *Client) groupInfo(ctx context.Context, g signalcli.Group) *bridgev2.ChatInfo {
	members := &bridgev2.ChatMemberList{IsFull: true, MemberMap: bridgev2.ChatMemberMap{}}
	for _, m := range g.Members {
		if !signalid.IsACI(m.UUID) {
			continue
		}
		member := bridgev2.ChatMember{EventSender: cl.sender(m.UUID), Membership: event.MembershipJoin}
		if m.IsAdmin {
			member.PowerLevel = ptr.Ptr(adminPowerLevel)
		}
		members.MemberMap.Set(member)
	}
	for _, m := range g.PendingMembers {
		if signalid.IsACI(m.UUID) {
			members.MemberMap.Add(bridgev2.ChatMember{EventSender: cl.sender(m.UUID), Membership: event.MembershipInvite})
		}
	}
	if !g.IsMember {
		members.MemberMap.Set(bridgev2.ChatMember{EventSender: cl.sender(cl.aci), Membership: event.MembershipLeave})
	}
	info := &bridgev2.ChatInfo{
		Name:      ptr.Ptr(g.Name),
		Topic:     ptr.Ptr(g.Description),
		Type:      ptr.Ptr(database.RoomTypeDefault),
		Members:   members,
		Disappear: disappearing(g.MessageExpirationTime),
		ExtraUpdates: func(ctx context.Context, portal *bridgev2.Portal) bool {
			return cl.saveMasterKey(ctx, portal, g.ID)
		},
	}
	if avatar, err := cl.acct.GroupAvatar(ctx, g.ID); err == nil {
		info.Avatar = &bridgev2.Avatar{
			ID:  avatarID(avatar),
			Get: func(ctx context.Context) ([]byte, error) { return avatar, nil },
		}
	} else if signalcli.IsRPCError(err) {
		info.Avatar = &bridgev2.Avatar{Remove: true}
	} else {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to get group avatar")
	}
	return info
}

func avatarID(data []byte) networkid.AvatarID {
	sum := sha256.Sum256(data)
	return networkid.AvatarID(hex.EncodeToString(sum[:8]))
}

// saveMasterKey keeps the group's master key with the portal, as backups need it.
func (cl *Client) saveMasterKey(ctx context.Context, portal *bridgev2.Portal, groupID string) bool {
	meta := portal.Metadata.(*PortalMetadata)
	if meta.MasterKey != "" {
		return false
	}
	keys, err := signalcli.GroupMasterKeys(cl.c.DataDir, cl.acct.Number)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to read group master keys")
		return false
	} else if key, ok := keys[groupID]; ok {
		meta.MasterKey = base64.StdEncoding.EncodeToString(key)
		return true
	}
	return false
}

// ContactName is how the app names a person: the nickname you gave them, else your name for them in your contacts,
// else the name in their profile, else their number.
func ContactName(c signalcli.Contact) string {
	if n := joinName(c.NickGivenName, c.NickFamilyName); n != "" {
		return n
	} else if c.NickName != "" {
		return c.NickName
	} else if c.Name != "" {
		return c.Name
	} else if n := joinName(c.GivenName, c.FamilyName); n != "" {
		return n
	} else if c.Profile != nil && joinName(c.Profile.GivenName, c.Profile.FamilyName) != "" {
		return joinName(c.Profile.GivenName, c.Profile.FamilyName)
	}
	return c.Number
}

func (cl *Client) userInfo(ctx context.Context, c signalcli.Contact) *bridgev2.UserInfo {
	info := &bridgev2.UserInfo{}
	if name := ContactName(c); name != "" {
		info.Name = ptr.Ptr(name)
	}
	if c.Number != "" {
		info.Identifiers = []string{"tel:" + c.Number}
	}
	if c.Profile != nil && c.Profile.HasAvatar {
		recipient := c.UUID
		info.Avatar = &bridgev2.Avatar{
			ID: networkid.AvatarID(fmt.Sprintf("profile-%d", c.Profile.LastUpdateTimestamp)),
			Get: func(ctx context.Context) ([]byte, error) {
				return cl.acct.ProfileAvatar(ctx, recipient)
			},
		}
	} else if c.Profile != nil {
		info.Avatar = &bridgev2.Avatar{Remove: true}
	}
	return info
}

func (cl *Client) GetUserInfo(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	contacts, err := cl.acct.Contacts(ctx, string(ghost.ID))
	if err != nil {
		return nil, err
	} else if len(contacts) == 0 {
		return &bridgev2.UserInfo{}, nil
	}
	return cl.userInfo(ctx, contacts[0]), nil
}
