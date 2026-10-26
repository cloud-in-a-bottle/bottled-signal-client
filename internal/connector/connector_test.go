package connector_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signaltest"
)

const (
	alice = signaltest.AliceACI
	bob   = signaltest.BobACI
	self  = signaltest.SelfACI
)

var aliceDM = signaltest.DMKey(alice)

func TestIncomingMessages(t *testing.T) {
	h := signaltest.New(t)
	h.Push(signaltest.Text(alice, 1_700_000_000_000, "hello there", ""))
	msgs := h.WaitForMessages(aliceDM, 1)
	if m := msgs[0]; m.Body != "hello there" || m.AuthorName != "Alice" || m.Outgoing || m.SentAt != 1_700_000_000_000 {
		t.Fatalf("message: %+v", m)
	}
	list, err := h.Store.ListConversations(h.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, c := range list {
		names[c.Kind] = c.Name
	}
	if names["dm"] != "Alice" || names["group"] != "Weekend plans" {
		t.Fatalf("conversations: %+v", list)
	}

	h.Push(signaltest.Text(bob, 1_700_000_001_000, "hi all", signaltest.GroupID))
	msgs = h.WaitForMessages(signaltest.GroupKey(), 1)
	if msgs[0].AuthorName != "Bob" || msgs[0].Body != "hi all" {
		t.Fatalf("group message: %+v", msgs[0])
	}
	// signal-cli redelivering a message (eg after a crash) doesn't duplicate it.
	h.Push(signaltest.Text(alice, 1_700_000_000_000, "hello there", ""))
	h.Push(signaltest.Text(alice, 1_700_000_002_000, "again", ""))
	if msgs = h.WaitForMessages(aliceDM, 2); msgs[1].Body != "again" {
		t.Fatalf("messages: %+v", msgs)
	}
}

func TestOwnMessageFromPhone(t *testing.T) {
	h := signaltest.New(t)
	body := "sent from my phone"
	h.Push(&signalcli.Envelope{SourceUUID: self, SourceDevice: 2, Timestamp: 1_700_000_000_000, SyncMessage: &signalcli.SyncMessage{
		SentMessage: &signalcli.SentMessage{
			DestinationUUID: alice,
			DataMessage:     signalcli.DataMessage{Timestamp: 1_700_000_000_000, Message: &body},
		},
	}})
	msgs := h.WaitForMessages(aliceDM, 1)
	if !msgs[0].Outgoing || msgs[0].Body != body {
		t.Fatalf("message: %+v", msgs[0])
	}
}

func TestReactionsEditsAndDeletes(t *testing.T) {
	h := signaltest.New(t)
	h.Push(signaltest.Text(alice, 1_000, "original", ""))
	h.WaitForMessages(aliceDM, 1)

	// Alice edits it; from then on Signal refers to it by the edit's timestamp.
	edited := "edited"
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 2_000, EditMessage: &signalcli.EditMessage{
		TargetSentTimestamp: 1_000, DataMessage: &signalcli.DataMessage{Timestamp: 2_000, Message: &edited},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return m[0].Body == "edited" && m[0].EditedAt == 2_000 })
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 3_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 3_000, Reaction: &signalcli.Reaction{Emoji: "👍", TargetAuthorUUID: alice, TargetSentTimestamp: 2_000},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return len(m[0].Reactions) == 1 && m[0].Reactions[0].Emoji == "👍" })
	// A new reaction replaces the last, as on Signal.
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 4_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 4_000, Reaction: &signalcli.Reaction{Emoji: "😂", TargetAuthorUUID: alice, TargetSentTimestamp: 2_000},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return len(m[0].Reactions) == 1 && m[0].Reactions[0].Emoji == "😂" })
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 5_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 5_000, Reaction: &signalcli.Reaction{Emoji: "😂", TargetAuthorUUID: alice, TargetSentTimestamp: 2_000, IsRemove: true},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return len(m[0].Reactions) == 0 })

	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 6_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 6_000, RemoteDelete: &signalcli.RemoteDelete{TargetTimestamp: 2_000},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return m[0].Deleted && m[0].Body == "" })
}

