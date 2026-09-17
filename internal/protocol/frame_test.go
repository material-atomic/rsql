package protocol

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"testing"
)

type frameFixture struct {
	Version     uint8            `json:"version"`
	HeaderBytes int              `json:"headerBytes"`
	Types       map[string]uint8 `json:"types"`
	Cases       []struct {
		Name       string          `json:"name"`
		Type       uint8           `json:"type"`
		ID         uint32          `json:"id"`
		JSON       json.RawMessage `json:"json"`
		PayloadHex string          `json:"payloadHex"`
		FrameHex   string          `json:"frameHex"`
	} `json:"cases"`
}

func load(t *testing.T) frameFixture {
	t.Helper()
	raw, err := os.ReadFile("../../fixtures/frames.json")
	if err != nil {
		t.Fatalf("the shared fixture must be readable: %v", err)
	}
	var f frameFixture
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if len(f.Cases) < 6 {
		t.Fatalf("fixture: expected at least 6 cases, got %d", len(f.Cases))
	}
	return f
}

// Bytes the client wrote, read here, field for field.
func TestFixtureFramesDecodeAsTheClientWroteThem(t *testing.T) {
	f := load(t)

	if f.Version != Version || f.HeaderBytes != HeaderBytes {
		t.Fatalf("header: fixture says version %d / %d bytes, this side says %d / %d", f.Version, f.HeaderBytes, Version, HeaderBytes)
	}
	for name, code := range f.Types {
		if got := Type(code).String(); got != name {
			t.Errorf("type %d: fixture calls it %q, this side calls it %q", code, name, got)
		}
	}

	for _, c := range f.Cases {
		wire, err := hex.DecodeString(c.FrameHex)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		wantPayload, err := hex.DecodeString(c.PayloadHex)
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}

		frame, err := NewReader(bytes.NewReader(wire)).Read()
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if frame.Version != Version || uint8(frame.Type) != c.Type || frame.ID != c.ID {
			t.Errorf("%s: got version %d type %d id %d, want %d/%d/%d", c.Name, frame.Version, frame.Type, frame.ID, Version, c.Type, c.ID)
		}
		if !bytes.Equal(frame.Payload, wantPayload) {
			t.Errorf("%s: payload differs", c.Name)
		}

		// And the same bytes come back out of this side's encoder.
		encoded, err := Encode(Frame{Type: Type(c.Type), ID: c.ID, Payload: wantPayload})
		if err != nil {
			t.Fatalf("%s: %v", c.Name, err)
		}
		if !bytes.Equal(encoded, wire) {
			t.Errorf("%s:\n got  %s\n want %s", c.Name, hex.EncodeToString(encoded), c.FrameHex)
		}
	}
}

// A stream is not a sequence of messages.
func TestReadsFramesBackToBackAndAcrossChunkBoundaries(t *testing.T) {
	var stream bytes.Buffer
	for id := uint32(1); id <= 3; id++ {
		frame, err := Encode(Frame{Type: Invoke, ID: id, Payload: []byte{byte(id), byte(id)}})
		if err != nil {
			t.Fatal(err)
		}
		stream.Write(frame)
	}

	reader := NewReader(&iotest{data: stream.Bytes(), chunk: 3})
	for id := uint32(1); id <= 3; id++ {
		frame, err := reader.Read()
		if err != nil {
			t.Fatalf("frame %d: %v", id, err)
		}
		if frame.ID != id || !bytes.Equal(frame.Payload, []byte{byte(id), byte(id)}) {
			t.Errorf("frame %d came back as %+v", id, frame)
		}
	}
	if _, err := reader.Read(); !errors.Is(err, io.EOF) {
		t.Errorf("a clean end between frames is io.EOF, got %v", err)
	}
}

// One byte at a time: the reader must not mistake a slow stream for a broken one.
type iotest struct {
	data  []byte
	chunk int
	at    int
}

func (r *iotest) Read(p []byte) (int, error) {
	if r.at >= len(r.data) {
		return 0, io.EOF
	}
	end := r.at + r.chunk
	if end > len(r.data) {
		end = len(r.data)
	}
	n := copy(p, r.data[r.at:end])
	r.at += n
	return n, nil
}

func TestHalfAFrameIsNotAFrame(t *testing.T) {
	frame, _ := Encode(Frame{Type: Result, ID: 1, Payload: []byte("abcdef")})

	if _, err := NewReader(bytes.NewReader(frame[:4])).Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("half a header: want ErrUnexpectedEOF, got %v", err)
	}
	if _, err := NewReader(bytes.NewReader(frame[:HeaderBytes+2])).Read(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("half a payload: want ErrUnexpectedEOF, got %v", err)
	}
}

func TestRefusesAVersionItCannotRead(t *testing.T) {
	frame, _ := Encode(Frame{Version: 99, Type: Ping, ID: 1})

	if _, err := NewReader(bytes.NewReader(frame)).Read(); !errors.Is(err, ErrVersion) {
		t.Errorf("want ErrVersion, got %v", err)
	}
	if _, err := NewReader(bytes.NewReader(frame)).Accept(99).Read(); err != nil {
		t.Errorf("a version it was told to accept: %v", err)
	}
}

func TestLengthIsAPromiseWithALimit(t *testing.T) {
	frame, _ := Encode(Frame{Type: Result, ID: 1, Payload: make([]byte, 64)})

	if _, err := NewReader(bytes.NewReader(frame)).Limit(32).Read(); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("want ErrPayloadTooLarge, got %v", err)
	}
	// Refused from the header alone, before a byte of payload is read.
	header := frame[:HeaderBytes]
	if _, err := NewReader(bytes.NewReader(header)).Limit(32).Read(); !errors.Is(err, ErrPayloadTooLarge) {
		t.Errorf("header alone: want ErrPayloadTooLarge, got %v", err)
	}
}

func TestZeroVersionMeansThisVersion(t *testing.T) {
	frame, err := Encode(Frame{Type: Ping, ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if frame[0] != Version {
		t.Errorf("version byte is %d, want %d", frame[0], Version)
	}
}
