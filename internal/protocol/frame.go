// Package protocol is the wire format: how a frame is written and read.
//
// The client side is @ecosy/sapedb/protocol. Both read fixtures/frames.json —
// bytes written by one, decoded by the other — so a change to the layout turns
// both test suites red at once rather than showing up as a connection that
// hangs with nothing in a log.
//
//	 0      version   u8     what this frame is written in
//	 1      type      u8     what it is
//	 2..5   id        u32be  which request it belongs to
//	 6..9   length    u32be  bytes of payload
//	10..    payload
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Version is what this side writes.
const Version uint8 = 1

// HeaderBytes is the fixed header in front of every payload.
const HeaderBytes = 10

// MaxPayload bounds what one frame may carry. A length field is a promise
// about an allocation; this is the limit on how much a peer can make us
// believe.
const MaxPayload = 16 << 20

// Type is what a frame is.
type Type uint8

const (
	Hello     Type = 1
	Welcome   Type = 2
	Ping      Type = 3
	Pong      Type = 4
	Invoke    Type = 5
	Result    Type = 6
	Failure   Type = 7
	Subscribe Type = 8
	Event     Type = 9
	Goodbye   Type = 10
	// Elevate answers the challenge in the welcome, proving the sender holds
	// the server's own secret. Explore then carries an access an operator
	// typed rather than the name of one somebody declared — refused on a
	// connection that has not proved it.
	Elevate Type = 11
	Explore Type = 12
)

var typeNames = map[Type]string{
	Hello: "hello", Welcome: "welcome", Ping: "ping", Pong: "pong", Invoke: "invoke",
	Result: "result", Failure: "failure", Subscribe: "subscribe", Event: "event", Goodbye: "goodbye",
	Elevate: "elevate", Explore: "explore",
}

// String names a frame type, or reports the code when this version has no name
// for it.
func (t Type) String() string {
	if name, ok := typeNames[t]; ok {
		return name
	}
	return fmt.Sprintf("type(%d)", uint8(t))
}

// Frame is one message.
type Frame struct {
	Version uint8
	Type    Type
	// ID ties a response to the Invoke that asked for it. Zero is for frames
	// nobody asked for — an Event from a subscription, say.
	ID      uint32
	Payload []byte
}

var (
	ErrPayloadTooLarge = errors.New("sapedb: frame payload over the limit")
	ErrVersion         = errors.New("sapedb: frame version not readable by this side")
)

// Encode writes a frame as bytes.
func Encode(frame Frame) ([]byte, error) {
	if len(frame.Payload) > MaxPayload {
		return nil, fmt.Errorf("%w: %d bytes", ErrPayloadTooLarge, len(frame.Payload))
	}

	version := frame.Version
	if version == 0 {
		version = Version
	}

	out := make([]byte, HeaderBytes+len(frame.Payload))
	out[0] = version
	out[1] = uint8(frame.Type)
	binary.BigEndian.PutUint32(out[2:6], frame.ID)
	binary.BigEndian.PutUint32(out[6:10], uint32(len(frame.Payload)))
	copy(out[HeaderBytes:], frame.Payload)

	return out, nil
}

// Reader reads frames off a stream.
//
// A socket hands over whatever arrived: half a header, three frames at once,
// the second half of a payload. Read pulls exactly one frame, waiting for the
// bytes it needs.
type Reader struct {
	source     io.Reader
	maxPayload int
	versions   map[uint8]bool
	header     [HeaderBytes]byte
}

// NewReader reads frames of this version, up to MaxPayload each.
func NewReader(source io.Reader) *Reader {
	return &Reader{source: source, maxPayload: MaxPayload, versions: map[uint8]bool{Version: true}}
}

// Accept adds a version this reader will read — for a peer still speaking an
// older one during a rollout.
func (r *Reader) Accept(version uint8) *Reader {
	r.versions[version] = true
	return r
}

// Limit lowers how much payload a single frame may declare.
func (r *Reader) Limit(bytes int) *Reader {
	r.maxPayload = bytes
	return r
}

// Read returns the next frame, or io.EOF when the stream ends cleanly between
// frames. A version this side does not read, or a length over the limit, ends
// the stream: neither can be resynchronised, only dropped.
func (r *Reader) Read() (Frame, error) {
	if _, err := io.ReadFull(r.source, r.header[:]); err != nil {
		return Frame{}, err
	}

	version := r.header[0]
	if !r.versions[version] {
		return Frame{}, fmt.Errorf("%w: %d", ErrVersion, version)
	}

	length := binary.BigEndian.Uint32(r.header[6:10])
	if int(length) > r.maxPayload {
		return Frame{}, fmt.Errorf("%w: a frame declared %d bytes", ErrPayloadTooLarge, length)
	}

	payload := make([]byte, length)
	if _, err := io.ReadFull(r.source, payload); err != nil {
		// Half a frame is not a frame: an EOF here is the stream breaking.
		if errors.Is(err, io.EOF) {
			return Frame{}, io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}

	return Frame{
		Version: version,
		Type:    Type(r.header[1]),
		ID:      binary.BigEndian.Uint32(r.header[2:6]),
		Payload: payload,
	}, nil
}
