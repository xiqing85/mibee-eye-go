// Portions derived from MediaMTX (https://github.com/bluenviron/mediamtx)
// Original code Copyright (c) bluenviron, MIT License
//
// pipe.go implements the binary pipe protocol for communicating with the
// mtxrpicam subprocess. This is an adaptation of MediaMTX's internal/staticsources/rpicamera/pipe.go

package camera

import (
	"encoding/binary"
	"fmt"
	"io"
)


// maxFrameSize caps the payload size for a single pipe frame.
// mtxrpicam video frames are typically < 1MB; 10MB is a generous ceiling
// that rejects corrupted length prefixes before they cause OOM on the RPi.
const maxFrameSize = 10 << 20 // 10 MB

// pipe provides binary framed read/write over an io.Reader/io.Writer pair.
// Wire format: 4-byte little-endian length prefix, followed by payload bytes.
// This matches the mtxrpicam subprocess protocol exactly.
type pipe struct {
	reader io.Reader
	writer io.Writer
}

// newPipe creates a framed pipe using the provided reader and writer.
func newPipe(reader io.Reader, writer io.Writer) *pipe {
	return &pipe{reader: reader, writer: writer}
}

// read reads a framed message from the pipe.
// Format: [4-byte LE length][payload bytes]
func (p *pipe) read() ([]byte, error) {
	var lenBuf [4]byte

	if _, err := io.ReadFull(p.reader, lenBuf[:]); err != nil {
		return nil, err
	}

	le := binary.LittleEndian.Uint32(lenBuf[:])
	if le == 0 {
		return nil, nil
	}
	if le > maxFrameSize {
		return nil, fmt.Errorf("pipe frame size %d exceeds maximum %d", le, maxFrameSize)
	}

	buf := make([]byte, le)
	if _, err := io.ReadFull(p.reader, buf); err != nil {
		return nil, err
	}

	return buf, nil
}

// write writes a framed message to the pipe.
// Format: [4-byte LE length][payload bytes]
func (p *pipe) write(byts []byte) error {
	var lenBuf [4]byte
	binary.LittleEndian.PutUint32(lenBuf[:], uint32(len(byts)))

	if _, err := p.writer.Write(lenBuf[:]); err != nil {
		return err
	}
	_, err := p.writer.Write(byts)
	return err
}
