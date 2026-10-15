package connector

import (
	"context"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/simplevent"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// Signal's apps stop showing someone as typing after this long without an update.
const typingTimeout = 15 * time.Second

func (cl *Client) handleReceive(ctx context.Context, r signalcli.Receive) {
	log := zerolog.Ctx(ctx)
	if r.Account != cl.acct.Number {
		// The daemon's stream carries every account signal-cli has (eg one logged out of here before registering
		// another number).
		return
	}
	if r.Exception != nil {
		log.Warn().Str("type", r.Exception.Type).Str("message", r.Exception.Message).Msg("signal-cli couldn't process an envelope")
	}
	env := r.Envelope
	if env == nil {
		return
	}
	sender := env.SourceUUID
	if !signalid.IsACI(sender) {
		// Everything since 2022 comes with the sender's ACI; without one there's nobody to attribute it to.
		if env.DataMessage != nil || env.SyncMessage != nil {
			log.Warn().Str("source_number", env.SourceNumber).Msg("Dropping envelope without a sender ACI")
		}
		return
	}
	switch {
	case env.TypingMessage != nil:
		cl.handleTyping(sender, env.TypingMessage)
	case env.ReceiptMessage != nil:
		cl.handleReceipt(ctx, sender, env.ReceiptMessage)
	case env.DataMessage != nil:
		cl.handleData(ctx, sender, cl.chatOf(env.DataMessage, sender), env.DataMessage)
	case env.EditMessage != nil && env.EditMessage.DataMessage != nil:
		cl.handleEdit(sender, cl.chatOf(env.EditMessage.DataMessage, sender), env.EditMessage)
	case env.SyncMessage != nil:
		cl.handleSync(ctx, env.SyncMessage)
	}
}

// chatOf is the chat a message from sender belongs in: its group, else the one-to-one chat with dmWith.
func (cl *Client) chatOf(d *signalcli.DataMessage, dmWith string) networkid.PortalKey {
	if d.GroupInfo != nil && signalid.IsGroupID(d.GroupInfo.GroupID) {
		return signalid.GroupPortalKey(d.GroupInfo.GroupID)
	}
	return signalid.DMPortalKey(dmWith, cl.login.ID)
}

func (cl *Client) handleSync(ctx context.Context, sync *signalcli.SyncMessage) {
	if sent := sync.SentMessage; sent != nil {
		dest := sent.DestinationUUID
		if sent.DestinationNumber == cl.acct.Number {
			// A message to Note to Self from another device.
			dest = cl.aci
		}
		if !signalid.IsACI(dest) && sent.GroupInfo == nil {
			zerolog.Ctx(ctx).Warn().Str("destination_number", sent.DestinationNumber).Msg("Dropping a sent message without a recipient ACI")
			return
		}
		data := &sent.DataMessage
		if sent.EditMessage != nil && sent.EditMessage.DataMessage != nil {
			cl.handleEdit(cl.aci, cl.chatOf(sent.EditMessage.DataMessage, dest), sent.EditMessage)
		} else if dest != "" || data.GroupInfo != nil {
			cl.handleData(ctx, cl.aci, cl.chatOf(data, dest), data)
		}
	}
	if len(sync.ReadMessages) > 0 {
		var ids []networkid.MessageID
		for _, read := range sync.ReadMessages {
			if signalid.IsACI(read.SenderUUID) {
				ids = append(ids, signalid.MessageID(read.SenderUUID, read.Timestamp))
			}
		}
		cl.dispatchReceipts(ctx, cl.aci, bridgev2.RemoteEventReadReceipt, ids)
	}
}

func (cl *Client) handleTyping(sender string, t *signalcli.TypingMessage) {
	key := signalid.DMPortalKey(sender, cl.login.ID)
	if signalid.IsGroupID(t.GroupID) {
		key = signalid.GroupPortalKey(t.GroupID)
	}
	timeout := time.Duration(0)
	if t.Action == "STARTED" {
		timeout = typingTimeout
	}
	cl.login.QueueRemoteEvent(&simplevent.Typing{
		EventMeta: simplevent.EventMeta{Type: bridgev2.RemoteEventTyping, PortalKey: key, Sender: cl.sender(sender)},
		Timeout:   timeout,
	})
}

func (cl *Client) handleReceipt(ctx context.Context, sender string, r *signalcli.ReceiptMessage) {
	evtType := bridgev2.RemoteEventReadReceipt
	if r.IsDelivery {
		evtType = bridgev2.RemoteEventDeliveryReceipt
	} else if !r.IsRead && !r.IsViewed {
		return
	}
	ids := make([]networkid.MessageID, len(r.Timestamps))
	for i, ts := range r.Timestamps {
		ids[i] = signalid.MessageID(cl.aci, ts)
	}
	cl.dispatchReceipts(ctx, sender, evtType, ids)
}

// dispatchReceipts sends receipts for messages, which may be in several chats: Signal's receipts name only the
// messages.
func (cl *Client) dispatchReceipts(ctx context.Context, sender string, evtType bridgev2.RemoteEventType, ids []networkid.MessageID) {
	byChat := make(map[networkid.PortalKey]*simplevent.Receipt)
	latest := make(map[networkid.PortalKey]time.Time)
	for _, id := range ids {
		msg, err := cl.c.br.DB.Message.GetFirstPartByID(ctx, cl.login.ID, id)
		if err != nil {
			zerolog.Ctx(ctx).Err(err).Msg("Failed to look up receipt target")
			continue
		} else if msg == nil {
			continue
		}
		r, ok := byChat[msg.Room]
		if !ok {
			r = &simplevent.Receipt{EventMeta: simplevent.EventMeta{Type: evtType, PortalKey: msg.Room, Sender: cl.sender(sender)}}
			byChat[msg.Room] = r
		}
		r.Targets = append(r.Targets, msg.ID)
		if msg.Timestamp.After(latest[msg.Room]) {
			latest[msg.Room] = msg.Timestamp
			r.LastTarget = msg.ID
		}
	}
	for _, r := range byChat {
		cl.login.QueueRemoteEvent(r)
	}
}

func (cl *Client) handleEdit(sender string, chat networkid.PortalKey, e *signalcli.EditMessage) {
	data := e.DataMessage
	cl.login.QueueRemoteEvent(&simplevent.Message[*signalcli.DataMessage]{
		EventMeta: simplevent.EventMeta{
			Type:      bridgev2.RemoteEventEdit,
			PortalKey: chat,
			Sender:    cl.sender(sender),
			Timestamp: ms(data.Timestamp),
			LogContext: func(c zerolog.Context) zerolog.Context {
				return c.Int64("edit_target_ts", e.TargetSentTimestamp).Int64("edit_ts", data.Timestamp)
			},
		},
		Data:          data,
		ID:            signalid.MessageID(sender, data.Timestamp),
		TargetMessage: signalid.MessageID(sender, e.TargetSentTimestamp),
		ConvertEditFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, existing []*database.Message, data *signalcli.DataMessage) (*bridgev2.ConvertedEdit, error) {
			return cl.convertEdit(ctx, portal, intent, existing, sender, data)
		},
	})
}

