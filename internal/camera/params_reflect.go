// Portions derived from MediaMTX (https://github.com/bluenviron/mediamtx)
// Original code Copyright (c) bluenviron, MIT License
//
// params_reflect.go provides reflection-based parameter access for the Camera interface.
// This allows SetParam/GetParam to work with string-based parameter names.

package camera

import (
	"fmt"
	"reflect"
	"strconv"
)

// setParamValue sets a field on Params by its struct field name.
func setParamValue(p *Params, fieldName string, value interface{}) error {
	rv := reflect.ValueOf(p).Elem()
	fv := rv.FieldByName(fieldName)
	if !fv.IsValid() {
		return fmt.Errorf("unknown field: %s", fieldName)
	}

	if !fv.CanSet() {
		return fmt.Errorf("field %s is not settable", fieldName)
	}

	switch fv.Kind() {
	case reflect.Uint32:
		switch v := value.(type) {
		case uint32:
			fv.SetUint(uint64(v))
		case int:
			if v < 0 {
				return fmt.Errorf("value %d out of range for uint32", v)
			}
			fv.SetUint(uint64(v))
		case float64:
			fv.SetUint(uint64(v))
		case string:
			u, err := strconv.ParseUint(v, 10, 32)
			if err != nil {
				return fmt.Errorf("parse uint32: %w", err)
			}
			fv.SetUint(u)
		default:
			return fmt.Errorf("cannot set uint32 field %s from %T", fieldName, value)
		}

	case reflect.Float32:
		switch v := value.(type) {
		case float32:
			fv.SetFloat(float64(v))
		case float64:
			fv.SetFloat(v)
		case int:
			fv.SetFloat(float64(v))
		case string:
			f, err := strconv.ParseFloat(v, 32)
			if err != nil {
				return fmt.Errorf("parse float32: %w", err)
			}
			fv.SetFloat(f)
		default:
			return fmt.Errorf("cannot set float32 field %s from %T", fieldName, value)
		}

	case reflect.String:
		switch v := value.(type) {
		case string:
			fv.SetString(v)
		default:
			return fmt.Errorf("cannot set string field %s from %T", fieldName, value)
		}

	case reflect.Bool:
		switch v := value.(type) {
		case bool:
			fv.SetBool(v)
		case int:
			fv.SetBool(v != 0)
		case float64:
			fv.SetBool(v != 0)
		case string:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("parse bool: %w", err)
			}
			fv.SetBool(b)
		default:
			return fmt.Errorf("cannot set bool field %s from %T", fieldName, value)
		}

	default:
		return fmt.Errorf("unsupported field type %s for %s", fv.Kind(), fieldName)
	}

	return nil
}

// getParamValue gets a field value from Params by its struct field name.
func getParamValue(p Params, fieldName string) (interface{}, error) {
	rv := reflect.ValueOf(p)
	fv := rv.FieldByName(fieldName)
	if !fv.IsValid() {
		return nil, fmt.Errorf("unknown field: %s", fieldName)
	}

	switch fv.Kind() {
	case reflect.Uint32:
		return fv.Uint(), nil
	case reflect.Float32:
		return float32(fv.Float()), nil
	case reflect.String:
		return fv.String(), nil
	case reflect.Bool:
		return fv.Bool(), nil
	default:
		return nil, fmt.Errorf("unsupported field type %s for %s", fv.Kind(), fieldName)
	}
}
