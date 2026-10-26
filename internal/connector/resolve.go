package connector

import (
	"context"
	"fmt"
	"strings"

	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalid"
)

// ResolveIdentifier finds someone by phone number (or ACI) and their one-to-one chat.
func (cl *Client) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	identifier = strings.TrimSpace(identifier)
	aci := strings.ToLower(identifier)
	if !signalid.IsACI(aci) {
		number := strings.NewReplacer(" ", "", "-", "", "(", "", ")", "").Replace(identifier)
		if !e164.MatchString(number) {
			return nil, fmt.Errorf("enter a phone number with its country code, eg +15551234567")
		}
		statuses, err := cl.acct.UserStatus(ctx, number)
		if err != nil {
			return nil, err
		} else if len(statuses) != 1 || !statuses[0].IsRegistered || !signalid.IsACI(statuses[0].UUID) {
			return nil, fmt.Errorf("%s isn't on Signal", number)
		}
		aci = statuses[0].UUID
	}
	return cl.resolveACI(ctx, aci, createChat)
}

func (cl *Client) resolveACI(ctx context.Context, aci string, createChat bool) (*bridgev2.ResolveIdentifierResponse, error) {
	ghost, err := cl.c.br.GetGhostByID(ctx, signalid.UserID(aci))
	if err != nil {
		return nil, err
	}
	resp := &bridgev2.ResolveIdentifierResponse{Ghost: ghost, UserID: signalid.UserID(aci)}
	if info, err := cl.GetUserInfo(ctx, ghost); err == nil {
		resp.UserInfo = info
	}
	if createChat {
		info, err := cl.dmInfo(ctx, aci)
		if err != nil {
			return nil, err
		}
		resp.Chat = &bridgev2.CreateChatResponse{PortalKey: signalid.DMPortalKey(aci, cl.login.ID), PortalInfo: info}
	}
	return resp, nil
}

func (cl *Client) CreateChatWithGhost(ctx context.Context, ghost *bridgev2.Ghost) (*bridgev2.CreateChatResponse, error) {
	resp, err := cl.resolveACI(ctx, string(ghost.ID), true)
	if err != nil {
		return nil, err
	}
	return resp.Chat, nil
}

// GetContactList lists the people signal-cli has as contacts (those with a name you gave them, or in your phone's
// address book, as synced from the phone when it was the primary).
func (cl *Client) GetContactList(ctx context.Context) ([]*bridgev2.ResolveIdentifierResponse, error) {
	contacts, err := cl.acct.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	var out []*bridgev2.ResolveIdentifierResponse
	for _, c := range contacts {
		if c.Name == "" && c.NickName == "" && c.NickGivenName == "" {
			continue
		}
		if r := cl.contactResponse(c); r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

// SearchUsers searches the people signal-cli knows of by name or number. Signal itself has no directory to search,
// beyond looking up a whole phone number (New chat › phone number).
func (cl *Client) SearchUsers(ctx context.Context, query string) ([]*bridgev2.ResolveIdentifierResponse, error) {
	contacts, err := cl.acct.Contacts(ctx)
	if err != nil {
		return nil, err
	}
	query = strings.ToLower(strings.TrimSpace(query))
	var out []*bridgev2.ResolveIdentifierResponse
	for _, c := range contacts {
		if c.IsBlocked || c.Unregistered {
			continue
		}
		name := ContactName(c)
		if !strings.Contains(strings.ToLower(name), query) && !strings.Contains(c.Number, query) {
			continue
		}
		if r := cl.contactResponse(c); r != nil {
			out = append(out, r)
		}
	}
	return out, nil
}

func (cl *Client) contactResponse(c signalcli.Contact) *bridgev2.ResolveIdentifierResponse {
	if !signalid.IsACI(c.UUID) || c.UUID == cl.aci {
		return nil
	}
	r := &bridgev2.ResolveIdentifierResponse{
		UserID:   signalid.UserID(c.UUID),
		UserInfo: &bridgev2.UserInfo{Name: ptr.Ptr(ContactName(c))},
	}
	if c.Number != "" {
		r.UserInfo.Identifiers = []string{"tel:" + c.Number}
	}
	return r
}
