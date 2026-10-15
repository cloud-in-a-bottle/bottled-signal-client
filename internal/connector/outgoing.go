package connector

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/variationselector"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/msgconv"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

func (cl *Client) HandleMatrixMessage(ctx context.Context, msg *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	to, err := cl.target(msg.Portal.ID)
	if err != nil {
		return nil, err
	}
	p := signalcli.SendParams{Target: to}
	content := msg.Content
	switch content.MsgType {
	case event.MsgText, event.MsgNotice, event.MsgEmote:
		p.Message = content.Body
	case event.MsgImage, event.MsgVideo, event.MsgAudio, event.MsgFile:
		dir, path, err := cl.saveOutgoing(ctx, content)
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(dir)
		p.Attachments = []string{path}
		if content.FileName != "" && content.Body != content.FileName {
			p.Message = content.Body
		}
	default:
		return nil, fmt.Errorf("%w %s", bridgev2.ErrUnsupportedMessageType, content.MsgType)
	}
	if msg.ReplyTo != nil {
		if author, ts, err := signalid.ParseMessageID(msg.ReplyTo.ID); err == nil {
			p.QuoteAuthor, p.QuoteTimestamp = author, ts
		}
	}
	res, err := cl.acct.Send(ctx, p)
	if err != nil {
		cl.checkSignedOut(err)
		return nil, bridgev2.WrapErrorInStatus(err).WithSendNotice(true)
	}
	if err = cl.checkSent(ctx, res); err != nil {
		return nil, bridgev2.WrapErrorInStatus(err).WithSendNotice(true)
	}
	resp := &bridgev2.MatrixMessageResponse{DB: &database.Message{
		ID:        signalid.MessageID(cl.aci, res.Timestamp),
		SenderID:  signalid.UserID(cl.aci),
		Timestamp: ms(res.Timestamp),
	}}
	if timer := msg.Portal.Disappear.Timer; timer > 0 {
		// A message you send disappears a set time after it was sent; others' after you read them.
		resp.Disappear = &database.DisappearingSetting{
			Type: event.DisappearingTypeAfterSend, Timer: timer, DisappearAt: ms(res.Timestamp).Add(timer),
		}
	}
	return resp, nil
}

// saveOutgoing writes a file being sent to the temp dir, for signal-cli to read. signal-cli names the attachment
// after the file, so it goes in a directory of its own under its own name.
func (cl *Client) saveOutgoing(ctx context.Context, content *event.MessageEventContent) (dir, path string, err error) {
	dir, err = os.MkdirTemp(cl.c.TempDir, "send-")
	if err != nil {
		return "", "", err
	}
	name := filepath.Base(content.FileName)
	if name == "." || name == "/" || name == "" {
		name = msgconv.DefaultFilename(content.GetInfo().MimeType)
	}
	path = filepath.Join(dir, name)
	err = cl.c.br.Bot.DownloadMediaToFile(ctx, content.URL, content.File, false, func(src *os.File) error {
		dst, err := os.Create(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(dst, src)
		return errors.Join(err, dst.Close())
	})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", fmt.Errorf("reading the attachment: %w", err)
	}
	return dir, path, nil
}

func (cl *Client) HandleMatrixEdit(ctx context.Context, msg *bridgev2.MatrixEdit) error {
	author, ts, err := signalid.ParseMessageID(msg.EditTarget.ID)
	if err != nil {
		return err
	} else if author != cl.aci {
		return errors.New("only your own messages can be edited")
	}
	to, err := cl.target(msg.Portal.ID)
	if err != nil {
		return err
	}
	res, err := cl.acct.Send(ctx, signalcli.SendParams{Target: to, Message: msg.Content.Body, EditTimestamp: ts})
	if err != nil {
		return err
	} else if err = cl.checkSent(ctx, res); err != nil {
		return err
	}
	// Signal refers to the edited message by the edit's timestamp from now on (see convertEdit), so all its parts
	// take that ID.
	prevID := msg.EditTarget.ID
	newID := signalid.MessageID(cl.aci, res.Timestamp)
	msg.EditTarget.ID = newID
	msg.EditTarget.EditCount++
	return cl.c.br.DB.DoTxn(ctx, nil, func(ctx context.Context) error {
		parts, err := cl.c.br.DB.Message.GetAllPartsByID(ctx, msg.Portal.Receiver, prevID)
		if err != nil {
			return err
		}
		for _, part := range parts {
			if part.RowID == msg.EditTarget.RowID || part.PartID == editStubPart {
				continue
			}
			part.ID = newID
			if err = cl.c.br.DB.Message.Update(ctx, part); err != nil {
				return err
			}
		}
		if err = cl.c.br.DB.Message.Update(ctx, msg.EditTarget); err != nil {
			return err
		}
		stub := &database.Message{
			ID: prevID, PartID: editStubPart, Room: msg.EditTarget.Room, SenderID: msg.EditTarget.SenderID,
			SenderMXID: msg.EditTarget.SenderMXID, Timestamp: msg.EditTarget.Timestamp,
		}
		stub.SetFakeMXID()
		return cl.c.br.DB.Message.Insert(ctx, stub)
	})
}

