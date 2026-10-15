package connector

import (
	"context"
	"time"

	"go.mau.fi/util/jsontime"
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"
)

// Signal takes any file; its apps show what they can.
var anyFile = &event.FileFeatures{
	MimeTypes: map[string]event.CapabilitySupportLevel{"*/*": event.CapLevelFullySupported},
	MaxSize:   MaxFileSize,
	Caption:   event.CapLevelFullySupported,
}

var groupFeatures = &event.RoomFeatures{
	ID: "com.github.cloud-in-a-bottle.signal.2026_10",
	File: event.FileFeatureMap{
		event.MsgImage: anyFile,
		event.MsgVideo: anyFile,
		event.MsgAudio: anyFile,
		event.MsgFile:  anyFile,
	},
	Reply:               event.CapLevelFullySupported,
	Edit:                event.CapLevelFullySupported,
	EditMaxCount:        10,
	EditMaxAge:          ptr.Ptr(jsontime.S(24 * time.Hour)),
	Delete:              event.CapLevelFullySupported,
	DeleteMaxAge:        ptr.Ptr(jsontime.S(24 * time.Hour)),
	Reaction:            event.CapLevelFullySupported,
	ReactionCount:       1,
	ReadReceipts:        true,
	TypingNotifications: true,
	DisappearingTimer:   &event.DisappearingTimerCapability{Types: []event.DisappearingType{event.DisappearingTypeAfterRead}},
}

var dmFeatures, noteToSelfFeatures *event.RoomFeatures

func init() {
	dmFeatures = ptr.Clone(groupFeatures)
	dmFeatures.ID += "+dm"
	noteToSelfFeatures = ptr.Clone(dmFeatures)
	noteToSelfFeatures.ID += "+note_to_self"
	// Signal lets you edit and delete what's in Note to Self whenever.
	noteToSelfFeatures.EditMaxAge = nil
	noteToSelfFeatures.DeleteMaxAge = nil
	noteToSelfFeatures.TypingNotifications = false
}

func (cl *Client) GetCapabilities(ctx context.Context, portal *bridgev2.Portal) *event.RoomFeatures {
	switch {
	case portal.ID == networkid.PortalID(cl.aci):
		return noteToSelfFeatures
	case portal.RoomType == database.RoomTypeDM:
		return dmFeatures
	}
	return groupFeatures
}
