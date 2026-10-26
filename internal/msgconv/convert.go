// Package msgconv turns Signal messages into the bridge's message parts. Live messages, imported backups and the
// pre-Go app's database all go through it, so a message arrives as the same parts (with the same part IDs, and so
// the same event IDs) whichever way it comes.
package msgconv

import (
	"fmt"
	"mime"
	"strings"
	"time"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// Attachment is a file already in the bridge's media store (URL), or one that couldn't be had (no URL).
type Attachment struct {
	URL         id.ContentURIString
	ContentType string
	Filename    string
	Size        int64
	Width       int
	Height      int
	DurationMS  int
	Caption     string
	Blurhash    string
	VoiceNote   bool
}

type Preview struct {
	URL         string
	Title       string
	Description string
	Image       *Attachment
}

type Sticker struct {
	Emoji string
	// Nil if the sticker's image couldn't be had.
	Image *Attachment
}

// Message is what a Signal message shows, whatever it came from.
type Message struct {
	Body        string
	Ranges      []BodyRange
	Attachments []Attachment
	Sticker     *Sticker
	Previews    []Preview
	// The message this one quotes (replies to).
	QuoteAuthorACI string
	QuoteSentAt    int64
	ViewOnce       bool
	// Signal's disappearing-messages timer for the message.
	ExpiresIn time.Duration
	// Set for a message the account sent: its timer runs from when it was sent rather than from when it's read.
	FromMe bool
	SentAt time.Time
	// A notice in place of content this app doesn't show (eg a payment), shown as a notice.
	Notice string
}

// IsEmpty reports whether there is nothing to show, eg a profile key update.
func (m *Message) IsEmpty() bool {
	return m.Body == "" && len(m.Attachments) == 0 && m.Sticker == nil && m.Notice == ""
}

// Convert turns a message into parts: the attachments in order, then the text. A single attachment with text is one
// part with a caption. person looks up mentioned people.
func Convert(m *Message, person func(aci string) Person) *bridgev2.ConvertedMessage {
	out := &bridgev2.ConvertedMessage{}
	if m.QuoteSentAt != 0 && signalid.IsACI(m.QuoteAuthorACI) {
		out.ReplyTo = &networkid.MessageOptionalPartID{MessageID: signalid.MessageID(m.QuoteAuthorACI, m.QuoteSentAt)}
	}
	if m.ExpiresIn > 0 {
		out.Disappear = database.DisappearingSetting{Type: event.DisappearingTypeAfterRead, Timer: m.ExpiresIn}
		if m.FromMe {
			out.Disappear.DisappearAt = m.SentAt.Add(m.ExpiresIn)
		}
	}
	var contents []*event.MessageEventContent
	eventType := event.EventMessage
	switch {
	case m.Sticker != nil:
		eventType = event.EventSticker
		contents = append(contents, stickerContent(m.Sticker))
	default:
		for _, a := range m.Attachments {
			contents = append(contents, attachmentContent(a, m.ViewOnce))
		}
	}
	if m.Notice != "" {
		contents = append(contents, &event.MessageEventContent{MsgType: event.MsgNotice, Body: m.Notice})
	}
	if m.Body != "" {
		f := format(m.Body, m.Ranges, person)
		text := &event.MessageEventContent{MsgType: event.MsgText, Body: f.Body}
		if f.HTML != "" {
			text.Format = event.FormatHTML
			text.FormattedBody = f.HTML
		}
		if len(f.Mentions) > 0 {
			text.Mentions = &event.Mentions{UserIDs: f.Mentions}
		}
		for _, p := range m.Previews {
			text.BeeperLinkPreviews = append(text.BeeperLinkPreviews, linkPreview(p))
		}
		contents = append(contents, text)
	}
	for i, content := range contents {
		partType := event.EventMessage
		if i == 0 {
			partType = eventType
		}
		out.Parts = append(out.Parts, &bridgev2.ConvertedMessagePart{ID: signalid.PartID(i), Type: partType, Content: content})
	}
	if eventType != event.EventSticker {
		out.MergeCaption()
	}
	return out
}

func msgTypeFor(contentType string) event.MessageType {
	switch {
	case strings.HasPrefix(contentType, "image/"):
		return event.MsgImage
	case strings.HasPrefix(contentType, "video/"):
		return event.MsgVideo
	case strings.HasPrefix(contentType, "audio/"):
		return event.MsgAudio
	}
	return event.MsgFile
}

// DefaultFilename names a file that came without a name, eg "image.jpeg".
func DefaultFilename(contentType string) string {
	kind, _, _ := strings.Cut(contentType, "/")
	switch kind {
	case "image", "video", "audio":
	default:
		kind = "file"
	}
	if exts, _ := mime.ExtensionsByType(contentType); len(exts) > 0 {
		return kind + exts[len(exts)-1]
	}
	return kind
}

func attachmentContent(a Attachment, viewOnce bool) *event.MessageEventContent {
	name := a.Filename
	if name == "" {
		name = DefaultFilename(a.ContentType)
	}
	content := &event.MessageEventContent{
		MsgType:  msgTypeFor(a.ContentType),
		Body:     name,
		FileName: name,
		URL:      a.URL,
		Info: &event.FileInfo{
			MimeType: a.ContentType,
			Size:     int(a.Size),
			Width:    a.Width,
			Height:   a.Height,
			Duration: a.DurationMS,
			Blurhash: a.Blurhash,
		},
	}
	if a.Caption != "" {
		content.Body = a.Caption
	}
	if a.VoiceNote {
		content.MsgType = event.MsgAudio
		content.MSC3245Voice = &event.MSC3245Voice{}
		content.MSC1767Audio = &event.MSC1767Audio{Duration: a.DurationMS}
	}
	if viewOnce {
		content.BeeperViewLimited = &event.BeeperViewLimitedMedia{Type: "count", Count: 1}
	}
	return content
}

func stickerContent(s *Sticker) *event.MessageEventContent {
	if s.Image == nil {
		return &event.MessageEventContent{MsgType: event.MsgNotice, Body: strings.TrimSpace(s.Emoji + " (sticker)")}
	}
	content := &event.MessageEventContent{
		Body: s.Emoji,
		URL:  s.Image.URL,
		Info: &event.FileInfo{MimeType: s.Image.ContentType, Size: int(s.Image.Size), Width: s.Image.Width, Height: s.Image.Height},
	}
	if content.Body == "" {
		content.Body = "sticker"
	}
	return content
}

func linkPreview(p Preview) *event.BeeperLinkPreview {
	lp := &event.BeeperLinkPreview{
		MatchedURL: p.URL,
		LinkPreview: event.LinkPreview{
			CanonicalURL: p.URL,
			Title:        p.Title,
			Description:  p.Description,
		},
	}
	if p.Image != nil && p.Image.URL != "" {
		lp.ImageURL = p.Image.URL
		lp.ImageType = p.Image.ContentType
		lp.ImageSize = event.IntOrString(p.Image.Size)
		lp.ImageWidth = event.IntOrString(p.Image.Width)
		lp.ImageHeight = event.IntOrString(p.Image.Height)
		lp.ImageBlurhash = p.Image.Blurhash
	}
	return lp
}

// ContactCard describes a shared contact as text.
func ContactCard(name string, phones, emails []string, organization string) string {
	lines := []string{"📇 " + strings.TrimSpace(name)}
	if organization != "" {
		lines = append(lines, organization)
	}
	lines = append(lines, phones...)
	lines = append(lines, emails...)
	return strings.Join(lines, "\n")
}

// PollText describes a poll as text.
func PollText(question string, options []string) string {
	lines := []string{"📊 " + question}
	for _, o := range options {
		lines = append(lines, "• "+o)
	}
	return strings.Join(lines, "\n")
}

// TimerText describes a disappearing-messages timer, as the update notices do.
func TimerText(d time.Duration) string {
	if d <= 0 {
		return "off"
	}
	for _, u := range []struct {
		d    time.Duration
		name string
	}{{7 * 24 * time.Hour, "week"}, {24 * time.Hour, "day"}, {time.Hour, "hour"}, {time.Minute, "minute"}, {time.Second, "second"}} {
		if d%u.d == 0 {
			n := int(d / u.d)
			if n == 1 {
				return "1 " + u.name
			}
			return fmt.Sprintf("%d %ss", n, u.name)
		}
	}
	return d.String()
}
