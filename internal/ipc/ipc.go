// Package ipc connects the MCP server of a session to the daemon.
package ipc

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"net"
	"time"
)

const (
	OpReplied  = "replied"
	OpProgress = "progress"
	OpMessage  = "message"
	OpApprove  = "approve"
	// OpCheck asks, after a tool call, for what happened on the card since
	// the turn started: new comments, and whether a progress note is due.
	OpCheck = "check"

	ioTimeout      = 10 * time.Second
	maxRequestSize = 1 << 20
)

// Request carries the token of a turn. The daemon made the token, and it
// decides the card and the hop count from it, not from the request.
type Request struct {
	Op     string `json:"op"`
	Token  string `json:"token"`
	ToCard int    `json:"to_card,omitempty"`
	Text   string `json:"text,omitempty"`

	ToolName  string          `json:"tool_name,omitempty"`
	ToolInput json.RawMessage `json:"tool_input,omitempty"`
}

type Response struct {
	Error    string `json:"error,omitempty"`
	Approved bool   `json:"approved,omitempty"`
	Message  string `json:"message,omitempty"`
}

func Send(socketPath string, request Request) error {
	_, err := SendAndWait(socketPath, request, ioTimeout)
	return err
}

// SendAndWait waits up to the given time for the answer of the daemon.
func SendAndWait(socketPath string, request Request, wait time.Duration) (Response, error) {
	var response Response
	conn, err := net.DialTimeout("unix", socketPath, ioTimeout)
	if err != nil {
		return response, err
	}
	defer conn.Close()

	conn.SetWriteDeadline(time.Now().Add(ioTimeout))
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return response, err
	}
	conn.SetReadDeadline(time.Now().Add(wait))
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&response); err != nil {
		return response, err
	}
	if response.Error != "" {
		return response, errors.New(response.Error)
	}
	return response, nil
}

// Serve handles one request for each connection until the listener closes.
func Serve(listener net.Listener, handle func(Request) Response) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()

			conn.SetReadDeadline(time.Now().Add(ioTimeout))
			var request Request
			if json.NewDecoder(io.LimitReader(conn, maxRequestSize)).Decode(&request) != nil {
				return
			}
			response := handle(request)
			conn.SetWriteDeadline(time.Now().Add(ioTimeout))
			json.NewEncoder(conn).Encode(response)
		}()
	}
}
