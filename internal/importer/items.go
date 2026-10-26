package importer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/backup/backuppb"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/msgconv"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

func ms(ts uint64) time.Time {
	return time.UnixMilli(int64(ts))
}

// convert turns a chat item into a message for the bridge, or nil for one there's nothing to show of (eg a payment
// notification from a future version).
func (r *run) convert(ctx context.Context, author recipient, item *backuppb.ChatItem) *bridge.ImportedMessage {
	m := &msgconv.Message{FromMe: author.self, SentAt: ms(item.GetDateSent())}
	if timer := item.GetExpiresInMs(); timer > 0 {
		m.ExpiresIn = time.Duration(timer) * time.Millisecond
	}
	var reactions []*backuppb.Reaction
	deleted := false
	switch it := item.GetItem().(type) {
	case *backuppb.ChatItem_StandardMessage:
		r.fillStandard(ctx, m, it.StandardMessage)
		reactions = it.StandardMessage.GetReactions()
	case *backuppb.ChatItem_ContactMessage:
		m.Body = contactCard(it.ContactMessage.GetContact())
		reactions = it.ContactMessage.GetReactions()
	case *backuppb.ChatItem_StickerMessage:
		s := it.StickerMessage.GetSticker()
		m.Sticker = &msgconv.Sticker{Emoji: s.GetEmoji()}
		if s.GetData() != nil {
			if a := r.attachment(ctx, s.GetData(), false); a.URL != "" {
				m.Sticker.Image = &a
			}
		}
		reactions = it.StickerMessage.GetReactions()
	case *backuppb.ChatItem_RemoteDeletedMessage, *backuppb.ChatItem_AdminDeletedMessage:
		// A placeholder, which the deletion then empties.
		deleted = true
		m.Notice = "Deleted"
	case *backuppb.ChatItem_ViewOnceMessage:
		m.ViewOnce = true
		if a := it.ViewOnceMessage.GetAttachment(); a != nil {
			m.Attachments = append(m.Attachments, r.attachment(ctx, a.GetPointer(), false))
		} else {
			m.Notice = "View-once media"
		}
		reactions = it.ViewOnceMessage.GetReactions()
	case *backuppb.ChatItem_Poll:
		var options []string
		for _, o := range it.Poll.GetOptions() {
			options = append(options, o.GetOption())
		}
		m.Body = msgconv.PollText(it.Poll.GetQuestion(), options)
		reactions = it.Poll.GetReactions()
	case *backuppb.ChatItem_DirectStoryReplyMessage:
		d := it.DirectStoryReplyMessage
		if emoji := d.GetEmoji(); emoji != "" {
			m.Body = "Reacted " + emoji + " to a story"
		} else if text := d.GetTextReply().GetText(); text != nil {
			m.Body = "↩️ Replied to a story: " + text.GetBody()
		}
		reactions = d.GetReactions()
	case *backuppb.ChatItem_PaymentNotification:
		m.Notice = strings.TrimSpace("💸 A payment, which this app can't show. " + it.PaymentNotification.GetNote())
	case *backuppb.ChatItem_GiftBadge:
		m.Notice = "🎁 A gift badge, which this app can't show."
	default:
		return nil
	}
	if m.IsEmpty() {
		return nil
	}
	converted := msgconv.Convert(m, r.person)
	if converted.Disappear.Type != "" && item.ExpireStartDate != nil {
		converted.Disappear.DisappearAt = ms(item.GetExpireStartDate()).Add(m.ExpiresIn)
	}
	msg := &bridge.ImportedMessage{
		ConvertedMessage: converted,
		ID:               signalid.MessageID(author.aci, int64(item.GetDateSent())),
		Sender:           r.sink.Sender(author.aci),
		Timestamp:        ms(item.GetDateSent()),
	}
	if deleted {
		msg.DeletedAt = msg.Timestamp
	} else {
		r.addRevisions(ctx, msg, author, item)
	}
	for _, reaction := range reactions {
		who, ok := r.recipients[reaction.GetAuthorId()]
		if !ok || who.aci == "" || reaction.GetEmoji() == "" {
			continue
		}
		msg.Reactions = append(msg.Reactions, &bridgev2.BackfillReaction{
			Sender: r.sink.Sender(who.aci), Emoji: reaction.GetEmoji(), Timestamp: ms(reaction.GetSentTimestamp()),
		})
	}
	r.addReceipts(msg, item)
	return msg
}

// addRevisions records an edited message's earlier versions. A backup gives the message its latest version's
// timestamp, which is also how Signal refers to it; the earlier ones are the IDs it had before.
func (r *run) addRevisions(ctx context.Context, msg *bridge.ImportedMessage, author recipient, item *backuppb.ChatItem) {
	for _, rev := range item.GetRevisions() {
		msg.OtherIDs = append(msg.OtherIDs, signalid.MessageID(author.aci, int64(rev.GetDateSent())))
		std := rev.GetStandardMessage()
		if std == nil {
			continue
		}
		text := &msgconv.Message{}
		r.fillText(ctx, text, std)
		if text.Body == "" {
			continue
		}
		parts := msgconv.Convert(text, r.person).Parts
		msg.Revisions = append(msg.Revisions, bridge.ImportedRevision{
			Timestamp: ms(rev.GetDateSent()), Content: parts[len(parts)-1].Content,
		})
	}
	// Edits can only change the text, which is the last part.
	if len(msg.Revisions) > 0 && msg.Parts[len(msg.Parts)-1].Content.MsgType.IsMedia() && len(msg.Parts) > 1 {
		msg.Revisions = nil
	}
}