func (cl *Client) handleData(ctx context.Context, sender string, chat networkid.PortalKey, d *signalcli.DataMessage) {
	meta := simplevent.EventMeta{
		PortalKey: chat,
		Sender:    cl.sender(sender),
		Timestamp: ms(d.Timestamp),
		LogContext: func(c zerolog.Context) zerolog.Context {
			return c.Str("sender_id", sender).Int64("message_ts", d.Timestamp)
		},
	}
	switch {
	case d.Reaction != nil:
		cl.handleReaction(meta, d.Reaction)
	case d.RemoteDelete != nil:
		meta.Type = bridgev2.RemoteEventMessageRemove
		cl.login.QueueRemoteEvent(&simplevent.MessageRemove{
			EventMeta: meta, TargetMessage: signalid.MessageID(sender, d.RemoteDelete.TargetTimestamp),
		})
	case d.AdminDelete != nil && signalid.IsACI(d.AdminDelete.TargetAuthorUUID):
		meta.Type = bridgev2.RemoteEventMessageRemove
		cl.login.QueueRemoteEvent(&simplevent.MessageRemove{
			EventMeta: meta, TargetMessage: signalid.MessageID(d.AdminDelete.TargetAuthorUUID, d.AdminDelete.TargetTimestamp),
		})
	case d.GroupInfo != nil && d.GroupInfo.Type == "UPDATE":
		cl.handleGroupUpdate(ctx, meta, d.GroupInfo.GroupID)
	case d.IsExpirationUpdate:
		meta.Type = bridgev2.RemoteEventChatInfoChange
		meta.CreatePortal = true
		cl.login.QueueRemoteEvent(&simplevent.ChatInfoChange{
			EventMeta:      meta,
			ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: &bridgev2.ChatInfo{Disappear: disappearing(d.ExpiresInSeconds)}},
		})
	default:
		if !hasContent(d) {
			// Profile key updates, session resets, poll votes, pins: nothing to show.
			return
		}
		meta.Type = bridgev2.RemoteEventMessage
		meta.CreatePortal = true
		cl.login.QueueRemoteEvent(&simplevent.Message[*signalcli.DataMessage]{
			EventMeta: meta,
			Data:      d,
			ID:        signalid.MessageID(sender, d.Timestamp),
			ConvertMessageFunc: func(ctx context.Context, portal *bridgev2.Portal, intent bridgev2.MatrixAPI, data *signalcli.DataMessage) (*bridgev2.ConvertedMessage, error) {
				return cl.convertMessage(ctx, portal, intent, sender, data), nil
			},
		})
	}
}

func hasContent(d *signalcli.DataMessage) bool {
	return d.Message != nil || len(d.Attachments) > 0 || d.Sticker != nil || len(d.Contacts) > 0 ||
		d.PollCreate != nil || d.Payment != nil || d.GroupCallUpdate != nil || d.ViewOnce
}

func (cl *Client) handleReaction(meta simplevent.EventMeta, r *signalcli.Reaction) {
	if !signalid.IsACI(r.TargetAuthorUUID) {
		return
	}
	meta.Type = bridgev2.RemoteEventReaction
	if r.IsRemove {
		meta.Type = bridgev2.RemoteEventReactionRemove
	}
	cl.login.QueueRemoteEvent(&simplevent.Reaction{
		EventMeta:     meta,
		TargetMessage: signalid.MessageID(r.TargetAuthorUUID, r.TargetSentTimestamp),
		Emoji:         r.Emoji,
	})
}

// handleGroupUpdate applies a change to a group (its name, members, ...) as signal-cli now has it: the update itself
// only says that the group changed.
func (cl *Client) handleGroupUpdate(ctx context.Context, meta simplevent.EventMeta, groupID string) {
	groups, err := cl.acct.Groups(ctx, groupID)
	if err != nil || len(groups) != 1 {
		zerolog.Ctx(ctx).Err(err).Int("found", len(groups)).Msg("Failed to get updated group")
		return
	}
	meta.Type = bridgev2.RemoteEventChatInfoChange
	meta.CreatePortal = true
	cl.login.QueueRemoteEvent(&simplevent.ChatInfoChange{
		EventMeta:      meta,
		ChatInfoChange: &bridgev2.ChatInfoChange{ChatInfo: cl.groupInfo(ctx, groups[0])},
	})
}
