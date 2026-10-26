// Package app is what makes the core into the Signal app: its branding, the bridge's settings, and the extra routes
// and settings sections.
package app

import (
	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
)

var Branding = web.Branding{
	Title:       "Bottled Signal",
	Network:     "Signal",
	AccentColor: "#3a76f0",
	LoginIntro: "This server becomes your number's primary Signal device, like a phone, and keeps the only copy of " +
		"your chats (Signal's servers don't keep history). You can link your phone to it afterwards.",
	LoginFlows: []web.LoginFlowLabel{
		{ID: connector.FlowRegister, Label: "Register a phone number",
			Hint: "Signal sends the number a code. Whatever phone is the number's primary device now is signed out."},
		{ID: connector.FlowExisting, Label: "Use the account already on this server",
			Hint: "signal-cli on this server is still registered, eg after logging out here."},
	},
	LoginFallbackHint: "Cancel and try again.",
	HistoryHelp: "Signal's servers don't keep message history: this server has what arrived since it was registered. " +
		"To bring in older history, import a backup from your phone (above).",
	LogoutHint: "Logging out disconnects this server from your Signal account, but signal-cli stays registered as its " +
		"primary device: messages wait on Signal's servers until you log back in (with the account already on this " +
		"server), and your chats here are kept. To give the number back to a phone, use Move history to a phone.",
	IdentifierLabel:       "Phone number",
	IdentifierPlaceholder: "Phone number, eg +1 555 123 4567",
	Extensions: []web.Extension{
		{Module: "account.js", Events: []string{}},
		{Module: "import.js", Events: []string{"import"}},
		{Module: "export.js", Events: []string{"export"}},
	},
}

// Messaging describes Signal to apps using the messaging service. People's remote IDs are their ACIs (UUIDs).
var Messaging = web.MessagingOptions{
	Network:            "signal",
	StartsChatsByPhone: true,
	DeliveryReceipts:   true,
}
