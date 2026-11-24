package extension

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// MaxFrame is the largest frame either side may send. A catalog or a
// generated file's content is the largest thing a frame carries; 64 MiB is
// far above any measured catalog and still bounds what a broken host can make
// voxagent allocate.
const MaxFrame = 64 << 20

// ErrFrameTooLarge is a frame whose declared length exceeds MaxFrame.
var ErrFrameTooLarge = errors.New("extension: frame exceeds the 64 MiB limit")

// WriteFrame writes body as one frame: a 4-byte big-endian length, then the
// bytes.
func WriteFrame(w io.Writer, body []byte) error {
	if len(body) > MaxFrame {
		return ErrFrameTooLarge
	}
	frame := make([]byte, 4+len(body))
	binary.BigEndian.PutUint32(frame, uint32(len(body)))
	copy(frame[4:], body)
	if _, err := w.Write(frame); err != nil {
		return fmt.Errorf("extension: write frame: %w", err)
	}
	return nil
}

// ReadFrame reads one frame. It returns io.EOF only when the stream ends
// exactly at a frame boundary, and io.ErrUnexpectedEOF when it ends inside
// one. The length is checked before anything is allocated.
func ReadFrame(r io.Reader) ([]byte, error) {
	var header [4]byte
	if _, err := io.ReadFull(r, header[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(header[:])
	if n > MaxFrame {
		return nil, fmt.Errorf("%w: %d bytes declared", ErrFrameTooLarge, n)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	return body, nil
}
