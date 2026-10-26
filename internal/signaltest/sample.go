package signaltest

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"go.mau.fi/util/ptr"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
)

// Carol is only in the sample backup: signal-cli doesn't know her.
const CarolACI = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

const (
	alice = AliceACI
	bob   = BobACI
	carol = CarolACI
)

// Backup recipient and chat IDs.
const (
	rSelf = iota + 1
	rAlice
	rBob
	rCarol
	rGroup
	rOldGroup
)

const (
	cAlice = iota + 1
	cGroup
	cOldGroup
	cSelf
	cCarol
)

// OldGroupKey is the master key of a group only in the sample backup (one left long ago).
var OldGroupKey = []byte("an old group's master key, 32 b.")

func aci(s string) []byte {
	id := uuid.MustParse(s)
	return id[:]
}

func recipient(id uint64, r *backuppb.Recipient) *backuppb.Frame {
	r.Id = id
	return &backuppb.Frame{Item: &backuppb.Frame_Recipient{Recipient: r}}
}

func contact(who, given string) *backuppb.Recipient {
	return &backuppb.Recipient{Destination: &backuppb.Recipient_Contact{Contact: &backuppb.Contact{Aci: aci(who), ProfileGivenName: &given,
		Registration: &backuppb.Contact_Registered_{Registered: &backuppb.Contact_Registered{}}}}}
}

func group(key []byte, title string, members ...string) *backuppb.Recipient {
	g := &backuppb.Group{MasterKey: key, Snapshot: &backuppb.Group_GroupSnapshot{
		Title: &backuppb.Group_GroupAttributeBlob{Content: &backuppb.Group_GroupAttributeBlob_Title{Title: title}},
	}}
	for _, m := range members {
		g.Snapshot.Members = append(g.Snapshot.Members, &backuppb.Group_Member{UserId: aci(m)})
	}
	return &backuppb.Recipient{Destination: &backuppb.Recipient_Group{Group: g}}
}

func chat(id, recipientID uint64) *backuppb.Frame {
	return &backuppb.Frame{Item: &backuppb.Frame_Chat{Chat: &backuppb.Chat{Id: id, RecipientId: recipientID}}}
}

func item(it *backuppb.ChatItem) *backuppb.Frame {
	return &backuppb.Frame{Item: &backuppb.Frame_ChatItem{ChatItem: it}}
}

func incoming(read bool) *backuppb.ChatItem_Incoming {
	return &backuppb.ChatItem_Incoming{Incoming: &backuppb.ChatItem_IncomingMessageDetails{Read: read}}
}

func text(body string) *backuppb.ChatItem_StandardMessage {
	return &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{Text: &backuppb.Text{Body: body}}}
}

// base is an hour or so ago, so disappearing messages and read-receipt windows behave as in real use.
var base = time.Now().Add(-time.Hour).UnixMilli()

// At is when the sample backup's message n seconds in was sent.
func At(s int) uint64 {
	return uint64(base + int64(s)*1000)
}

