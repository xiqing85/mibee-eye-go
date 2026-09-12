// Portions derived from MediaMTX (https://github.com/bluenviron/mediamtx)
// Original code Copyright (c) bluenviron, MIT License
//
// params_serialize.go serializes Params into the wire format expected by mtxrpicam.
// Adapted from MediaMTX's internal/staticsources/rpicamera/params_serialize.go
//
// Wire format: field values joined by spaces, each field as "FieldName:Value".
// String values are base64-encoded, uint32 as decimal, float32 as decimal,
// bool as 0/1.

package camera

import (
	"encoding/base64"
	"fmt"
	"reflect"
	"strconv"
	"strings"
)

// Serialize encodes params into the mtxrpicam wire format.
// Each field is "FieldName:Value", joined by spaces.
// Returns an error if any field has an unsupported type — this prevents
// a single bad field from crashing the entire camera service.
func (p Params) Serialize() ([]byte, error) {
	rv := reflect.ValueOf(p)
	rt := rv.Type()
	nf := rv.NumField()
	ret := make([]string, nf)

	for i := range nf {
		entry := rt.Field(i).Name + ":"
		f := rv.Field(i)
		if !supportedParamKind(f.Kind()) {
			return nil, fmt.Errorf("unsupported param field type: %s has %s", rt.Field(i).Name, f.Kind())
		}
		v := f.Interface()

		switch v := v.(type) {
		case uint32:
			entry += strconv.FormatUint(uint64(v), 10)

		case float32:
			entry += strconv.FormatFloat(float64(v), 'f', -1, 64)

		case string:
			entry += base64.StdEncoding.EncodeToString([]byte(v))

		case bool:
			if f.Bool() {
				entry += "1"
			} else {
				entry += "0"
			}

		default:
			return nil, fmt.Errorf("unhandled param type for field %s: %T", rt.Field(i).Name, v)
		}

		ret[i] = entry
	}

	return []byte(strings.Join(ret, " ")), nil
}

// SerializeCommand returns the full command bytes for sending params to mtxrpicam.
// Prefix: 'c' + serialized params. Returns error if serialization fails.
func (p Params) SerializeCommand() ([]byte, error) {
	data, err := p.Serialize()
	if err != nil {
		return nil, err
	}
	return append([]byte{'c'}, data...), nil
}

// SerializeQuit returns the quit command bytes.
func SerializeQuit() []byte {
	return []byte{'e'}
}

// DeserializeParamValue decodes a single base64-encoded string value.
func DeserializeParamValue(encoded string) (string, error) {
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", err
	}
	return string(decoded), nil
}

// supportedParamKind reports whether k is a permitted parameter field kind.
// Only uint32, float32, string, and bool are supported in the wire format.
func supportedParamKind(k reflect.Kind) bool {
	return k == reflect.Uint32 || k == reflect.Float32 || k == reflect.String || k == reflect.Bool
}
