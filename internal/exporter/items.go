package exporter

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// item turns a message into a chat item, or nil for one that can't (or mustn't) go in a backup: updates (kept here
// only as text), messages in chats or from people the backup doesn't have, and messages about to disappear.
func (e *export) item(ctx context.Context, m store.ExportMessage) *backuppb.ChatItem {
	chat, ok := e.chats[m.ConversationID]
	author, known := e.people[m.AuthorRemoteID]
	if !ok || !known || m.Kind != "message" {
		return nil
	}
	outgoing := author == selfRecipient
	read := m.SentAt <= chat.conv.LastReadAt
	item := &backuppb.ChatItem{ChatId: uint64(chat.conv.ID), AuthorId: author, DateSent: e.sentAt(ctx, m)}
	if !e.setTimer(item, m, outgoing || read) {
		return nil
	}
	switch {
	case m.DeletedAt != 0:
		item.Item = &backuppb.ChatItem_RemoteDeletedMessage{RemoteDeletedMessage: &backuppb.RemoteDeletedMessage{}}
	case m.ViewOnce:
		view := &backuppb.ViewOnceMessage{Reactions: e.reactions(m)}
		for _, a := range m.Attachments {
			if a.Status == "present" {
				view.Attachment = &backuppb.MessageAttachment{Pointer: e.pointer(ctx, a)}
				break
			}
		}
		item.Item = &backuppb.ChatItem_ViewOnceMessage{ViewOnceMessage: view}
	default:
		std := e.standard(ctx, m)
		if std == nil {
			return nil
		}
		item.Item = &backuppb.ChatItem_StandardMessage{StandardMessage: std}
	}
	e.setDirection(item, m, chat, outgoing, read)
	if _, ok := item.Item.(*backuppb.ChatItem_StandardMessage); ok {
		item.Revisions = e.revisions(item, m)
	}
	return item
}

// setTimer copies the message's disappearing timer, reporting false if the message is about to disappear anyway.
// started says whether its timer has started: Signal starts it when a message is sent or read.
func (e *export) setTimer(item *backuppb.ChatItem, m store.ExportMessage, started bool) bool {
	if m.DisappearTimerMS <= 0 && m.ExpiresAt == 0 {
		return true
	}
	timer := m.DisappearTimerMS
	expiresAt := m.ExpiresAt
	if timer <= 0 {
		timer = m.ExpiresAt - m.SentAt
	}
	if expiresAt == 0 && started {
		// Read here, at a time this store doesn't keep: count from when it arrived.
		expiresAt = m.ReceivedAt + timer
	}
	if expiresAt != 0 && time.UnixMilli(expiresAt).Sub(e.now) < minExpiryLeft {
		return false
	}
	item.ExpiresInMs = ptr.Ptr(uint64(timer))
	if expiresAt != 0 {
		item.ExpireStartDate = ptr.Ptr(uint64(expiresAt - timer))
	}
	return true
}

func (e *export) setDirection(item *backuppb.ChatItem, m store.ExportMessage, chat exportChat, outgoing, read bool) {
	if !outgoing {
		item.DirectionalDetails = &backuppb.ChatItem_Incoming{Incoming: &backuppb.ChatItem_IncomingMessageDetails{
			DateReceived: uint64(m.ReceivedAt), Read: read,
		}}
		return
	}
	out := &backuppb.ChatItem_OutgoingMessageDetails{DateReceived: uint64(m.ReceivedAt)}
	readBy := make(map[string]int64)
	for _, r := range chat.conv.ReadReceipts {
		if r.ReadUpTo >= m.SentAt {
			readBy[r.RemoteID] = r.At
		}
	}
	for _, aci := range chat.members {
		recipient, ok := e.people[aci]
		if !ok || recipient == selfRecipient {
			continue
		}
		s := &backuppb.SendStatus{RecipientId: recipient, Timestamp: uint64(m.SentAt)}
		switch at, wasRead := readBy[aci]; {
		case wasRead:
			s.Timestamp = uint64(at)
			s.DeliveryStatus = &backuppb.SendStatus_Read_{Read: &backuppb.SendStatus_Read{}}
		case m.Status == "delivered":
			s.DeliveryStatus = &backuppb.SendStatus_Delivered_{Delivered: &backuppb.SendStatus_Delivered{}}
		case m.Status == "failed":
			s.DeliveryStatus = &backuppb.SendStatus_Failed_{Failed: &backuppb.SendStatus_Failed{}}
		case m.Status == "pending":
			s.DeliveryStatus = &backuppb.SendStatus_Pending_{Pending: &backuppb.SendStatus_Pending{}}
		default:
			s.DeliveryStatus = &backuppb.SendStatus_Sent_{Sent: &backuppb.SendStatus_Sent{}}
		}
		out.SendStatus = append(out.SendStatus, s)
	}
	item.DirectionalDetails = &backuppb.ChatItem_Outgoing{Outgoing: out}
}

