package signalcli

// The shapes of signal-cli's JSON output, as of 0.14.9 (src/main/java/org/asamk/signal/json). Fields signal-cli marks
// deprecated (eg "source", "author") are left out in favour of their Number/Uuid replacements.

// Receive is the payload of a "receive" notification.
type Receive struct {
	Account   string    `json:"account"`
	Envelope  *Envelope `json:"envelope"`
	Exception *Error    `json:"exception"`
}

type Error struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type Envelope struct {
	SourceNumber             string          `json:"sourceNumber"`
	SourceUUID               string          `json:"sourceUuid"`
	SourceName               string          `json:"sourceName"`
	SourceDevice             int             `json:"sourceDevice"`
	Timestamp                int64           `json:"timestamp"`
	ServerReceivedTimestamp  int64           `json:"serverReceivedTimestamp"`
	ServerDeliveredTimestamp int64           `json:"serverDeliveredTimestamp"`
	DataMessage              *DataMessage    `json:"dataMessage"`
	EditMessage              *EditMessage    `json:"editMessage"`
	SyncMessage              *SyncMessage    `json:"syncMessage"`
	ReceiptMessage           *ReceiptMessage `json:"receiptMessage"`
	TypingMessage            *TypingMessage  `json:"typingMessage"`
}

type DataMessage struct {
	Timestamp          int64            `json:"timestamp"`
	Message            *string          `json:"message"`
	ExpiresInSeconds   int64            `json:"expiresInSeconds"`
	IsExpirationUpdate bool             `json:"isExpirationUpdate"`
	ViewOnce           bool             `json:"viewOnce"`
	GroupCallUpdate    *GroupCallUpdate `json:"groupCallUpdate"`
	IsEndSession       bool             `json:"isEndSession"`
	IsProfileKeyUpdate bool             `json:"isProfileKeyUpdate"`
	Reaction           *Reaction        `json:"reaction"`
	Quote              *Quote           `json:"quote"`
	Payment            *Payment         `json:"payment"`
	Mentions           []Mention        `json:"mentions"`
	Previews           []Preview        `json:"previews"`
	Attachments        []Attachment     `json:"attachments"`
	Sticker            *Sticker         `json:"sticker"`
	RemoteDelete       *RemoteDelete    `json:"remoteDelete"`
	Contacts           []SharedContact  `json:"contacts"`
	PollCreate         *PollCreate      `json:"pollCreate"`
	PollVote           *PollVote        `json:"pollVote"`
	PollTerminate      *PollTerminate   `json:"pollTerminate"`
	TextStyles         []TextStyle      `json:"textStyles"`
	GroupInfo          *GroupInfo       `json:"groupInfo"`
	StoryContext       *StoryContext    `json:"storyContext"`
	PinMessage         *PinMessage      `json:"pinMessage"`
	UnpinMessage       *PinMessage      `json:"unpinMessage"`
	AdminDelete        *AdminDelete     `json:"adminDelete"`
}

type GroupCallUpdate struct {
	EraID string `json:"eraId"`
}

type Reaction struct {
	Emoji               string `json:"emoji"`
	TargetAuthorNumber  string `json:"targetAuthorNumber"`
	TargetAuthorUUID    string `json:"targetAuthorUuid"`
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
	IsRemove            bool   `json:"isRemove"`
}

type Quote struct {
	ID           int64     `json:"id"`
	AuthorNumber string    `json:"authorNumber"`
	AuthorUUID   string    `json:"authorUuid"`
	Text         string    `json:"text"`
	Mentions     []Mention `json:"mentions"`
}

type Payment struct {
	Note string `json:"note"`
}

type Mention struct {
	Number string `json:"number"`
	UUID   string `json:"uuid"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

type TextStyle struct {
	// BOLD, ITALIC, SPOILER, STRIKETHROUGH or MONOSPACE.
	Style  string `json:"style"`
	Start  int    `json:"start"`
	Length int    `json:"length"`
}

type Preview struct {
	URL         string      `json:"url"`
	Title       string      `json:"title"`
	Description string      `json:"description"`
	Image       *Attachment `json:"image"`
}

type Attachment struct {
	ContentType string `json:"contentType"`
	Filename    string `json:"filename"`
	// The file's name in signal-cli's attachments directory, once downloaded.
	ID          string `json:"id"`
	Size        int64  `json:"size"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Caption     string `json:"caption"`
	IsVoiceNote bool   `json:"isVoiceNote"`
}

type Sticker struct {
	PackID    string `json:"packId"`
	StickerID int    `json:"stickerId"`
}

type RemoteDelete struct {
	TargetTimestamp int64 `json:"targetTimestamp"`
}

type AdminDelete struct {
	TargetAuthorNumber string `json:"targetAuthorNumber"`
	TargetAuthorUUID   string `json:"targetAuthorUuid"`
	TargetTimestamp    int64  `json:"targetTimestamp"`
}

type SharedContact struct {
	Name         *ContactName   `json:"name"`
	Phone        []ContactValue `json:"phone"`
	Email        []ContactValue `json:"email"`
	Organization string         `json:"organization"`
}

type ContactName struct {
	Nickname string `json:"nickname"`
	Given    string `json:"given"`
	Family   string `json:"family"`
}

type ContactValue struct {
	Value string `json:"value"`
}

type PollCreate struct {
	Question      string   `json:"question"`
	AllowMultiple bool     `json:"allowMultiple"`
	Options       []string `json:"options"`
}

