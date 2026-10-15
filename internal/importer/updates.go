package importer

import (
	"fmt"
	"time"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/msgconv"
)

func (r *run) nameOf(aci string) string {
	if aci == r.sink.Self {
		return "You"
	} else if name := r.names[aci]; name != "" {
		return name
	}
	return "Someone"
}

func (r *run) updateLines(author recipient, u *backuppb.ChatUpdateMessage) []Update {
	by := func(msgtype, body string) []Update {
		return []Update{{AuthorACI: author.aci, MsgType: msgtype, Body: body}}
	}
	notice := func(body string, quiet bool) []Update {
		return []Update{{MsgType: "notice", Body: body, Quiet: quiet}}
	}
	switch upd := u.GetUpdate().(type) {
	case *backuppb.ChatUpdateMessage_SimpleUpdate:
		return r.simpleUpdate(author, upd.SimpleUpdate.GetType())
	case *backuppb.ChatUpdateMessage_ExpirationTimerChange:
		timer := time.Duration(upd.ExpirationTimerChange.GetExpiresInMs()) * time.Millisecond
		if timer == 0 {
			return by("disappearing", "turned off disappearing messages")
		}
		return by("disappearing", "set messages to disappear after "+msgconv.TimerText(timer))
	case *backuppb.ChatUpdateMessage_ProfileChange:
		p := upd.ProfileChange
		return notice(fmt.Sprintf("%s changed their name to %s", p.GetPreviousName(), p.GetNewName()), true)
	case *backuppb.ChatUpdateMessage_IndividualCall:
		return notice(callText(upd.IndividualCall), false)
	case *backuppb.ChatUpdateMessage_GroupCall:
		return notice("📞 Group call", false)
	case *backuppb.ChatUpdateMessage_ThreadMerge:
		return notice(fmt.Sprintf("Your history with %s and their number +%d was merged", r.nameOf(author.aci), upd.ThreadMerge.GetPreviousE164()), true)
	case *backuppb.ChatUpdateMessage_SessionSwitchover:
		return notice(fmt.Sprintf("%s belongs to +%d", r.nameOf(author.aci), upd.SessionSwitchover.GetE164()), true)
	case *backuppb.ChatUpdateMessage_PollTerminate:
		return by("poll", "ended a poll")
	case *backuppb.ChatUpdateMessage_PinMessage:
		return by("pin", "pinned a message")
	case *backuppb.ChatUpdateMessage_GroupChange:
		var lines []Update
		for _, change := range upd.GroupChange.GetUpdates() {
			lines = append(lines, r.groupChange(change))
		}
		return lines
	}
	return nil
}

func (r *run) simpleUpdate(author recipient, t backuppb.SimpleChatUpdate_Type) []Update {
	name := r.nameOf(author.aci)
	var body string
	quiet := false
	switch t {
	case backuppb.SimpleChatUpdate_JOINED_SIGNAL:
		body = name + " joined Signal"
	case backuppb.SimpleChatUpdate_IDENTITY_UPDATE:
		body, quiet = "Your safety number with "+name+" changed", true
	case backuppb.SimpleChatUpdate_IDENTITY_VERIFIED:
		body, quiet = "You marked your safety number with "+name+" as verified", true
	case backuppb.SimpleChatUpdate_IDENTITY_DEFAULT:
		body, quiet = "You marked your safety number with "+name+" as unverified", true
	case backuppb.SimpleChatUpdate_CHANGE_NUMBER:
		body = name + " changed their phone number"
	case backuppb.SimpleChatUpdate_END_SESSION:
		body, quiet = "Secure session reset", true
	case backuppb.SimpleChatUpdate_CHAT_SESSION_REFRESH:
		body, quiet = "Chat session refreshed", true
	case backuppb.SimpleChatUpdate_BAD_DECRYPT:
		body = "A message from " + name + " couldn't be delivered"
	case backuppb.SimpleChatUpdate_PAYMENTS_ACTIVATED:
		body, quiet = name+" activated payments", true
	case backuppb.SimpleChatUpdate_PAYMENT_ACTIVATION_REQUEST:
		body, quiet = name+" asked you to activate payments", true
	case backuppb.SimpleChatUpdate_UNSUPPORTED_PROTOCOL_MESSAGE:
		body = name + " sent a message this version of Signal couldn't show"
	case backuppb.SimpleChatUpdate_REPORTED_SPAM:
		body, quiet = "You reported this chat as spam", true
	case backuppb.SimpleChatUpdate_BLOCKED:
		body, quiet = "You blocked this chat", true
	case backuppb.SimpleChatUpdate_UNBLOCKED:
		body, quiet = "You unblocked this chat", true
	case backuppb.SimpleChatUpdate_MESSAGE_REQUEST_ACCEPTED:
		body, quiet = "You accepted the message request", true
	default:
		// Donation requests and types from later versions.
		return nil
	}
	return []Update{{MsgType: "notice", Body: body, Quiet: quiet}}
}

