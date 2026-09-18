// Package wire is a client, in Go, for the server in this repository.
//
// The driver applications use is the TypeScript one; this is what the tools
// that ship with the server need — the shell today, and whatever follows it.
// It is deliberately small: a connection, the frames, and the handful of
// requests the tools send. Everything it does not do is a thing nobody has
// needed yet.
//
// One request at a time, on purpose. A tool that a person is typing at has one
// question outstanding, and the pipelining that a driver needs is machinery
// that would have to be tested for a use that does not exist.
package wire

import (
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/sapedb/sapedb/internal/connection"
	"github.com/sapedb/sapedb/internal/protocol"
	"github.com/sapedb/sapedb/internal/signing"
	"github.com/sapedb/sapedb/internal/store"
)

// ErrRefused is the server saying no. It carries the code, because a tool that
// tells failures apart by matching prose breaks when the server's wording
// improves.
type ErrRefused struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *ErrRefused) Error() string {
	if e.Code == "" {
		return e.Message
	}
	return e.Message + " (" + e.Code + ")"
}

// Client is one connection to one server.
type Client struct {
	conn    net.Conn
	reader  *protocol.Reader
	next    uint32
	welcome Welcome
}

// Welcome is what the server said when the connection opened.
type Welcome struct {
	Version   uint8  `json:"version"`
	Account   string `json:"account"`
	DBName    string `json:"dbname,omitempty"`
	Mode      string `json:"mode"`
	LSN       uint64 `json:"lsn,omitempty"`
	Encrypted bool   `json:"encrypted"`
	Challenge string `json:"challenge,omitempty"`
}

// Options are what a connection needs beyond the string.
type Options struct {
	// Insecure connects without TLS. Said out loud, as on the server: a
	// connection that quietly falls back to plaintext is worse than one that
	// refuses.
	Insecure bool
	Timeout  time.Duration
}

// Dial opens a connection string and does the handshake.
func Dial(where connection.Connection, options Options) (*Client, error) {
	timeout := options.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	address := net.JoinHostPort(where.Host, fmt.Sprint(where.Port))

	var conn net.Conn
	var err error
	if options.Insecure {
		conn, err = net.DialTimeout("tcp", address, timeout)
	} else {
		conn, err = tls.DialWithDialer(&net.Dialer{Timeout: timeout}, "tcp", address, &tls.Config{ServerName: where.Host})
	}
	if err != nil {
		return nil, err
	}

	client := &Client{conn: conn, reader: protocol.NewReader(conn).Accept(protocol.Version)}

	frame, err := client.request(protocol.Hello, map[string]any{
		"account":  where.Account,
		"password": where.Password,
		"dbname":   where.DBName,
		"sig":      where.Signature,
		"mode":     "bound",
	})
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	if frame.Type != protocol.Welcome {
		_ = conn.Close()
		return nil, fmt.Errorf("sapedb/wire: the server opened with a %s", frame.Type)
	}
	if err := json.Unmarshal(frame.Payload, &client.welcome); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return client, nil
}

// Welcome is what the server said at the handshake.
func (c *Client) Welcome() Welcome { return c.welcome }

// Close says goodbye and hangs up. The goodbye is best effort: a connection
// being closed is not a place to fail.
func (c *Client) Close() error {
	_, _ = c.send(protocol.Goodbye, nil)
	return c.conn.Close()
}

// Operate proves this connection may explore, by answering the challenge the
// server issued with the server's own secret.
func (c *Client) Operate(secret string) error {
	if c.welcome.Challenge == "" {
		return errors.New("sapedb/wire: this server issued no challenge, so it cannot be operated")
	}
	challenge, err := hex.DecodeString(c.welcome.Challenge)
	if err != nil {
		return fmt.Errorf("sapedb/wire: the challenge is not hex: %w", err)
	}
	proof, err := signing.Operating(secret, challenge)
	if err != nil {
		return err
	}
	_, err = c.ask(protocol.Elevate, map[string]any{"proof": proof})
	return err
}

// Explored is the answer to an access somebody typed.
type Explored struct {
	Result store.Result     `json:"result"`
	Draft  store.Operation  `json:"draft"`
	Here   *store.Catalogue `json:"here,omitempty"`
}

// Explore runs a typed access.
func (c *Client) Explore(access store.Access) (Explored, error) {
	payload, err := c.ask(protocol.Explore, map[string]any{"access": access})
	if err != nil {
		return Explored{}, err
	}
	answer := Explored{}
	return answer, json.Unmarshal(payload, &answer)
}

// WhatIsHere asks what the database holds.
func (c *Client) WhatIsHere() (store.Catalogue, error) {
	payload, err := c.ask(protocol.Explore, map[string]any{"catalogue": true})
	if err != nil {
		return store.Catalogue{}, err
	}
	answer := Explored{}
	if err := json.Unmarshal(payload, &answer); err != nil {
		return store.Catalogue{}, err
	}
	if answer.Here == nil {
		return store.Catalogue{}, errors.New("sapedb/wire: the server sent no catalogue")
	}
	return *answer.Here, nil
}

// Invoke runs a declared operation.
func (c *Client) Invoke(name string, arguments map[string]any) (store.Result, error) {
	payload, err := c.ask(protocol.Invoke, map[string]any{"command": name, "arguments": arguments})
	if err != nil {
		return store.Result{}, err
	}
	result := store.Result{}
	return result, json.Unmarshal(payload, &result)
}

// ask sends a request and returns the payload of a successful answer, turning
// a refusal into an error that carries the code.
func (c *Client) ask(kind protocol.Type, body any) ([]byte, error) {
	frame, err := c.request(kind, body)
	if err != nil {
		return nil, err
	}
	if frame.Type == protocol.Failure {
		refused := &ErrRefused{}
		if err := json.Unmarshal(frame.Payload, refused); err != nil {
			return nil, fmt.Errorf("sapedb/wire: the server refused it and the reason does not read: %w", err)
		}
		return nil, refused
	}
	return frame.Payload, nil
}

// request sends one frame and reads the one that answers it.
func (c *Client) request(kind protocol.Type, body any) (protocol.Frame, error) {
	id, err := c.send(kind, body)
	if err != nil {
		return protocol.Frame{}, err
	}
	frame, err := c.reader.Read()
	if err != nil {
		return protocol.Frame{}, err
	}
	// One request at a time, so an answer to something else is a server that
	// is not the one this speaks to.
	if frame.ID != id {
		return protocol.Frame{}, fmt.Errorf("sapedb/wire: asked %d and was answered %d", id, frame.ID)
	}
	return frame, nil
}

func (c *Client) send(kind protocol.Type, body any) (uint32, error) {
	var payload []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		payload = encoded
	}

	c.next++
	frame, err := protocol.Encode(protocol.Frame{Version: protocol.Version, Type: kind, ID: c.next, Payload: payload})
	if err != nil {
		return 0, err
	}
	if _, err := c.conn.Write(frame); err != nil {
		return 0, err
	}
	return c.next, nil
}
