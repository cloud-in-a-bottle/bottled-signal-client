package connector

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/msgconv"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// convertMessage turns a data message into parts, moving its attachments out of signal-cli's directory into the
// bridge's media store.
func (cl *Client) convertMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, sender string, d *signalcli.DataMessage) *bridgev2.ConvertedMessage {
	m := cl.toMessage(ctx, portal, intent, sender, d)
	converted := msgconv.Convert(m, cl.personLookup(ctx))
	if converted.Disappear.Type != "" && !d.ViewOnce {
		// A message's timer is also the chat's, eg when a message arrives from someone who changed it while this
		// server was offline.
		portal.UpdateDisappearingSetting(ctx, database.DisappearingSetting{Type: converted.Disappear.Type, Timer: converted.Disappear.Timer},
			bridgev2.UpdateDisappearingSettingOpts{Sender: intent, Timestamp: m.SentAt, Implicit: true, Save: true, SendNotice: true})
	}
	return converted
}

const (
	editStubPart networkid.PartID = "editstub"
)

// convertEdit applies an edit. Signal refers to an edited message by its latest version's timestamp (later edits,
// reactions, replies and deletions target that), so every part of the message takes the edit's ID, and a stub keeps
// the old ID so a repeat of the edit is recognized. Only the text can be edited: the attachments an edit carries again
// are the original's, which are here already.
func (cl *Client) convertEdit(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, sender string, d *signalcli.DataMessage) (*bridgev2.ConvertedEdit, error) {
	existing = slices.DeleteFunc(slices.Clone(existing), func(m *database.Message) bool { return m.PartID == editStubPart })
	if len(existing) == 0 {
		return nil, fmt.Errorf("%w: edit target was already edited", bridgev2.ErrIgnoringRemoteEvent)
	}
	textOnly := *d
	textOnly.Attachments, textOnly.Previews, textOnly.Sticker = nil, nil, nil
	converted := msgconv.Convert(cl.toMessage(ctx, portal, intent, sender, &textOnly), cl.personLookup(ctx))
	if len(converted.Parts) == 0 {
		return nil, fmt.Errorf("%w: edit has no text", bridgev2.ErrIgnoringRemoteEvent)
	}
	newID := signalid.MessageID(sender, d.Timestamp)
	edit := &bridgev2.ConvertedEdit{}
	for i, part := range existing {
		renamed := ptr.Clone(part)
		renamed.ID = newID
		renamed.EditCount++
		if i < len(existing)-1 {
			// Not shown as edited, only renamed.
			edit.ModifiedParts = append(edit.ModifiedParts, &bridgev2.ConvertedEditPart{
				Part: renamed, Type: event.EventMessage, Content: &event.MessageEventContent{}, DontBridge: true,
			})
			continue
		}
		textPart := converted.Parts[len(converted.Parts)-1].ToEditPart(part)
		textPart.Part = renamed
		edit.ModifiedParts = append(edit.ModifiedParts, textPart)
	}
	if existing[0].ID != newID {
		edit.AddedParts = &bridgev2.ConvertedMessage{Parts: []*bridgev2.ConvertedMessagePart{{
			ID: editStubPart, Type: event.EventMessage, Content: &event.MessageEventContent{}, DontBridge: true,
		}}}
	}
	return edit, nil
}

// personLookup names mentioned people from the bridge's ghosts.
func (cl *Client) personLookup(ctx context.Context) func(aci string) msgconv.Person {
	return func(aci string) msgconv.Person {
		p := msgconv.Person{Name: aci, MXID: cl.c.br.Matrix.FormatGhostMXID(signalid.UserID(aci))}
		if aci == cl.aci {
			p.Name = cl.login.RemoteName
			return p
		}
		ghost, err := cl.c.br.GetGhostByID(ctx, signalid.UserID(aci))
		if err == nil && ghost.Name != "" {
			p.Name = ghost.Name
		}
		return p
	}
}