// addReceipts records how far the account's own messages got, and whether others' were read.
func (r *run) addReceipts(msg *bridge.ImportedMessage, item *backuppb.ChatItem) {
	if in := item.GetIncoming(); in != nil {
		msg.Read = in.GetRead()
		return
	}
	delivered, sent := false, false
	for _, s := range item.GetOutgoing().GetSendStatus() {
		who, ok := r.recipients[s.GetRecipientId()]
		if !ok || who.aci == "" {
			continue
		}
		switch s.GetDeliveryStatus().(type) {
		case *backuppb.SendStatus_Read_, *backuppb.SendStatus_Viewed_:
			delivered = true
			msg.ReadBy = append(msg.ReadBy, bridge.ImportedReceipt{Sender: signalid.UserID(who.aci), At: ms(s.GetTimestamp())})
		case *backuppb.SendStatus_Delivered_:
			delivered = true
		case *backuppb.SendStatus_Sent_:
			sent = true
		}
	}
	switch {
	case delivered:
		msg.Status = "delivered"
	case sent:
		msg.Status = "sent"
	}
}

func (r *run) fillStandard(ctx context.Context, m *msgconv.Message, s *backuppb.StandardMessage) {
	r.fillText(ctx, m, s)
	for _, a := range s.GetAttachments() {
		m.Attachments = append(m.Attachments, r.attachment(ctx, a.GetPointer(), a.GetFlag() == backuppb.MessageAttachment_VOICE_MESSAGE))
	}
	for _, p := range s.GetLinkPreview() {
		preview := msgconv.Preview{URL: p.GetUrl(), Title: p.GetTitle(), Description: p.GetDescription()}
		if p.GetImage() != nil {
			if img := r.attachment(ctx, p.GetImage(), false); img.URL != "" {
				preview.Image = &img
			}
		}
		m.Previews = append(m.Previews, preview)
	}
	if q := s.GetQuote(); q != nil && q.TargetSentTimestamp != nil {
		if who, ok := r.recipients[q.GetAuthorId()]; ok && who.aci != "" {
			m.QuoteAuthorACI, m.QuoteSentAt = who.aci, int64(q.GetTargetSentTimestamp())
		}
	}
}

// fillText sets the text, from the long-text file if the message has one (it then holds the whole text).
func (r *run) fillText(ctx context.Context, m *msgconv.Message, s *backuppb.StandardMessage) {
	m.Body = s.GetText().GetBody()
	if s.GetLongText() != nil {
		var buf bytes.Buffer
		found, err := r.archive.WriteAttachment(s.GetLongText(), &buf)
		if err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Msg("Couldn't read a long message's full text; keeping its start")
		} else if found {
			m.Body = buf.String()
		}
	}
	for _, br := range s.GetText().GetBodyRanges() {
		rng := msgconv.BodyRange{Start: int(br.GetStart()), Length: int(br.GetLength())}
		if aci := aciString(br.GetMentionAci()); aci != "" {
			rng.MentionACI = aci
		} else if style := br.GetStyle(); style != backuppb.BodyRange_NONE {
			rng.Style = style.String()
		} else {
			continue
		}
		m.Ranges = append(m.Ranges, rng)
	}
}

var errNotInBackup = errors.New("not in the backup")

// attachment decrypts an attachment from the backup into the media store. One the backup doesn't have (it wasn't on
// the phone) is kept as missing.
func (r *run) attachment(ctx context.Context, p *backuppb.FilePointer, voice bool) msgconv.Attachment {
	a := msgconv.Attachment{
		ContentType: p.GetContentType(), Filename: p.GetFileName(), Size: int64(p.GetLocatorInfo().GetSize()),
		Width: int(p.GetWidth()), Height: int(p.GetHeight()), Caption: p.GetCaption(), Blurhash: p.GetBlurHash(),
		VoiceNote: voice, DurationMS: int(p.GetAudioDurationSeconds() * 1000),
	}
	if a.ContentType == "" {
		a.ContentType = "application/octet-stream"
	}
	sha, err := r.m.fs.PutStream(func(w io.Writer) error {
		found, err := r.archive.WriteAttachment(p, w)
		if err == nil && !found {
			return errNotInBackup
		}
		return err
	})
	if errors.Is(err, errNotInBackup) {
		return a
	} else if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("filename", a.Filename).Msg("Skipping an attachment that didn't decrypt")
		return a
	}
	a.URL = localmx.ContentURI(sha)
	r.sink.Counts.Attachments++
	return a
}

func contactCard(c *backuppb.ContactAttachment) string {
	name := joinName(c.GetName().GetGivenName(), c.GetName().GetFamilyName())
	if name == "" {
		name = c.GetName().GetNickname()
	}
	var phones, emails []string
	for _, n := range c.GetNumber() {
		phones = append(phones, n.GetValue())
	}
	for _, e := range c.GetEmail() {
		emails = append(emails, e.GetValue())
	}
	return msgconv.ContactCard(name, phones, emails, c.GetOrganization())
}
