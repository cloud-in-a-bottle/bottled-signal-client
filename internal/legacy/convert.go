package legacy

import (
	"encoding/json"
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/localmx"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/importer"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/msgconv"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// legacyRange is a body range as the previous version stored it.
type legacyRange struct {
	Start      int    `json:"start"`
	Length     int    `json:"length"`
	Style      string `json:"style"`
	MentionACI string `json:"mentionAci"`
}

func parseRanges(raw string) ([]msgconv.BodyRange, error) {
	if raw == "" {
		return nil, nil
	}
	var stored []legacyRange
	if err := json.Unmarshal([]byte(raw), &stored); err != nil {
		return nil, fmt.Errorf("body ranges: %w", err)
	}
	out := make([]msgconv.BodyRange, len(stored))
	for i, r := range stored {
		out[i] = msgconv.BodyRange{Start: r.Start, Length: r.Length, Style: r.Style, MentionACI: r.MentionACI}
	}
	return out, nil
}

func (m *migration) person(aci string) msgconv.Person {
	name := "Unknown"
	if aci == m.sink.Self {
		name = m.sink.SelfName()
	}
	for _, r := range m.recipients {
		if r.aci == aci && r.name != "" {
			name = r.name
			break
		}
	}
	return msgconv.Person{Name: name, MXID: m.sink.GhostMXID(aci)}
}

// convert turns a stored message into one for the bridge. latest is the timestamp of its latest version, which is how
// Signal refers to an edited message.
func (m *migration) convert(lm legacyMessage, author legacyRecipient, latest int64) (*bridge.ImportedMessage, error) {
	ranges, err := parseRanges(lm.bodyRanges)
	if err != nil {
		return nil, err
	}
	msg := &msgconv.Message{
		Body: lm.body, Ranges: ranges, ViewOnce: lm.viewOnce, FromMe: author.aci == m.sink.Self,
		SentAt: time.UnixMilli(lm.sentAt), ExpiresIn: time.Duration(lm.expireTimerS) * time.Second,
	}
	if lm.quoteAuthorID.Valid && lm.quoteSentAt.Valid {
		msg.QuoteAuthorACI, msg.QuoteSentAt = m.recipients[lm.quoteAuthorID.Int64].aci, lm.quoteSentAt.Int64
	}
	if msg.Attachments, err = m.attachments(lm.id); err != nil {
		return nil, err
	}
	deleted := lm.deletedAt.Valid
	if deleted {
		msg.Notice = "Deleted"
	} else if msg.ViewOnce && len(msg.Attachments) == 0 {
		msg.Notice = "View-once media"
	}
	if msg.IsEmpty() {
		return nil, nil
	}
	converted := msgconv.Convert(msg, m.person)
	if lm.expiresAt.Valid {
		converted.Disappear.DisappearAt = time.UnixMilli(lm.expiresAt.Int64)
	}
	out := &bridge.ImportedMessage{
		ConvertedMessage: converted,
		ID:               signalid.MessageID(author.aci, latest),
		Sender:           m.sink.Sender(author.aci),
		Timestamp:        time.UnixMilli(latest),
	}
	if deleted {
		out.DeletedAt = time.UnixMilli(lm.deletedAt.Int64)
	}
	return out, nil
}

func (m *migration) attachments(messageID int64) ([]msgconv.Attachment, error) {
	rows, err := m.db.Query(`SELECT coalesce(content_type, ''), coalesce(filename, ''), coalesce(size, 0),
			coalesce(width, 0), coalesce(height, 0), coalesce(duration_ms, 0), coalesce(caption, ''),
			coalesce(blurhash, ''), is_voice_note, coalesce(file_sha256, ''), status
		FROM legacy_attachments WHERE message_id = ? ORDER BY ordinal`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []msgconv.Attachment
	for rows.Next() {
		var a msgconv.Attachment
		var sha, status string
		if err = rows.Scan(&a.ContentType, &a.Filename, &a.Size, &a.Width, &a.Height, &a.DurationMS, &a.Caption,
			&a.Blurhash, &a.VoiceNote, &sha, &status); err != nil {
			return nil, err
		}
		// The previous version kept files in the same place, the same way: they're already in the media store.
		if status == "present" && sha != "" && m.fs.Size(sha) >= 0 {
			a.URL = localmx.ContentURI(sha)
		}
		if a.ContentType == "" {
			a.ContentType = "application/octet-stream"
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// revisions adds an edited message's earlier versions, which the previous version kept with when each was sent.
func (m *migration) revisions(lm legacyMessage, msg *bridge.ImportedMessage) error {
	rows, err := m.db.Query(`SELECT coalesce(body, ''), coalesce(body_ranges, ''), edited_at FROM legacy_message_edits
		WHERE message_id = ? ORDER BY revision`, lm.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body, rawRanges string
		var sentAt int64
		if err = rows.Scan(&body, &rawRanges, &sentAt); err != nil {
			return err
		}
		ranges, err := parseRanges(rawRanges)
		if err != nil {
			return err
		}
		if body == "" {
			continue
		}
		parts := msgconv.Convert(&msgconv.Message{Body: body, Ranges: ranges}, m.person).Parts
		msg.Revisions = append(msg.Revisions, bridge.ImportedRevision{
			Timestamp: time.UnixMilli(sentAt), Content: parts[len(parts)-1].Content,
		})
	}
	return rows.Err()
}

func (m *migration) reactions(lm legacyMessage, msg *bridge.ImportedMessage) error {
	rows, err := m.db.Query("SELECT reactor_id, emoji, sent_at FROM legacy_reactions WHERE message_id = ?", lm.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var reactorID, sentAt int64
		var emoji string
		if err = rows.Scan(&reactorID, &emoji, &sentAt); err != nil {
			return err
		}
		if aci := m.recipients[reactorID].aci; signalid.IsACI(aci) {
			msg.Reactions = append(msg.Reactions, &bridgev2.BackfillReaction{
				Sender: m.sink.Sender(aci), Emoji: emoji, Timestamp: time.UnixMilli(sentAt),
			})
		}
	}
	return rows.Err()
}

// receipts records how far the account's own messages got, and whether others' were read.
func (m *migration) receipts(lm legacyMessage, msg *bridge.ImportedMessage, author legacyRecipient, lc *legacyChat) error {
	if author.aci != m.sink.Self {
		msg.Read = lm.sentAt <= lc.lastReadAt
		return nil
	}
	rows, err := m.db.Query("SELECT recipient_id, status, at FROM legacy_receipts WHERE message_id = ?", lm.id)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var recipientID, at int64
		var status string
		if err = rows.Scan(&recipientID, &status, &at); err != nil {
			return err
		}
		aci := m.recipients[recipientID].aci
		switch status {
		case "read", "viewed":
			msg.Status = "delivered"
			if signalid.IsACI(aci) {
				msg.ReadBy = append(msg.ReadBy, bridge.ImportedReceipt{Sender: signalid.UserID(aci), At: time.UnixMilli(at)})
			}
		case "delivered":
			msg.Status = "delivered"
		case "sent":
			if msg.Status == "" {
				msg.Status = "sent"
			}
		}
	}
	return rows.Err()
}

// legacyUpdate is an update's payload as the previous version stored it.
type legacyUpdate struct {
	Type      string `json:"type"`
	Seconds   int64  `json:"seconds"`
	Text      string `json:"text"`
	Direction string `json:"direction"`
	Video     bool   `json:"video"`
	Missed    bool   `json:"missed"`
	GroupName string `json:"groupName"`
}

func (m *migration) updateLines(author legacyRecipient, raw string) []importer.Update {
	var u legacyUpdate
	if raw == "" || json.Unmarshal([]byte(raw), &u) != nil {
		return nil
	}
	name := author.name
	if author.aci == m.sink.Self {
		name = "You"
	} else if name == "" {
		name = "Someone"
	}
	notice := func(body string, quiet bool) []importer.Update {
		return []importer.Update{{MsgType: "notice", Body: body, Quiet: quiet}}
	}
	switch u.Type {
	case "expirationTimer":
		if u.Seconds == 0 {
			return []importer.Update{{AuthorACI: author.aci, MsgType: "disappearing", Body: "turned off disappearing messages"}}
		}
		return []importer.Update{{AuthorACI: author.aci, MsgType: "disappearing",
			Body: "set messages to disappear after " + msgconv.TimerText(time.Duration(u.Seconds)*time.Second)}}
	case "groupUpdate":
		if u.GroupName != "" {
			return []importer.Update{{AuthorACI: author.aci, MsgType: "group", Body: "updated the group “" + u.GroupName + "”"}}
		}
		return []importer.Update{{AuthorACI: author.aci, MsgType: "group", Body: "updated the group"}}
	case "groupCall":
		return notice("📞 Group call", false)
	case "call":
		kind := "voice call"
		if u.Video {
			kind = "video call"
		}
		switch {
		case u.Missed:
			return notice("📞 Missed "+kind, false)
		case u.Direction == "outgoing":
			return notice("📞 Outgoing "+kind, false)
		}
		return notice("📞 Incoming "+kind, false)
	case "joinedSignal":
		return notice(name+" joined Signal", false)
	case "safetyNumberChange":
		return notice("Your safety number with "+name+" changed", true)
	case "chatSessionRefresh":
		return notice("Chat session refreshed", true)
	case "profileChange":
		return notice(name+" changed their profile", true)
	case "simple":
		return notice(u.Text, false)
	}
	return nil
}
