package extension

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	for _, body := range [][]byte{[]byte(`{}`), []byte(`{"a":"ü"}`), {}} {
		if err := WriteFrame(&buf, body); err != nil {
			t.Fatalf("WriteFrame(%q): %v", body, err)
		}
	}
	for _, want := range []string{`{}`, `{"a":"ü"}`, ``} {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		if string(got) != want {
			t.Errorf("ReadFrame = %q, want %q", got, want)
		}
	}
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Errorf("ReadFrame at a clean boundary = %v, want io.EOF", err)
	}
}

func TestReadFrameRefusesTruncationAndOversize(t *testing.T) {
	header := func(n uint32) []byte {
		b := make([]byte, 4)
		binary.BigEndian.PutUint32(b, n)
		return b
	}
	for _, tt := range []struct {
		name string
		in   []byte
		want error
	}{
		{"half a header", []byte{0, 0}, io.ErrUnexpectedEOF},
		{"short body", append(header(10), []byte("abc")...), io.ErrUnexpectedEOF},
		{"one byte over the limit", header(MaxFrame + 1), ErrFrameTooLarge},
		{"the largest length", header(^uint32(0)), ErrFrameTooLarge},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ReadFrame(bytes.NewReader(tt.in))
			if !errors.Is(err, tt.want) {
				t.Errorf("ReadFrame = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestWriteFrameRefusesOversize(t *testing.T) {
	err := WriteFrame(io.Discard, make([]byte, MaxFrame+1))
	if !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("WriteFrame = %v, want ErrFrameTooLarge", err)
	}
}

// ReadFrame must never panic and never hand back more than MaxFrame bytes,
// whatever the host sends.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{0, 0, 0, 2, '{', '}'})
	f.Add([]byte{0xff, 0xff, 0xff, 0xff})
	f.Add([]byte{0, 0})
	f.Fuzz(func(t *testing.T, in []byte) {
		body, err := ReadFrame(bytes.NewReader(in))
		if err == nil && len(body) > MaxFrame {
			t.Fatalf("ReadFrame returned %d bytes", len(body))
		}
	})
}