// revisions are an edited message's earlier versions, oldest first. The store keeps each replaced text with when it
// was replaced, which is when the next version was sent.
func (e *export) revisions(item *backuppb.ChatItem, m store.ExportMessage) []*backuppb.ChatItem {
	var out []*backuppb.ChatItem
	sent := uint64(m.SentAt)
	for _, edit := range m.Edits {
		if edit.Body != "" {
			rev := &backuppb.ChatItem{
				ChatId: item.ChatId, AuthorId: item.AuthorId, DateSent: sent,
				Item: &backuppb.ChatItem_StandardMessage{StandardMessage: &backuppb.StandardMessage{
					Text: &backuppb.Text{Body: truncate(edit.Body)},
				}},
			}
			switch d := item.DirectionalDetails.(type) {
			case *backuppb.ChatItem_Incoming:
				rev.DirectionalDetails = &backuppb.ChatItem_Incoming{Incoming: &backuppb.ChatItem_IncomingMessageDetails{
					DateReceived: sent, Read: d.Incoming.GetRead(),
				}}
			default:
				rev.DirectionalDetails = &backuppb.ChatItem_Outgoing{Outgoing: &backuppb.ChatItem_OutgoingMessageDetails{
					DateReceived: sent,
				}}
			}
			out = append(out, rev)
		}
		sent = uint64(edit.EditedAt)
	}
	return out
}

func (e *export) standard(ctx context.Context, m store.ExportMessage) *backuppb.StandardMessage {
	if m.Body == "" && len(m.Attachments) == 0 {
		return nil
	}
	std := &backuppb.StandardMessage{Reactions: e.reactions(m)}
	if m.Body != "" {
		std.Text = &backuppb.Text{Body: truncate(m.Body)}
	}
	for _, a := range m.Attachments {
		ma := &backuppb.MessageAttachment{Pointer: e.pointer(ctx, a), WasDownloaded: a.Status == "present"}
		if a.IsVoiceNote {
			ma.Flag = backuppb.MessageAttachment_VOICE_MESSAGE
		}
		std.Attachments = append(std.Attachments, ma)
	}
	if q := m.ReplyTo; q != nil {
		if author, ok := e.people[q.AuthorRemoteID]; ok {
			target := uint64(q.SentAt)
			if _, ts, err := signalid.ParseMessageID(networkid.MessageID(q.RemoteID)); err == nil {
				target = uint64(ts)
			}
			std.Quote = &backuppb.Quote{TargetSentTimestamp: ptr.Ptr(target), AuthorId: author, Type: backuppb.Quote_NORMAL}
			if q.Body != "" {
				std.Quote.Text = &backuppb.Text{Body: truncate(q.Body)}
			}
		}
	}
	if m.LinkPreviews != "" && m.Body != "" {
		var previews []event.BeeperLinkPreview
		if err := json.Unmarshal([]byte(m.LinkPreviews), &previews); err == nil {
			for _, p := range previews {
				url := p.CanonicalURL
				if url == "" {
					url = p.MatchedURL
				}
				if url == "" || !strings.Contains(m.Body, url) {
					continue
				}
				lp := &backuppb.LinkPreview{Url: url}
				if p.Title != "" {
					lp.Title = ptr.Ptr(p.Title)
				}
				if p.Description != "" {
					lp.Description = ptr.Ptr(p.Description)
				}
				std.LinkPreview = append(std.LinkPreview, lp)
			}
		}
	}
	return std
}

func (e *export) reactions(m store.ExportMessage) []*backuppb.Reaction {
	var out []*backuppb.Reaction
	for i, r := range m.Reactions {
		reactor, ok := e.people[r.ReactorRemoteID]
		if !ok {
			continue
		}
		out = append(out, &backuppb.Reaction{
			Emoji: r.Emoji, AuthorId: reactor, SentTimestamp: uint64(r.SentAt), SortOrder: uint64(i + 1),
		})
	}
	return out
}

// pointer writes an attachment's file into the backup, encrypted, and describes it. One whose file isn't here gets
// an empty locator, which marks it unavailable.
func (e *export) pointer(ctx context.Context, a store.ExportAttachment) *backuppb.FilePointer {
	p := &backuppb.FilePointer{LocatorInfo: &backuppb.FilePointer_LocatorInfo{}}
	if a.ContentType != "" {
		p.ContentType = ptr.Ptr(a.ContentType)
	}
	if a.Filename != "" {
		p.FileName = ptr.Ptr(a.Filename)
	}
	if a.Width > 0 && a.Height > 0 {
		p.Width, p.Height = ptr.Ptr(uint32(a.Width)), ptr.Ptr(uint32(a.Height))
	}
	if a.Caption != "" {
		p.Caption = ptr.Ptr(a.Caption)
	}
	if a.Blurhash != "" {
		p.BlurHash = ptr.Ptr(a.Blurhash)
	}
	if a.Status != "present" || a.FileSHA256 == "" {
		return p
	}
	f, err := os.Open(e.m.fs.Path(a.FileSHA256))
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Leaving out an attachment whose file is missing")
		return p
	}
	defer f.Close()
	loc, err := e.w.AddAttachment(f)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to write an attachment into the backup")
		return p
	}
	p.LocatorInfo = loc
	e.counts.Attachments++
	return p
}

// truncate cuts a body to what a backup allows.
func truncate(body string) string {
	if len(body) <= maxBodyBytes {
		return body
	}
	cut := body[:maxBodyBytes]
	for !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return cut
}
