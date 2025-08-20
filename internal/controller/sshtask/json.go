package sshtask

import (
	"errors"
	"fmt"
)

// GetNestedField retrieves a nested map within a map[string]any structure.
// It traverses the object using the provided sequence of field names.
// Example:
//
//	nested, err := GetNestedField(obj, "spec", "image")
//
// the obj["spec"]["image"] should be a map[string]any otherwise it will return an errors
func getNestedField(obj map[string]any, fields ...string) (map[string]any, error) {
	if len(fields) == 0 {
		return nil, errors.New("no fields provided")
	}
	m := obj
	for _, field := range fields {
		if val, ok := m[field].(map[string]any); ok {
			m = val
		} else {
			return nil, errors.New(fmt.Sprintf("field [%s] not found in the object or its type is not map[string]any", field))
		}
	}
	return m, nil // the last field is not found in the object
}

// getNestedValue returns the nested value of a map[string]interface{} object as an interface{}
func getNestedValue(obj map[string]any, fields ...string) (any, error) {
	f := fields[:len(fields)-1]
	value, err := getNestedField(obj, f...)
	if err != nil {
		return nil, err
	}
	if val, ok := value[fields[len(fields)-1]]; ok {
		return val, nil
	}
	return nil, fmt.Errorf("field %s not found in the object", fields[len(fields)-1])
}