func (cl *Client) toMessage(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, sender string, d *signalcli.DataMessage) *msgconv.Message {
	m := &msgconv.Message{
		ViewOnce:  d.ViewOnce,
		ExpiresIn: time.Duration(d.ExpiresInSeconds) * time.Second,
		FromMe:    sender == cl.aci,
		SentAt:    ms(d.Timestamp),
	}
	if d.Message != nil {
		m.Body = *d.Message
	}
	for _, mention := range d.Mentions {
		if signalid.IsACI(mention.UUID) {
			m.Ranges = append(m.Ranges, msgconv.BodyRange{Start: mention.Start, Length: mention.Length, MentionACI: mention.UUID})
		}
	}
	for _, s := range d.TextStyles {
		m.Ranges = append(m.Ranges, msgconv.BodyRange{Start: s.Start, Length: s.Length, Style: s.Style})
	}
	if q := d.Quote; q != nil && signalid.IsACI(q.AuthorUUID) {
		m.QuoteAuthorACI, m.QuoteSentAt = q.AuthorUUID, q.ID
	}
	for _, a := range d.Attachments {
		m.Attachments = append(m.Attachments, cl.takeAttachment(ctx, portal, intent, a))
	}
	for _, p := range d.Previews {
		preview := msgconv.Preview{URL: p.URL, Title: p.Title, Description: p.Description}
		if p.Image != nil {
			preview.Image = ptr.Ptr(cl.takeAttachment(ctx, portal, intent, *p.Image))
		}
		m.Previews = append(m.Previews, preview)
	}
	if s := d.Sticker; s != nil {
		m.Sticker = &msgconv.Sticker{Image: cl.fetchSticker(ctx, portal, intent, s)}
	}
	for _, c := range d.Contacts {
		m.Body = strings.TrimSpace(m.Body + "\n\n" + contactCard(c))
	}
	if p := d.PollCreate; p != nil {
		m.Body = strings.TrimSpace(m.Body + "\n\n" + msgconv.PollText(p.Question, p.Options))
	}
	switch {
	case d.Payment != nil:
		m.Notice = strings.TrimSpace("💸 Sent a payment, which this app can't show. " + d.Payment.Note)
	case d.GroupCallUpdate != nil:
		m.Notice = "📞 Group call"
	case d.ViewOnce && len(d.Attachments) == 0:
		m.Notice = "View-once media that has already been viewed"
	}
	if d.StoryContext != nil && m.Body != "" {
		m.Body = "↩️ Replied to a story: " + m.Body
	}
	return m
}

func contactCard(c signalcli.SharedContact) string {
	var name, org string
	if c.Name != nil {
		name = joinName(c.Name.Given, c.Name.Family)
		if name == "" {
			name = c.Name.Nickname
		}
	}
	org = c.Organization
	var phones, emails []string
	for _, p := range c.Phone {
		phones = append(phones, p.Value)
	}
	for _, e := range c.Email {
		emails = append(emails, e.Value)
	}
	return msgconv.ContactCard(name, phones, emails, org)
}

// takeAttachment moves a file signal-cli downloaded into the bridge's media store. A file that didn't arrive (eg one
// that expired on Signal's servers) leaves an attachment without a URL, shown as missing.
func (cl *Client) takeAttachment(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, a signalcli.Attachment) msgconv.Attachment {
	out := msgconv.Attachment{
		ContentType: a.ContentType, Filename: a.Filename, Size: a.Size, Width: a.Width, Height: a.Height,
		Caption: a.Caption, VoiceNote: a.IsVoiceNote,
	}
	if out.ContentType == "" {
		out.ContentType = "application/octet-stream"
	}
	if a.ID == "" || strings.ContainsAny(a.ID, `/\`) {
		return out
	}
	path := filepath.Join(cl.c.DataDir, "attachments", a.ID)
	info, err := os.Stat(path)
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("attachment_id", a.ID).Msg("signal-cli didn't download an attachment")
		return out
	}
	uri, _, err := intent.UploadMediaStream(ctx, portal.MXID, info.Size(), true, func(w io.Writer) (*bridgev2.FileStreamResult, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return &bridgev2.FileStreamResult{FileName: out.Filename, MimeType: out.ContentType}, err
	})
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Str("attachment_id", a.ID).Msg("Failed to store attachment")
		return out
	}
	out.URL = uri
	out.Size = info.Size()
	if err = os.Remove(path); err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Msg("Failed to remove signal-cli's copy of an attachment")
	}
	return out
}

// fetchSticker gets a sticker's image. signal-cli downloads a sticker's pack when the sticker arrives, in the
// background, so the image may take a moment to be there.
func (cl *Client) fetchSticker(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, s *signalcli.Sticker) *msgconv.Attachment {
	var data []byte
	var err error
	for attempt := range 5 {
		if attempt > 0 {
			time.Sleep(time.Second)
		}
		if data, err = cl.acct.Sticker(ctx, s.PackID, s.StickerID); err == nil {
			break
		}
	}
	if err != nil {
		zerolog.Ctx(ctx).Warn().Err(err).Str("pack_id", s.PackID).Msg("Failed to get sticker image")
		return nil
	}
	contentType := http.DetectContentType(data)
	uri, _, err := intent.UploadMedia(ctx, portal.MXID, data, "sticker", contentType)
	if err != nil {
		zerolog.Ctx(ctx).Err(err).Msg("Failed to store sticker image")
		return nil
	}
	return &msgconv.Attachment{URL: uri, ContentType: contentType, Size: int64(len(data))}
}