func (cl *Client) PreHandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (bridgev2.MatrixReactionPreResponse, error) {
	// One reaction per person per message, as on Signal: a new one replaces the last.
	return bridgev2.MatrixReactionPreResponse{
		SenderID: signalid.UserID(cl.aci),
		Emoji:    variationselector.FullyQualify(msg.Content.RelatesTo.Key),
	}, nil
}

func (cl *Client) HandleMatrixReaction(ctx context.Context, msg *bridgev2.MatrixReaction) (*database.Reaction, error) {
	return &database.Reaction{}, cl.react(ctx, msg.Portal.ID, msg.TargetMessage.ID, msg.PreHandleResp.Emoji, false)
}

func (cl *Client) HandleMatrixReactionRemove(ctx context.Context, msg *bridgev2.MatrixReactionRemove) error {
	return cl.react(ctx, msg.Portal.ID, msg.TargetReaction.MessageID, msg.TargetReaction.Emoji, true)
}

func (cl *Client) react(ctx context.Context, portalID networkid.PortalID, target networkid.MessageID, emoji string, remove bool) error {
	author, ts, err := signalid.ParseMessageID(target)
	if err != nil {
		return err
	}
	to, err := cl.target(portalID)
	if err != nil {
		return err
	}
	res, err := cl.acct.React(ctx, to, emoji, author, ts, remove)
	if err != nil {
		return err
	}
	return cl.checkSent(ctx, res)
}

func (cl *Client) HandleMatrixMessageRemove(ctx context.Context, msg *bridgev2.MatrixMessageRemove) error {
	author, ts, err := signalid.ParseMessageID(msg.TargetMessage.ID)
	if err != nil {
		return err
	} else if author != cl.aci {
		return errors.New("only your own messages can be deleted for everyone")
	}
	to, err := cl.target(msg.Portal.ID)
	if err != nil {
		return err
	}
	res, err := cl.acct.RemoteDelete(ctx, to, ts)
	if err != nil {
		return err
	}
	return cl.checkSent(ctx, res)
}

// HandleMatrixReadReceipt tells the authors of the messages up to the receipt that they were read.
func (cl *Client) HandleMatrixReadReceipt(ctx context.Context, receipt *bridgev2.MatrixReadReceipt) error {
	if !receipt.ReadUpTo.After(receipt.LastRead) {
		return nil
	}
	since := receipt.LastRead
	if since.IsZero() {
		since = receipt.ReadUpTo.Add(-24 * time.Hour)
	}
	messages, err := cl.c.br.DB.Message.GetMessagesBetweenTimeQuery(ctx, receipt.Portal.PortalKey, since, receipt.ReadUpTo)
	if err != nil {
		return err
	}
	byAuthor := make(map[string][]int64)
	for _, m := range messages {
		author, ts, err := signalid.ParseMessageID(m.ID)
		if err != nil || author == cl.aci || m.PartID == editStubPart {
			continue
		}
		if len(byAuthor[author]) == 0 || byAuthor[author][len(byAuthor[author])-1] != ts {
			byAuthor[author] = append(byAuthor[author], ts)
		}
	}
	for author, timestamps := range byAuthor {
		if err = cl.acct.SendReceipt(ctx, author, timestamps, "read"); err != nil {
			zerolog.Ctx(ctx).Warn().Err(err).Str("author", author).Msg("Failed to send read receipt")
		}
	}
	return nil
}

func (cl *Client) HandleMatrixTyping(ctx context.Context, msg *bridgev2.MatrixTyping) error {
	to, err := cl.target(msg.Portal.ID)
	if err != nil || to.NoteToSelf {
		return err
	}
	return cl.acct.SendTyping(ctx, to, !msg.IsTyping)
}

// HandleMatrixViewLimitedMedia tells the sender a view-once message was opened.
func (cl *Client) HandleMatrixViewLimitedMedia(ctx context.Context, msg *bridgev2.MatrixViewLimitedMedia) error {
	if msg.Content == nil || *msg.Content != (event.BeeperViewLimitedMedia{Type: "count", Count: 1}) {
		return bridgev2.ErrUnsupportedViewLimitedType
	}
	author, ts, err := signalid.ParseMessageID(msg.Message.ID)
	if err != nil {
		return err
	} else if author == cl.aci {
		return nil
	}
	return cl.acct.SendReceipt(ctx, author, []int64{ts}, "viewed")
}

// checkSignedOut notices Signal refusing the account, eg after the number was registered on a phone again.
func (cl *Client) checkSignedOut(err error) {
	var rpcErr *signalcli.RPCError
	if errors.As(err, &rpcErr) && (containsAny(rpcErr.Message, "Authorization failed", "[401]", "[403]", "not registered")) {
		cl.login.BridgeState.Send(statusSignedOut(rpcErr.Message))
	}
}

func statusSignedOut(message string) status.BridgeState {
	return status.BridgeState{StateEvent: status.StateBadCredentials, Message: message}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}