// SampleBackup writes a backup with a bit of everything: text, an edit, an image, reactions, receipts, a deletion,
// formatting and a quote, updates, chats signal-cli doesn't know, and a message that has already disappeared. It
// returns the zip's path.
func SampleBackup(t *testing.T) string {
	b := NewBackup(t)
	photo := b.Attachment([]byte("a photo"), "image/jpeg", "beach.jpg")
	b.Frames(
		recipient(rSelf, &backuppb.Recipient{Destination: &backuppb.Recipient_Self{Self: &backuppb.Self{}}}),
		recipient(rAlice, contact(alice, "Alice")),
		recipient(rBob, contact(bob, "Bob")),
		recipient(rCarol, contact(carol, "Carol")),
		recipient(rGroup, group(GroupMasterKey, "Weekend plans", alice, bob)),
		recipient(rOldGroup, group(OldGroupKey, "Book club", carol)),
		chat(cAlice, rAlice), chat(cGroup, rGroup), chat(cOldGroup, rOldGroup), chat(cSelf, rSelf), chat(cCarol, rCarol),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rAlice, DateSent: At(0), DirectionalDetails: incoming(true),
			Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
				Text:      &backuppb.Text{Body: "hi from the backup"},
				Reactions: []*backuppb.Reaction{{Emoji: "👍", AuthorId: rSelf, SentTimestamp: At(1)}},
			}}}),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rSelf, DateSent: At(10),
			DirectionalDetails: &backuppb.ChatItem_Outgoing{Outgoing: &backuppb.ChatItem_OutgoingMessageDetails{
				SendStatus: []*backuppb.SendStatus{{RecipientId: rAlice, Timestamp: At(20),
					DeliveryStatus: &backuppb.SendStatus_Read_{Read: &backuppb.SendStatus_Read{}}}},
			}},
			Item: text("my reply")}),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rAlice, DateSent: At(40), DirectionalDetails: incoming(true),
			Revisions: []*backuppb.ChatItem{{ChatId: cAlice, AuthorId: rAlice, DateSent: At(30), DirectionalDetails: incoming(true), Item: text("first version")}},
			Item:      text("second version")}),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rAlice, DateSent: At(50), DirectionalDetails: incoming(false),
			Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
				Text:        &backuppb.Text{Body: "look"},
				Attachments: []*backuppb.MessageAttachment{{Pointer: photo}},
			}}}),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rSelf, DateSent: At(60),
			DirectionalDetails: &backuppb.ChatItem_Outgoing{Outgoing: &backuppb.ChatItem_OutgoingMessageDetails{}},
			Item:               &backuppb.ChatItem_RemoteDeletedMessage{RemoteDeletedMessage: &backuppb.RemoteDeletedMessage{}}}),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rAlice, DateSent: At(70), DirectionalDetails: &backuppb.ChatItem_Directionless{},
			Item: &backuppb.ChatItem_UpdateMessage{UpdateMessage: &backuppb.ChatUpdateMessage{
				Update: &backuppb.ChatUpdateMessage_SimpleUpdate{SimpleUpdate: &backuppb.SimpleChatUpdate{Type: backuppb.SimpleChatUpdate_IDENTITY_UPDATE}},
			}}}),
		item(&backuppb.ChatItem{ChatId: cAlice, AuthorId: rAlice, DateSent: At(80), DirectionalDetails: incoming(true),
			ExpiresInMs: ptr.Ptr(uint64(60_000)), ExpireStartDate: ptr.Ptr(At(80)),
			Item: text("this disappeared long ago")}),
		item(&backuppb.ChatItem{ChatId: cGroup, AuthorId: rAlice, DateSent: At(90), DirectionalDetails: incoming(true), Item: text("who's bringing what?")}),
		item(&backuppb.ChatItem{ChatId: cGroup, AuthorId: rBob, DateSent: At(100), DirectionalDetails: incoming(true),
			Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
				Text: &backuppb.Text{Body: "￼ bring snacks", BodyRanges: []*backuppb.BodyRange{
					{Start: 0, Length: 1, AssociatedValue: &backuppb.BodyRange_MentionAci{MentionAci: aci(alice)}},
					{Start: 2, Length: 5, AssociatedValue: &backuppb.BodyRange_Style_{Style: backuppb.BodyRange_BOLD}},
				}},
				Quote: &backuppb.Quote{TargetSentTimestamp: ptr.Ptr(At(90)), AuthorId: rAlice},
			}}}),
		item(&backuppb.ChatItem{ChatId: cGroup, AuthorId: rBob, DateSent: At(110), DirectionalDetails: &backuppb.ChatItem_Directionless{},
			Item: &backuppb.ChatItem_UpdateMessage{UpdateMessage: &backuppb.ChatUpdateMessage{
				Update: &backuppb.ChatUpdateMessage_GroupChange{GroupChange: &backuppb.GroupChangeChatUpdate{
					Updates: []*backuppb.GroupChangeChatUpdate_Update{{Update: &backuppb.GroupChangeChatUpdate_Update_GroupNameUpdate{
						GroupNameUpdate: &backuppb.GroupNameUpdate{UpdaterAci: aci(bob), NewGroupName: ptr.Ptr("Weekend plans")},
					}}},
				}},
			}}}),
		item(&backuppb.ChatItem{ChatId: cOldGroup, AuthorId: rCarol, DateSent: At(120), DirectionalDetails: incoming(true), Item: text("old times")}),
		item(&backuppb.ChatItem{ChatId: cSelf, AuthorId: rSelf, DateSent: At(130),
			DirectionalDetails: &backuppb.ChatItem_Outgoing{Outgoing: &backuppb.ChatItem_OutgoingMessageDetails{}},
			Item:               text("note to self")}),
		item(&backuppb.ChatItem{ChatId: cCarol, AuthorId: rCarol, DateSent: At(140), DirectionalDetails: incoming(true), Item: text("hi, it's Carol")}),
	)
	return b.Zip()
}