func TestFormattingAndReplies(t *testing.T) {
	h := signaltest.New(t)
	h.Push(signaltest.Text(bob, 1_000, "first", signaltest.GroupID))
	body := "￼ look at this, it's great"
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 2_000, DataMessage: &signalcli.DataMessage{
		Timestamp:  2_000,
		Message:    &body,
		GroupInfo:  &signalcli.GroupInfo{GroupID: signaltest.GroupID, Type: "DELIVER"},
		Mentions:   []signalcli.Mention{{UUID: bob, Start: 0, Length: 1}},
		TextStyles: []signalcli.TextStyle{{Style: "BOLD", Start: 21, Length: 5}},
		Quote:      &signalcli.Quote{ID: 1_000, AuthorUUID: bob, Text: "first"},
	}})
	msgs := h.WaitForMessages(signaltest.GroupKey(), 2)
	m := msgs[1]
	if m.Body != "@Bob look at this, it's great" {
		t.Fatalf("body: %q", m.Body)
	}
	if !strings.Contains(m.FormattedBody, "<strong>great</strong>") || !strings.Contains(m.FormattedBody, ">@Bob</a>") {
		t.Fatalf("formatted body: %q", m.FormattedBody)
	}
	if m.Quote == nil || m.Quote.MessageID != msgs[0].ID {
		t.Fatalf("reply: %+v", m.Quote)
	}
}

func TestAttachments(t *testing.T) {
	h := signaltest.New(t)
	path := filepath.Join(h.SignalDir, "attachments", "abc123.jpg")
	if err := os.WriteFile(path, []byte("jpeg bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	caption := "look"
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 1_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 1_000, Message: &caption, Attachments: []signalcli.Attachment{
			{ID: "abc123.jpg", ContentType: "image/jpeg", Filename: "beach.jpg", Size: 10, Width: 4, Height: 3},
			{ID: "never-downloaded.bin", ContentType: "application/pdf", Filename: "doc.pdf", Size: 99},
		},
	}})
	// Each attachment, and the text, is a part of its own, arriving one after another.
	msgs := h.WaitFor(aliceDM, func(m []store.Message) bool { return len(m) == 1 && m[0].Body != "" })
	a := msgs[0].Attachments
	if len(a) != 2 || a[0].Status != "present" || a[0].Filename != "beach.jpg" || a[0].MsgType != "m.image" || a[1].Status != "missing" {
		t.Fatalf("attachments: %+v", a)
	}
	if msgs[0].Body != "look" {
		t.Fatalf("body: %q", msgs[0].Body)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("signal-cli's copy wasn't removed: %v", err)
	}
	file, err := h.Store.AttachmentFile(h.Ctx, a[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := h.Files.Read(file.SHA256); err != nil || string(data) != "jpeg bytes" {
		t.Fatalf("stored file: %q, %v", data, err)
	}
}

func TestGroupChangesAndTimer(t *testing.T) {
	h := signaltest.New(t)
	h.Daemon.Groups[0].Name = "Weekend plans (v2)"
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 1_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 1_000, GroupInfo: &signalcli.GroupInfo{GroupID: signaltest.GroupID, Type: "UPDATE", Revision: 2},
	}})
	msgs := h.WaitForMessages(signaltest.GroupKey(), 1)
	if msgs[0].Kind != "update" || msgs[0].AuthorName != "Alice" || !strings.Contains(msgs[0].Body, "Weekend plans (v2)") {
		t.Fatalf("update: %+v", msgs[0])
	}

	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 2_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 2_000, IsExpirationUpdate: true, ExpiresInSeconds: 3600,
	}})
	msgs = h.WaitForMessages(aliceDM, 1)
	if msgs[0].Kind != "update" || msgs[0].Body != "set messages to disappear after 1 hour" {
		t.Fatalf("timer update: %+v", msgs[0])
	}
	detail, err := h.Store.ConversationDetail(h.Ctx, h.ConversationID(aliceDM))
	if err != nil || detail.Summary.DisappearMS != 3_600_000 || detail.Summary.DisappearType != "after_read" {
		t.Fatalf("conversation: %+v, %v", detail.Summary, err)
	}
}

