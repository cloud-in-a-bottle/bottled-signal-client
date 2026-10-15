package signalcli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// FollowEvents follows the daemon's event stream until ctx ends, reconnecting when it drops, and hands each received
// envelope to handle. connected is told when the stream connects (nil) and when it drops (why). The daemon fixes the
// accounts a stream covers when it opens, so cancel and follow again after an account is registered.
func (c *Client) FollowEvents(ctx context.Context, handle func(Receive), connected func(error), log zerolog.Logger) {
	for ctx.Err() == nil {
		err := follow(ctx, c.base, handle, connected, log)
		if ctx.Err() != nil {
			return
		}
		connected(err)
		log.Warn().Err(err).Msg("signal-cli event stream dropped; reconnecting")
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}
}

func follow(ctx context.Context, baseURL string, handle func(Receive), connected func(error), log zerolog.Logger) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/v1/events", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	log.Info().Msg("signal-cli event stream connected")
	connected(nil)
	scanner := bufio.NewScanner(resp.Body)
	// An envelope's JSON (eg a long message with many mentions) can exceed bufio's default line limit.
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var name string
	var data []string
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if name == "receive" && len(data) > 0 {
				dispatch(strings.Join(data, "\n"), handle, log)
			}
			name, data = "", nil
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err = scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("stream ended")
}

func dispatch(raw string, handle func(Receive), log zerolog.Logger) {
	var r Receive
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		// Losing an envelope silently would be worse, but one malformed envelope mustn't stop all the others.
		log.Error().Err(err).Str("data", truncate(raw, 2000)).Msg("Failed to decode signal-cli event")
		return
	}
	handle(r)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