type PollVote struct {
	AuthorNumber        string `json:"authorNumber"`
	AuthorUUID          string `json:"authorUuid"`
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
	OptionIndexes       []int  `json:"optionIndexes"`
}

type PollTerminate struct {
	TargetSentTimestamp int64 `json:"targetSentTimestamp"`
}

type GroupInfo struct {
	// Base64 of the 32-byte group identifier.
	GroupID   string `json:"groupId"`
	GroupName string `json:"groupName"`
	Revision  int    `json:"revision"`
	// DELIVER for messages, UPDATE for group changes.
	Type string `json:"type"`
}

type StoryContext struct {
	AuthorNumber  string `json:"authorNumber"`
	AuthorUUID    string `json:"authorUuid"`
	SentTimestamp int64  `json:"sentTimestamp"`
}

type PinMessage struct {
	TargetAuthorNumber  string `json:"targetAuthorNumber"`
	TargetAuthorUUID    string `json:"targetAuthorUuid"`
	TargetSentTimestamp int64  `json:"targetSentTimestamp"`
}

type EditMessage struct {
	TargetSentTimestamp int64        `json:"targetSentTimestamp"`
	DataMessage         *DataMessage `json:"dataMessage"`
}

type SyncMessage struct {
	SentMessage  *SentMessage  `json:"sentMessage"`
	ReadMessages []ReadMessage `json:"readMessages"`
	// CONTACTS_SYNC, GROUPS_SYNC or REQUEST_SYNC.
	Type string `json:"type"`
}

// SentMessage is a message the account sent from another device: a data message (its fields are inlined) plus the
// one-to-one destination, or an edit.
type SentMessage struct {
	DataMessage
	DestinationNumber string       `json:"destinationNumber"`
	DestinationUUID   string       `json:"destinationUuid"`
	EditMessage       *EditMessage `json:"editMessage"`
}

type ReadMessage struct {
	SenderNumber string `json:"senderNumber"`
	SenderUUID   string `json:"senderUuid"`
	Timestamp    int64  `json:"timestamp"`
}

type ReceiptMessage struct {
	When       int64   `json:"when"`
	IsDelivery bool    `json:"isDelivery"`
	IsRead     bool    `json:"isRead"`
	IsViewed   bool    `json:"isViewed"`
	Timestamps []int64 `json:"timestamps"`
}

type TypingMessage struct {
	// STARTED or STOPPED.
	Action    string `json:"action"`
	Timestamp int64  `json:"timestamp"`
	GroupID   string `json:"groupId"`
}

// Address is how signal-cli identifies a person in results: by ACI (UUID) and/or phone number.
type Address struct {
	UUID   string `json:"uuid"`
	Number string `json:"number"`
}

type SendResult struct {
	Timestamp int64                 `json:"timestamp"`
	Results   []RecipientSendResult `json:"results"`
}

type RecipientSendResult struct {
	RecipientAddress Address `json:"recipientAddress"`
	GroupID          string  `json:"groupId"`
	// SUCCESS, NETWORK_FAILURE, UNREGISTERED_FAILURE, IDENTITY_FAILURE, RATE_LIMIT_FAILURE or
	// PROOF_REQUIRED_FAILURE.
	Type string `json:"type"`
}

type Contact struct {
	Number                string          `json:"number"`
	UUID                  string          `json:"uuid"`
	Name                  string          `json:"name"`
	GivenName             string          `json:"givenName"`
	FamilyName            string          `json:"familyName"`
	NickName              string          `json:"nickName"`
	NickGivenName         string          `json:"nickGivenName"`
	NickFamilyName        string          `json:"nickFamilyName"`
	IsBlocked             bool            `json:"isBlocked"`
	IsArchived            bool            `json:"isArchived"`
	MessageExpirationTime int64           `json:"messageExpirationTime"`
	Unregistered          bool            `json:"unregistered"`
	Profile               *ContactProfile `json:"profile"`
}

type ContactProfile struct {
	LastUpdateTimestamp int64  `json:"lastUpdateTimestamp"`
	GivenName           string `json:"givenName"`
	FamilyName          string `json:"familyName"`
	About               string `json:"about"`
	HasAvatar           bool   `json:"hasAvatar"`
}

type Group struct {
	ID                    string        `json:"id"`
	Name                  string        `json:"name"`
	Description           string        `json:"description"`
	IsMember              bool          `json:"isMember"`
	IsBlocked             bool          `json:"isBlocked"`
	MessageExpirationTime int64         `json:"messageExpirationTime"`
	Members               []GroupMember `json:"members"`
	PendingMembers        []Address     `json:"pendingMembers"`
	PermissionEditDetails string        `json:"permissionEditDetails"`
	PermissionSendMessage string        `json:"permissionSendMessage"`
}

type GroupMember struct {
	Number  string `json:"number"`
	UUID    string `json:"uuid"`
	IsAdmin bool   `json:"isAdmin"`
}

type UserStatus struct {
	Recipient    string `json:"recipient"`
	Number       string `json:"number"`
	UUID         string `json:"uuid"`
	IsRegistered bool   `json:"isRegistered"`
}

type ListedAccount struct {
	Number string `json:"number"`
	ACI    string `json:"aci"`
}

type Device struct {
	ID                int64  `json:"id"`
	Name              string `json:"name"`
	CreatedTimestamp  int64  `json:"createdTimestamp"`
	LastSeenTimestamp int64  `json:"lastSeenTimestamp"`
}

type AttachmentData struct {
	// Base64 of the file.
	Data string `json:"data"`
}
