package app

import (
	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
)

// DBOwner marks the bridge database as this app's, so it isn't mistaken for another program's.
const DBOwner = "bottled-signal-client"

// BridgeOptions sets up the bridge with the signal-cli connector. Signal's servers keep no history, so there's nothing
// to backfill: what isn't stored here comes from imported backups.
func BridgeOptions(conn *connector.Connector) bridge.Options {
	return bridge.Options{
		Connector:   conn,
		DBOwner:     DBOwner,
		MaxFileSize: connector.MaxFileSize,
	}
}
