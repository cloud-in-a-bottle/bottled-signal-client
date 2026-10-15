package signalcli

import (
	"context"
	"encoding/base64"
	"fmt"
)

// ListAccounts lists the accounts registered in signal-cli's data directory.
func (c *Client) ListAccounts(ctx context.Context) ([]ListedAccount, error) {
	var out []ListedAccount
	return out, c.Call(ctx, "listAccounts", nil, &out)
}

// Verify finishes registering number with the code Signal sent, and the registration lock PIN if it has one.
func (c *Client) Verify(ctx context.Context, number, code, pin string) error {
	return c.Call(ctx, "verify", struct {
		Account          string `json:"account"`
		VerificationCode string `json:"verificationCode"`
		PIN              string `json:"pin,omitempty"`
	}{number, code, pin}, nil)
}

// Account makes calls as one registered account.
type Account struct {
	c      *Client
	Number string
}

func (c *Client) Account(number string) *Account {
	return &Account{c: c, Number: number}
}

type accountParam struct {
	Account string `json:"account"`
}

func (a *Account) param() accountParam {
	return accountParam{Account: a.Number}
}

// Target says where a message goes: one person (by ACI or number), a group, or the account's own "Note to Self".
type Target struct {
	Recipient  string
	GroupID    string
	NoteToSelf bool
}

type targetParams struct {
	Recipient  []string `json:"recipient,omitempty"`
	GroupID    string   `json:"groupId,omitempty"`
	NoteToSelf bool     `json:"noteToSelf,omitempty"`
}

func (t Target) params() targetParams {
	p := targetParams{GroupID: t.GroupID, NoteToSelf: t.NoteToSelf}
	if t.Recipient != "" {
		p.Recipient = []string{t.Recipient}
	}
	return p
}

type SendParams struct {
	Target
	Message string
	// Paths of files to attach. signal-cli reads them, and names each attachment after its file.
	Attachments []string
	// "start:length:ACI" for each mention, start and length in UTF-16 code units.
	Mentions []string
	// "start:length:STYLE".
	TextStyles     []string
	QuoteTimestamp int64
	QuoteAuthor    string
	QuoteMessage   string
	// Set to send an edit of the message sent at this timestamp.
	EditTimestamp int64
	ViewOnce      bool
}

func (a *Account) Send(ctx context.Context, p SendParams) (SendResult, error) {
	params := struct {
		accountParam
		targetParams
		Message        string   `json:"message,omitempty"`
		Attachments    []string `json:"attachments,omitempty"`
		Mentions       []string `json:"mentions,omitempty"`
		TextStyles     []string `json:"textStyles,omitempty"`
		QuoteTimestamp int64    `json:"quoteTimestamp,omitempty"`
		QuoteAuthor    string   `json:"quoteAuthor,omitempty"`
		QuoteMessage   string   `json:"quoteMessage,omitempty"`
		EditTimestamp  int64    `json:"editTimestamp,omitempty"`
		ViewOnce       bool     `json:"viewOnce,omitempty"`
	}{
		a.param(), p.Target.params(), p.Message, p.Attachments, p.Mentions, p.TextStyles, p.QuoteTimestamp,
		p.QuoteAuthor, p.QuoteMessage, p.EditTimestamp, p.ViewOnce,
	}
	var out SendResult
	return out, a.c.Call(ctx, "send", params, &out)
}

// React sets (or with remove, takes back) a reaction to the message targetAuthor sent at targetTimestamp.
func (a *Account) React(ctx context.Context, to Target, emoji, targetAuthor string, targetTimestamp int64, remove bool) (SendResult, error) {
	params := struct {
		accountParam
		targetParams
		Emoji           string `json:"emoji"`
		TargetAuthor    string `json:"targetAuthor"`
		TargetTimestamp int64  `json:"targetTimestamp"`
		Remove          bool   `json:"remove,omitempty"`
	}{a.param(), to.params(), emoji, targetAuthor, targetTimestamp, remove}
	var out SendResult
	return out, a.c.Call(ctx, "sendReaction", params, &out)
}

// RemoteDelete deletes one of the account's own messages for everyone.
func (a *Account) RemoteDelete(ctx context.Context, to Target, targetTimestamp int64) (SendResult, error) {
	params := struct {
		accountParam
		targetParams
		TargetTimestamp int64 `json:"targetTimestamp"`
	}{a.param(), to.params(), targetTimestamp}
	var out SendResult
	return out, a.c.Call(ctx, "remoteDelete", params, &out)
}

// SendReceipt tells sender that the messages it sent at timestamps were read ("read") or viewed ("viewed").
func (a *Account) SendReceipt(ctx context.Context, sender string, timestamps []int64, receiptType string) error {
	return a.c.Call(ctx, "sendReceipt", struct {
		accountParam
		Recipient       string  `json:"recipient"`
		TargetTimestamp []int64 `json:"targetTimestamp"`
		Type            string  `json:"type"`
	}{a.param(), sender, timestamps, receiptType}, nil)
}