func sendParams(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSendingFromHere(t *testing.T) {
	h := signaltest.New(t)
	// Read receipts go out for messages from the last day (see HandleMatrixReadReceipt), so this one is recent.
	receivedAt := time.Now().Add(-time.Minute).UnixMilli()
	h.Push(signaltest.Text(alice, receivedAt, "hi", ""))
	h.WaitForMessages(aliceDM, 1)
	conversationID := h.ConversationID(aliceDM)

	if _, err := h.Svc.Send(h.Ctx, conversationID, "hello Alice", 0, nil); err != nil {
		t.Fatal(err)
	}
	msgs := h.WaitFor(aliceDM, func(m []store.Message) bool { return len(m) == 2 && m[1].Status == "sent" })
	sent := sendParams(t, h.Daemon.Calls("send")[0])
	if sent["message"] != "hello Alice" || sent["recipient"].([]any)[0] != alice || sent["account"] != signaltest.SelfNumber {
		t.Fatalf("send: %v", sent)
	}
	sentAt := int64(1_800_000_000_001)

	// Alice's phone confirms delivery, then reading.
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 2_000, ReceiptMessage: &signalcli.ReceiptMessage{
		When: 2_000, IsDelivery: true, Timestamps: []int64{sentAt},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return m[1].Status == "delivered" })
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 3_000, ReceiptMessage: &signalcli.ReceiptMessage{
		When: 3_000, IsRead: true, Timestamps: []int64{sentAt},
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return m[1].Status == "read" })

	// Edits, reactions and deletions name the message by author and timestamp.
	if err := h.Svc.Edit(h.Ctx, msgs[1].ID, "hello, Alice"); err != nil {
		t.Fatal(err)
	}
	edit := sendParams(t, h.Daemon.Calls("send")[1])
	if edit["message"] != "hello, Alice" || edit["editTimestamp"] != float64(sentAt) {
		t.Fatalf("edit: %v", edit)
	}
	editedAt := int64(1_800_000_000_002)
	if err := h.Svc.React(h.Ctx, msgs[0].ID, "❤️"); err != nil {
		t.Fatal(err)
	}
	reaction := sendParams(t, h.Daemon.Calls("sendReaction")[0])
	if reaction["targetAuthor"] != alice || reaction["targetTimestamp"] != float64(receivedAt) || reaction["emoji"] != "❤️" {
		t.Fatalf("reaction: %v", reaction)
	}
	if err := h.Svc.Unsend(h.Ctx, msgs[1].ID); err != nil {
		t.Fatal(err)
	}
	deletion := sendParams(t, h.Daemon.Calls("remoteDelete")[0])
	if deletion["targetTimestamp"] != float64(editedAt) {
		t.Fatalf("delete targets the edited message's latest timestamp, got %v", deletion)
	}

	// Reading Alice's message tells her.
	if err := h.Svc.MarkRead(h.Ctx, conversationID); err != nil {
		t.Fatal(err)
	}
	h.Eventually("read receipt", func() bool { return len(h.Daemon.Calls("sendReceipt")) > 0 })
	receipt := sendParams(t, h.Daemon.Calls("sendReceipt")[0])
	if receipt["recipient"] != alice || receipt["type"] != "read" {
		t.Fatalf("receipt: %v", receipt)
	}
}

func TestStartChatByPhoneNumber(t *testing.T) {
	h := signaltest.New(t)
	conversationID, err := h.Svc.StartChat(h.Ctx, "+1 555 000 0002")
	if err != nil {
		t.Fatal(err)
	}
	if conversationID != h.ConversationID(aliceDM) {
		t.Fatalf("started conversation %d", conversationID)
	}
	if _, err = h.Svc.StartChat(h.Ctx, "+15559999999"); err == nil || !strings.Contains(err.Error(), "isn't on Signal") {
		t.Fatalf("unknown number: %v", err)
	}
}

