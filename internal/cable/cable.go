// Package cable is a minimal ActionCable client. It subscribes to one Turbo
// stream and reports each broadcast as a signal.
package cable

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
)

const staleAfter = 15 * time.Second

type Subscription struct {
	BaseURL      string
	AccountSlug  string
	SessionToken string
	StreamName   string
}

type message struct {
	Type       string          `json:"type"`
	Identifier string          `json:"identifier"`
	Message    json.RawMessage `json:"message"`
	Reason     string          `json:"reason"`
}

var ErrUnauthorized = errors.New("cable: session cookie not accepted")

// Listen connects and blocks until the connection ends. It calls onReady when
// the subscription is confirmed, and onSignal for each broadcast.
func (s Subscription) Listen(ctx context.Context, onReady, onSignal func()) error {
	base, err := url.Parse(s.BaseURL)
	if err != nil {
		return err
	}
	cableURL := *base
	cableURL.Scheme = map[string]string{"http": "ws", "https": "wss"}[base.Scheme]
	cableURL.Path = "/" + s.AccountSlug + "/cable"

	header := http.Header{}
	header.Set("Origin", base.Scheme+"://"+base.Host)
	header.Set("Cookie", "session_token="+s.SessionToken)

	conn, _, err := websocket.Dial(ctx, cableURL.String(), &websocket.DialOptions{
		HTTPHeader:   header,
		Subprotocols: []string{"actioncable-v1-json"},
	})
	if err != nil {
		return fmt.Errorf("cable: connect: %w", err)
	}
	defer conn.CloseNow()
	conn.SetReadLimit(4 << 20)

	identifier, _ := json.Marshal(map[string]string{
		"channel":            "Turbo::StreamsChannel",
		"signed_stream_name": s.StreamName,
	})

	for {
		readCtx, cancel := context.WithTimeout(ctx, staleAfter)
		_, data, err := conn.Read(readCtx)
		cancel()
		if err != nil {
			return fmt.Errorf("cable: read: %w", err)
		}

		var msg message
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "welcome":
			subscribe, _ := json.Marshal(map[string]string{"command": "subscribe", "identifier": string(identifier)})
			if err := conn.Write(ctx, websocket.MessageText, subscribe); err != nil {
				return fmt.Errorf("cable: subscribe: %w", err)
			}
		case "confirm_subscription":
			onReady()
		case "reject_subscription":
			return errors.New("cable: subscription rejected")
		case "disconnect":
			if strings.Contains(msg.Reason, "unauthorized") {
				return ErrUnauthorized
			}
			return fmt.Errorf("cable: disconnected: %s", msg.Reason)
		case "ping":
		default:
			if len(msg.Message) > 0 {
				onSignal()
			}
		}
	}
}