func callText(c *backuppb.IndividualCall) string {
	kind := "voice call"
	if c.GetType() == backuppb.IndividualCall_VIDEO_CALL {
		kind = "video call"
	}
	switch {
	case c.GetState() == backuppb.IndividualCall_MISSED || c.GetState() == backuppb.IndividualCall_MISSED_NOTIFICATION_PROFILE:
		return "📞 Missed " + kind
	case c.GetState() == backuppb.IndividualCall_NOT_ACCEPTED:
		return "📞 Declined " + kind
	case c.GetDirection() == backuppb.IndividualCall_OUTGOING:
		return "📞 Outgoing " + kind
	}
	return "📞 Incoming " + kind
}

// groupChange describes one change to a group, attributed to who made it when the backup says.
func (r *run) groupChange(change *backuppb.GroupChangeChatUpdate_Update) Update {
	by := func(updater []byte, body string) Update {
		if aci := aciString(updater); aci != "" {
			return Update{AuthorACI: aci, MsgType: "group", Body: body}
		}
		return Update{MsgType: "notice", Body: "Someone " + body}
	}
	name := func(raw []byte) string { return r.nameOf(aciString(raw)) }
	switch c := change.GetUpdate().(type) {
	case *backuppb.GroupChangeChatUpdate_Update_GroupCreationUpdate:
		return by(c.GroupCreationUpdate.GetUpdaterAci(), "created the group")
	case *backuppb.GroupChangeChatUpdate_Update_GroupNameUpdate:
		if n := c.GroupNameUpdate.GetNewGroupName(); n != "" {
			return by(c.GroupNameUpdate.GetUpdaterAci(), fmt.Sprintf("named the group “%s”", n))
		}
		return by(c.GroupNameUpdate.GetUpdaterAci(), "removed the group's name")
	case *backuppb.GroupChangeChatUpdate_Update_GroupAvatarUpdate:
		if c.GroupAvatarUpdate.GetWasRemoved() {
			return by(c.GroupAvatarUpdate.GetUpdaterAci(), "removed the group photo")
		}
		return by(c.GroupAvatarUpdate.GetUpdaterAci(), "changed the group photo")
	case *backuppb.GroupChangeChatUpdate_Update_GroupDescriptionUpdate:
		return by(c.GroupDescriptionUpdate.GetUpdaterAci(), "changed the group description")
	case *backuppb.GroupChangeChatUpdate_Update_GroupMemberJoinedUpdate:
		return by(c.GroupMemberJoinedUpdate.GetNewMemberAci(), "joined the group")
	case *backuppb.GroupChangeChatUpdate_Update_GroupMemberJoinedByLinkUpdate:
		return by(c.GroupMemberJoinedByLinkUpdate.GetNewMemberAci(), "joined the group with its link")
	case *backuppb.GroupChangeChatUpdate_Update_GroupInvitationAcceptedUpdate:
		return by(c.GroupInvitationAcceptedUpdate.GetNewMemberAci(), "joined the group")
	case *backuppb.GroupChangeChatUpdate_Update_GroupMemberAddedUpdate:
		a := c.GroupMemberAddedUpdate
		return by(a.GetUpdaterAci(), "added "+name(a.GetNewMemberAci()))
	case *backuppb.GroupChangeChatUpdate_Update_GroupMemberLeftUpdate:
		return by(c.GroupMemberLeftUpdate.GetAci(), "left the group")
	case *backuppb.GroupChangeChatUpdate_Update_GroupMemberRemovedUpdate:
		rm := c.GroupMemberRemovedUpdate
		return by(rm.GetRemoverAci(), "removed "+name(rm.GetRemovedAci()))
	case *backuppb.GroupChangeChatUpdate_Update_GroupAdminStatusUpdate:
		a := c.GroupAdminStatusUpdate
		if a.GetWasAdminStatusGranted() {
			return by(a.GetUpdaterAci(), "made "+name(a.GetMemberAci())+" an admin")
		}
		return by(a.GetUpdaterAci(), "removed "+name(a.GetMemberAci())+" as an admin")
	case *backuppb.GroupChangeChatUpdate_Update_SelfInvitedToGroupUpdate:
		return by(c.SelfInvitedToGroupUpdate.GetInviterAci(), "invited you to the group")
	case *backuppb.GroupChangeChatUpdate_Update_GroupExpirationTimerUpdate:
		e := c.GroupExpirationTimerUpdate
		if e.GetExpiresInMs() == 0 {
			return by(e.GetUpdaterAci(), "turned off disappearing messages")
		}
		return by(e.GetUpdaterAci(), "set messages to disappear after "+msgconv.TimerText(time.Duration(e.GetExpiresInMs())*time.Millisecond))
	case *backuppb.GroupChangeChatUpdate_Update_GroupAnnouncementOnlyChangeUpdate:
		if c.GroupAnnouncementOnlyChangeUpdate.GetIsAnnouncementOnly() {
			return by(c.GroupAnnouncementOnlyChangeUpdate.GetUpdaterAci(), "allowed only admins to send messages")
		}
		return by(c.GroupAnnouncementOnlyChangeUpdate.GetUpdaterAci(), "allowed all members to send messages")
	case *backuppb.GroupChangeChatUpdate_Update_GroupV2MigrationUpdate:
		return Update{MsgType: "notice", Body: "This group was upgraded to a new group"}
	case *backuppb.GroupChangeChatUpdate_Update_GenericGroupUpdate:
		return by(c.GenericGroupUpdate.GetUpdaterAci(), "updated the group")
	}
	return Update{MsgType: "notice", Body: "The group was updated"}
}