func TestViewOnceMedia(t *testing.T) {
	h := signaltest.New(t)
	if err := os.WriteFile(filepath.Join(h.SignalDir, "attachments", "secret.jpg"), []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 1_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 1_000, ViewOnce: true, Attachments: []signalcli.Attachment{{ID: "secret.jpg", ContentType: "image/jpeg"}},
	}})
	msgs := h.WaitForMessages(aliceDM, 1)
	if !msgs[0].ViewOnce || msgs[0].Attachments[0].Status != "present" {
		t.Fatalf("view-once message: %+v", msgs[0])
	}
	if err := h.Svc.MarkViewed(h.Ctx, msgs[0].ID); err != nil {
		t.Fatal(err)
	}
	h.WaitFor(aliceDM, func(m []store.Message) bool { return m[0].Attachments[0].Status == "purged" })
	receipt := sendParams(t, h.Daemon.Calls("sendReceipt")[0])
	if receipt["type"] != "viewed" || receipt["recipient"] != alice {
		t.Fatalf("receipt: %v", receipt)
	}
}

func TestOtherAccountsAreIgnored(t *testing.T) {
	h := signaltest.New(t)
	body := "for another number"
	h.Daemon.Push(signalcli.Receive{Account: "+15559998888", Envelope: &signalcli.Envelope{
		SourceUUID: bob, SourceDevice: 1, Timestamp: 1_000, DataMessage: &signalcli.DataMessage{Timestamp: 1_000, Message: &body},
	}})
	h.Push(signaltest.Text(alice, 2_000, "for this one", ""))
	h.WaitForMessages(aliceDM, 1)
	if msgs := h.Messages(signaltest.DMKey(bob)); len(msgs) != 0 {
		t.Fatalf("another account's message was bridged: %+v", msgs)
	}
}

func TestEditingThenDeletingAnAlbum(t *testing.T) {
	h := signaltest.New(t)
	var attachments []signalcli.Attachment
	for _, name := range []string{"one.jpg", "two.jpg"} {
		if err := os.WriteFile(filepath.Join(h.SignalDir, "attachments", name), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
		attachments = append(attachments, signalcli.Attachment{ID: name, ContentType: "image/jpeg", Filename: name})
	}
	caption := "two photos"
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 1_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 1_000, Message: &caption, Attachments: attachments,
	}})
	h.WaitFor(aliceDM, func(m []store.Message) bool { return len(m) == 1 && m[0].Body == caption })

	// The edit carries the attachments again; they stay as they are.
	edited := "two nice photos"
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 2_000, EditMessage: &signalcli.EditMessage{
		TargetSentTimestamp: 1_000, DataMessage: &signalcli.DataMessage{Timestamp: 2_000, Message: &edited, Attachments: attachments},
	}})
	msgs := h.WaitFor(aliceDM, func(m []store.Message) bool { return m[0].Body == edited })
	if a := msgs[0].Attachments; len(a) != 2 || a[0].Status != "present" || a[1].Status != "present" {
		t.Fatalf("attachments after the edit: %+v", a)
	}
	// Deleting it names the edit's timestamp, and takes the whole album.
	h.Push(&signalcli.Envelope{SourceUUID: alice, SourceDevice: 1, Timestamp: 3_000, DataMessage: &signalcli.DataMessage{
		Timestamp: 3_000, RemoteDelete: &signalcli.RemoteDelete{TargetTimestamp: 2_000},
	}})
	msgs = h.WaitFor(aliceDM, func(m []store.Message) bool { return m[0].Deleted })
	for _, a := range msgs[0].Attachments {
		if a.Status == "present" {
			t.Fatalf("attachments after the deletion: %+v", msgs[0].Attachments)
		}
	}
}
