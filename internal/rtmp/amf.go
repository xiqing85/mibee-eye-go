// AMF0 encoding helpers for RTMP command messages.
package rtmp

import (
	"encoding/binary"
	"math"
)

// AMF0 type markers.
const (
	amfNumber = 0x00
	amfBool   = 0x01
	amfString = 0x02
	amfObject = 0x03
	amfNull   = 0x05
)

// amfObjectEnd is the 3-byte terminator for an AMF0 object.
var amfObjectEnd = []byte{0x00, 0x00, 0x09}

// amfWriter serialises AMF0 values into a byte buffer.
type amfWriter struct {
	buf []byte
}

func newAMFWriter() *amfWriter {
	return &amfWriter{}
}

func (w *amfWriter) bytes() []byte { return w.buf }

// writeNumber encodes a float64 as AMF0 Number (type 0x00, 8 bytes BE double).
func (w *amfWriter) writeNumber(v float64) {
	w.buf = append(w.buf, amfNumber)
	var tmp [8]byte
	binary.BigEndian.PutUint64(tmp[:], math.Float64bits(v))
	w.buf = append(w.buf, tmp[:]...)
}

// writeBool encodes a bool as AMF0 Boolean (type 0x01, 1 byte value).
func (w *amfWriter) writeBool(v bool) {
	w.buf = append(w.buf, amfBool)
	if v {
		w.buf = append(w.buf, 0x01)
	} else {
		w.buf = append(w.buf, 0x00)
	}
}

// writeString encodes a string as AMF0 String (type 0x02, 2-byte BE length + UTF-8).
func (w *amfWriter) writeString(s string) {
	w.buf = append(w.buf, amfString)
	w.writeUTF8(s)
}

// writeUTF8 writes a plain UTF-8 string with a 2-byte big-endian length prefix (no type marker).
func (w *amfWriter) writeUTF8(s string) {
	var tmp [2]byte
	binary.BigEndian.PutUint16(tmp[:], uint16(len(s)))
	w.buf = append(w.buf, tmp[:]...)
	w.buf = append(w.buf, []byte(s)...)
}

// writeNull writes an AMF0 Null (type 0x05, 0 bytes payload).
func (w *amfWriter) writeNull() {
	w.buf = append(w.buf, amfNull)
}

// writeObjectStart begins an AMF0 Object.
// All key-value pairs are written with writeObjectEntry, then terminated with writeObjectEnd.
func (w *amfWriter) writeObjectStart() {
	w.buf = append(w.buf, amfObject)
}

// writeObjectEntry writes a single key-value pair inside an AMF0 object.
// key must be a string; value must be float64, string, bool, or nil.
func (w *amfWriter) writeObjectEntry(key string, value interface{}) {
	w.writeUTF8(key) // object key as UTF-8 string (2-byte length prefix, no type)
	switch v := value.(type) {
	case float64:
		w.writeNumber(v)
	case string:
		w.writeString(v)
	case bool:
		w.writeBool(v)
	case nil:
		w.writeNull()
	default:
		w.writeNull()
	}
}

// writeObjectEnd terminates the AMF0 object with the 0x0000 0x09 marker.
func (w *amfWriter) writeObjectEnd() {
	w.buf = append(w.buf, amfObjectEnd...)
}

// writeStringNoType writes a UTF-8 string without the AMF0 String type marker (2-byte length + data).
// This is used for object keys and some specific RTMP fields.

// --- AMF0 Reader (minimal, for parsing responses) ---

type amfReader struct {
	data []byte
	pos  int
}

func newAMFReader(data []byte) *amfReader {
	return &amfReader{data: data}
}

func (r *amfReader) available() int { return len(r.data) - r.pos }

// readMarker reads and returns the AMF0 type marker byte.
func (r *amfReader) readMarker() (byte, error) {
	if r.pos >= len(r.data) {
		return 0, errUnexpectedEOF
	}
	m := r.data[r.pos]
	r.pos++
	return m, nil
}

// readString reads an AMF0 String (type 0x02) or plain UTF-8 (if marker is 0x02).
// Returns the string value.
func (r *amfReader) readString() (string, error) {
	m, err := r.readMarker()
	if err != nil {
		return "", err
	}
	if m != amfString {
		return "", errAMFType
	}
	return r.readUTF8()
}

// readUTF8 reads a 2-byte length-prefixed UTF-8 string.
func (r *amfReader) readUTF8() (string, error) {
	if r.pos+2 > len(r.data) {
		return "", errUnexpectedEOF
	}
	length := int(binary.BigEndian.Uint16(r.data[r.pos:]))
	r.pos += 2
	if r.pos+length > len(r.data) {
		return "", errUnexpectedEOF
	}
	s := string(r.data[r.pos : r.pos+length])
	r.pos += length
	return s, nil
}

// readNumber reads an AMF0 Number (type 0x00).
func (r *amfReader) readNumber() (float64, error) {
	m, err := r.readMarker()
	if err != nil {
		return 0, err
	}
	if m != amfNumber {
		return 0, errAMFType
	}
	if r.pos+8 > len(r.data) {
		return 0, errUnexpectedEOF
	}
	v := math.Float64frombits(binary.BigEndian.Uint64(r.data[r.pos:]))
	r.pos += 8
	return v, nil
}

// skipValue skips one AMF0 value at the current position.
func (r *amfReader) skipValue() error {
	if r.pos >= len(r.data) {
		return errUnexpectedEOF
	}
	m := r.data[r.pos]
	r.pos++
	switch m {
	case amfNumber:
		if r.pos+8 > len(r.data) {
			return errUnexpectedEOF
		}
		r.pos += 8
	case amfBool:
		if r.pos+1 > len(r.data) {
			return errUnexpectedEOF
		}
		r.pos++
	case amfString:
		return r.skipUTF8()
	case amfObject:
		for {
			if r.pos+3 > len(r.data) {
				return errUnexpectedEOF
			}
			// Check for object end marker 0x000009
			if r.data[r.pos] == 0 && r.data[r.pos+1] == 0 && r.data[r.pos+2] == 0x09 {
				r.pos += 3
				return nil
			}
			if err := r.skipUTF8(); err != nil { // key
				return err
			}
			if err := r.skipValue(); err != nil { // value
				return err
			}
		}
	case amfNull:
		return nil
	default:
		// Unknown type — skip nothing; caller should handle
		return errAMFType
	}
	return nil
}

func (r *amfReader) skipUTF8() error {
	if r.pos+2 > len(r.data) {
		return errUnexpectedEOF
	}
	length := int(binary.BigEndian.Uint16(r.data[r.pos:]))
	r.pos += 2
	if r.pos+length > len(r.data) {
		return errUnexpectedEOF
	}
	r.pos += length
	return nil
}
