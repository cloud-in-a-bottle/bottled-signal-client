// Package signalid is how Signal's identifiers map to the bridge's: people by ACI, chats by the other person's ACI or
// the group's ID, and messages by author and sent timestamp, which is how Signal itself refers to a message (eg in
// replies, reactions and edits). It follows mautrix-signal's scheme.
package signalid

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"
)

var aciPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// IsACI reports whether s is an ACI in its canonical form (a lowercase UUID).
func IsACI(s string) bool {
	return aciPattern.MatchString(s)
}

func UserID(aci string) networkid.UserID {
	return networkid.UserID(aci)
}

func LoginID(aci string) networkid.UserLoginID {
	return networkid.UserLoginID(aci)
}

// groupIDLength is the length of a group identifier (32 bytes) in base64.
const groupIDLength = 44

// IsGroupID reports whether s is a group identifier as signal-cli writes it (standard base64 of 32 bytes).
func IsGroupID(s string) bool {
	if len(s) != groupIDLength {
		return false
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	return err == nil && len(raw) == 32
}

// DMPortalKey is the chat with one person (or, for the account's own ACI, "Note to Self"). One-to-one chats belong to
// the login, as in mautrix-signal.
func DMPortalKey(aci string, login networkid.UserLoginID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(aci), Receiver: login}
}

func GroupPortalKey(groupID string) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(groupID)}
}

// Chat says which chat a portal is: a group, or a person (whose ACI it is).
type Chat struct {
	GroupID string
	ACI     string
}

func ParsePortalID(id networkid.PortalID) (Chat, error) {
	switch s := string(id); {
	case IsGroupID(s):
		return Chat{GroupID: s}, nil
	case IsACI(s):
		return Chat{ACI: s}, nil
	default:
		return Chat{}, fmt.Errorf("invalid Signal chat ID %q", s)
	}
}

func MessageID(authorACI string, sentAt int64) networkid.MessageID {
	return networkid.MessageID(fmt.Sprintf("%s|%d", authorACI, sentAt))
}

func ParseMessageID(id networkid.MessageID) (authorACI string, sentAt int64, err error) {
	author, ts, ok := strings.Cut(string(id), "|")
	if !ok || !IsACI(author) {
		return "", 0, fmt.Errorf("invalid Signal message ID %q", id)
	}
	sentAt, err = strconv.ParseInt(ts, 10, 64)
	return author, sentAt, err
}

// PartID numbers the parts of a message (its attachments, then its text), the first being "".
func PartID(index int) networkid.PartID {
	if index == 0 {
		return ""
	}
	return networkid.PartID(strconv.Itoa(index))
}
