// Package signaltest runs the app's bridge against a fake signal-cli daemon: the real core (bridge, local connector,
// store) and this app's connector, with the daemon's JSON-RPC calls and event stream simulated. Tests push envelopes
// as signal-cli would deliver them and check what was stored, or what the connector asked signal-cli to do.
package signaltest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
)

// Call is a JSON-RPC call the daemon received.
type Call struct {
	Method string
	Params json.RawMessage
}

// Daemon fakes signal-cli's daemon: its HTTP JSON-RPC API and event stream.
type Daemon struct {
	srv *httptest.Server

	mu       sync.Mutex
	Accounts []signalcli.ListedAccount
	Contacts []signalcli.Contact
	Groups   []signalcli.Group
	calls    []Call
	// The next send's timestamp; each send takes the next millisecond.
	nextTimestamp int64
	streams       []chan string
	streamOpened  *sync.Cond
}

func NewDaemon(t *testing.T) *Daemon {
	d := &Daemon{nextTimestamp: 1_800_000_000_000}
	d.streamOpened = sync.NewCond(&d.mu)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/check", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("POST /api/v1/rpc", d.rpc)
	mux.HandleFunc("GET /api/v1/events", d.events)
	d.srv = httptest.NewServer(mux)
	t.Cleanup(d.close)
	return d
}

func (d *Daemon) close() {
	d.mu.Lock()
	for _, ch := range d.streams {
		close(ch)
	}
	d.streams = nil
	d.mu.Unlock()
	d.srv.Close()
}

func (d *Daemon) URL() string {
	return d.srv.URL
}

// Calls lists the calls of a method so far, oldest first.
func (d *Daemon) Calls(method string) []json.RawMessage {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out []json.RawMessage
	for _, c := range d.calls {
		if c.Method == method {
			out = append(out, c.Params)
		}
	}
	return out
}

// Push delivers an envelope on the event stream, as signal-cli does when a message arrives. It waits for a stream to
// be connected.
func (d *Daemon) Push(r signalcli.Receive) {
	data, err := json.Marshal(r)
	if err != nil {
		panic(err)
	}
	d.mu.Lock()
	for len(d.streams) == 0 {
		d.streamOpened.Wait()
	}
	streams := slices.Clone(d.streams)
	d.mu.Unlock()
	for _, ch := range streams {
		ch <- string(data)
	}
}

func (d *Daemon) events(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	w.(http.Flusher).Flush()
	ch := make(chan string)
	d.mu.Lock()
	d.streams = append(d.streams, ch)
	d.streamOpened.Broadcast()
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.streams = slices.DeleteFunc(d.streams, func(c chan string) bool { return c == ch })
		d.mu.Unlock()
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		case data, open := <-ch:
			if !open {
				return
			}
			_, _ = fmt.Fprintf(w, "event:receive\ndata:%s\n\n", data)
			w.(http.Flusher).Flush()
		}
	}
}

type rpcRequest struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      int64     `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

// params are the parameters the fake looks at.
type params struct {
	Recipient []string `json:"recipient"`
	// A group ID, or a list of them.
	GroupID json.RawMessage `json:"groupId"`
}

func (d *Daemon) rpc(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var p params
	_ = json.Unmarshal(req.Params, &p)
	d.mu.Lock()
	d.calls = append(d.calls, Call{Method: req.Method, Params: req.Params})
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "listAccounts":
		resp.Result = d.Accounts
	case "listContacts":
		contacts := []signalcli.Contact{}
		for _, c := range d.Contacts {
			if len(p.Recipient) == 0 || slices.Contains(p.Recipient, c.UUID) || slices.Contains(p.Recipient, c.Number) {
				contacts = append(contacts, c)
			}
		}
		resp.Result = contacts
	case "listGroups":
		var wanted []string
		if len(p.GroupID) > 0 && json.Unmarshal(p.GroupID, &wanted) != nil {
			var one string
			_ = json.Unmarshal(p.GroupID, &one)
			wanted = []string{one}
		}
		groups := []signalcli.Group{}
		for _, g := range d.Groups {
			if len(wanted) == 0 || slices.Contains(wanted, g.ID) {
				groups = append(groups, g)
			}
		}
		resp.Result = groups
	case "send", "sendReaction", "remoteDelete":
		d.nextTimestamp++
		result := signalcli.SendResult{Timestamp: d.nextTimestamp, Results: []signalcli.RecipientSendResult{}}
		for _, recipient := range p.Recipient {
			result.Results = append(result.Results, signalcli.RecipientSendResult{
				RecipientAddress: signalcli.Address{UUID: recipient}, Type: "SUCCESS",
			})
		}
		resp.Result = result
	case "sendReceipt", "sendTyping", "updateProfile", "setPin", "addDevice", "removeDevice":
		resp.Result = map[string]any{}
	case "listDevices":
		resp.Result = []signalcli.Device{{ID: 1, Name: "signal-cli"}}
	case "getUserStatus":
		statuses := []signalcli.UserStatus{}
		for _, number := range p.Recipient {
			s := signalcli.UserStatus{Recipient: number, Number: number}
			for _, c := range d.Contacts {
				if c.Number == number {
					s.UUID, s.IsRegistered = c.UUID, true
				}
			}
			statuses = append(statuses, s)
		}
		resp.Result = statuses
	default:
		resp.Error = &rpcError{Code: -1, Message: "not found: " + req.Method}
	}
	d.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