func (a *Account) SendTyping(ctx context.Context, to Target, stop bool) error {
	return a.c.Call(ctx, "sendTyping", struct {
		accountParam
		targetParams
		Stop bool `json:"stop,omitempty"`
	}{a.param(), to.params(), stop}, nil)
}

// Contacts lists everyone signal-cli knows of (not only saved contacts), or just the given recipients.
func (a *Account) Contacts(ctx context.Context, recipients ...string) ([]Contact, error) {
	var out []Contact
	return out, a.c.Call(ctx, "listContacts", struct {
		accountParam
		AllRecipients bool     `json:"allRecipients"`
		Recipient     []string `json:"recipient,omitempty"`
	}{a.param(), true, recipients}, &out)
}

// Groups lists the groups signal-cli knows of, or just the given ones.
func (a *Account) Groups(ctx context.Context, groupIDs ...string) ([]Group, error) {
	var out []Group
	return out, a.c.Call(ctx, "listGroups", struct {
		accountParam
		GroupID []string `json:"groupId,omitempty"`
	}{a.param(), groupIDs}, &out)
}

// UserStatus looks up whether phone numbers are on Signal, and their ACIs.
func (a *Account) UserStatus(ctx context.Context, numbers ...string) ([]UserStatus, error) {
	var out []UserStatus
	return out, a.c.Call(ctx, "getUserStatus", struct {
		accountParam
		Recipient []string `json:"recipient"`
	}{a.param(), numbers}, &out)
}

func (a *Account) UpdateProfile(ctx context.Context, givenName, familyName string) error {
	return a.c.Call(ctx, "updateProfile", struct {
		accountParam
		GivenName  string `json:"givenName"`
		FamilyName string `json:"familyName"`
	}{a.param(), givenName, familyName}, nil)
}

// SetPIN sets the Signal PIN, which turns on registration lock.
func (a *Account) SetPIN(ctx context.Context, pin string) error {
	return a.c.Call(ctx, "setPin", struct {
		accountParam
		PIN string `json:"pin"`
	}{a.param(), pin}, nil)
}

// AddDevice links a device that showed a sgnl://linkdevice URI (as a QR code).
func (a *Account) AddDevice(ctx context.Context, uri string) error {
	return a.c.Call(ctx, "addDevice", struct {
		accountParam
		URI string `json:"uri"`
	}{a.param(), uri}, nil)
}

func (a *Account) Devices(ctx context.Context) ([]Device, error) {
	var out []Device
	return out, a.c.Call(ctx, "listDevices", a.param(), &out)
}

func (a *Account) RemoveDevice(ctx context.Context, deviceID int64) error {
	return a.c.Call(ctx, "removeDevice", struct {
		accountParam
		DeviceID int64 `json:"deviceId"`
	}{a.param(), deviceID}, nil)
}

// ProfileAvatar fetches the profile photo of a person (by ACI or number), as signal-cli has stored it.
func (a *Account) ProfileAvatar(ctx context.Context, recipient string) ([]byte, error) {
	return a.file(ctx, struct {
		accountParam
		Profile string `json:"profile"`
	}{a.param(), recipient}, "getAvatar")
}

// GroupAvatar fetches a group's photo, as signal-cli has stored it.
func (a *Account) GroupAvatar(ctx context.Context, groupID string) ([]byte, error) {
	return a.file(ctx, struct {
		accountParam
		GroupID string `json:"groupId"`
	}{a.param(), groupID}, "getAvatar")
}

// file runs a command whose result is a file's contents, base64-encoded.
func (a *Account) file(ctx context.Context, params any, method string) ([]byte, error) {
	var out AttachmentData
	if err := a.c.Call(ctx, method, params, &out); err != nil {
		return nil, err
	}
	data, err := base64.StdEncoding.DecodeString(out.Data)
	if err != nil {
		return nil, fmt.Errorf("decoding %s result: %w", method, err)
	}
	return data, nil
}

// DeleteLocalData removes the account from signal-cli's data directory, eg once Signal has signed it out.
func (a *Account) DeleteLocalData(ctx context.Context) error {
	return a.c.Call(ctx, "deleteLocalAccountData", struct {
		accountParam
		IgnoreRegistered bool `json:"ignoreRegistered"`
	}{a.param(), true}, nil)
}

// Sticker fetches a sticker's image from the sticker packs signal-cli has downloaded.
func (a *Account) Sticker(ctx context.Context, packID string, stickerID int) ([]byte, error) {
	return a.file(ctx, struct {
		accountParam
		PackID    string `json:"packId"`
		StickerID int    `json:"stickerId"`
	}{a.param(), packID, stickerID}, "getSticker")
}
