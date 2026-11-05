package maps

import (
	"fmt"
	"strconv"
	"strings"
)

func NestedValue(obj map[string]any, fields []string) (any, bool) {
	var current any = obj

	for _, field := range fields {
		switch typedCurrent := current.(type) {
		case map[string]any:
			// THE FOUND FLAG IS PROPAGATED, NOT DISCARDED. It used to be dropped here, so a missing
			// LAST key set current to nil and the loop returned (nil, true): "found, and it is nil".
			// A missing intermediate key was only correct by accident (nil falls to the default
			// case below). NestedString then turned that phantom nil into an access error, so
			// reading status.digest from a CR whose status existed without one FAILED instead of
			// returning "". The composition-dynamic-controller's adoption path hit exactly that and
			// could never complete (core-provider, the #322 end-to-end test), and its
			// staleinventory.go had already switched to unstructured.NestedString to code around it.
			var found bool
			current, found, _ = nestedFieldNoCopy(typedCurrent, field)
			if !found {
				return nil, false
			}
		case []any:
			index, err := strconv.Atoi(field)
			if err != nil || index < 0 || index >= len(typedCurrent) {
				return nil, false
			}
			current = typedCurrent[index]
		default:
			return nil, false
		}
	}

	return current, true
}

func SetNestedValue(obj map[string]any, fields []string, newValue any) error {
	lastIndex := len(fields) - 1

	var current any = obj
	var parent any = nil
	var parentKey string

	for i, field := range fields {
		switch typedCurrent := current.(type) {
		case map[string]any:
			// if last element then update its value
			if i == lastIndex {
				typedCurrent[field] = newValue
				return nil
			}
			parent = typedCurrent
			parentKey = field
			current = typedCurrent[field]

		case []any:
			index, err := strconv.Atoi(field)
			if err != nil || index < 0 || index >= len(typedCurrent) {
				return fmt.Errorf("invalid index: %s", field)
			}

			// if last element then update the array
			if i == lastIndex {
				typedCurrent[index] = newValue
				return nil
			}

			parent = typedCurrent
			parentKey = field
			current = typedCurrent[index]

		default:
			if i == lastIndex {
				// update value in its parent
				switch typedParent := parent.(type) {
				case map[string]any:
					typedParent[parentKey] = newValue
					return nil
				case []any:
					index, err := strconv.Atoi(parentKey)
					if err == nil && index >= 0 && index < len(typedParent) {
						typedParent[index] = newValue
						return nil
					}
				}
			}

			return fmt.Errorf("data structure not navigable at path: %s", strings.Join(fields, "."))
		}
	}

	return fmt.Errorf("unable to update path: %s", strings.Join(fields, "."))
}

// NestedMapNoCopy returns a map[string]any value of a nested field.
// Returns false if value is not found and an error if not a map[string]any.
func NestedMapNoCopy(obj map[string]any, fields ...string) (map[string]any, bool, error) {
	val, found, err := nestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	m, ok := val.(map[string]interface{})
	if !ok {
		return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected map[string]interface{}",
			strings.Join(fields, "."), val, val)
	}
	return m, true, nil
}

// NestedMap returns a deep copy of map[string]any value of a nested field.
// Returns false if value is not found and an error if not a map[string]any.
func NestedMap(obj map[string]any, fields ...string) (map[string]any, bool, error) {
	m, found, err := NestedMapNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	return DeepCopyJSON(m), true, nil
}

func NestedSliceNoCopy(obj map[string]any, fields ...string) ([]any, bool, error) {
	val, found, err := nestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	_, ok := val.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected []any",
			strings.Join(fields, "."), val, val)
	}
	return val.([]any), true, nil
}

// NestedFieldCopy returns a deep copy of the value of a nested field.
// Returns false if the value is missing.
// No error is returned for a nil field.
//
// Note: fields passed to this function are treated as keys within the passed
// object; no array/slice syntax is supported.
func NestedFieldCopy(obj map[string]any, fields ...string) (any, bool, error) {
	val, found, err := nestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	return deepCopyJSONValue(val), true, nil
}

// NestedSlice returns a deep copy of []any value of a nested field.
// Returns false if value is not found and an error if not a []any.
func NestedSlice(obj map[string]any, fields ...string) ([]any, bool, error) {
	val, found, err := nestedFieldNoCopy(obj, fields...)
	if !found || err != nil {
		return nil, found, err
	}
	_, ok := val.([]any)
	if !ok {
		return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected []any",
			strings.Join(fields, "."), val, val)
	}
	return deepCopyJSONValue(val).([]any), true, nil
}

// SetNestedField sets the value of a nested field to a deep copy of the value provided.
// Returns an error if value cannot be set because one of the nesting levels is not a map[string]any.
func SetNestedField(obj map[string]any, value any, fields ...string) error {
	return setNestedFieldNoCopy(obj, deepCopyJSONValue(value), fields...)
}

func setNestedFieldNoCopy(obj map[string]any, value any, fields ...string) error {
	m := obj

	for i, field := range fields[:len(fields)-1] {
		if val, ok := m[field]; ok {
			if valMap, ok := val.(map[string]any); ok {
				m = valMap
			} else {
				return fmt.Errorf("value cannot be set because %v is not a map[string]any", strings.Join(fields[:i+1], "."))
			}
		} else {
			newVal := make(map[string]any)
			m[field] = newVal
			m = newVal
		}
	}
	m[fields[len(fields)-1]] = value
	return nil
}

// nestedFieldNoCopy returns a reference to a nested field.
// Returns false if value is not found and an error if unable
// to traverse obj.
//
// Note: fields passed to this function are treated as keys within the passed
// object; no array/slice syntax is supported.
func nestedFieldNoCopy(obj map[string]any, fields ...string) (any, bool, error) {
	var val any = obj

	for i, field := range fields {
		if val == nil {
			return nil, false, nil
		}
		if m, ok := val.(map[string]any); ok {
			val, ok = m[field]
			if !ok {
				return nil, false, nil
			}
		} else {
			return nil, false, fmt.Errorf("%v accessor error: %v is of the type %T, expected map[string]any",
				strings.Join(fields[:i+1], "."), val, val)
		}
	}
	return val, true, nil
}
