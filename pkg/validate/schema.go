package validate

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/chris/kqueue/pkg/api"
)

// Validator validates job payloads against per-queue JSON schemas.
type Validator struct {
	schemas map[string]*Schema // keyed by queue name
}

// Schema is a compiled JSON Schema for a queue.
type Schema struct {
	QueueName  string
	Properties map[string]Property
	Required   []string
	raw        interface{}
}

// Property describes a single schema property.
type Property struct {
	Type string
	Enum []interface{}
}

// ValidationError contains details about why a payload was rejected.
type ValidationError struct {
	Queue   string   `json:"queue"`
	Errors  []string `json:"errors"`
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("payload validation failed for queue %q: %s", e.Queue, strings.Join(e.Errors, "; "))
}

// NewValidator creates a validator from queue configs.
// Queues without a payload_schema are not validated.
func NewValidator(queues []api.QueueConfig) (*Validator, error) {
	v := &Validator{
		schemas: make(map[string]*Schema),
	}

	for _, q := range queues {
		if q.PayloadSchema == nil {
			continue
		}

		schema, err := compileSchema(q.Name, q.PayloadSchema)
		if err != nil {
			return nil, fmt.Errorf("invalid schema for queue %q: %w", q.Name, err)
		}
		v.schemas[q.Name] = schema
	}

	return v, nil
}

// Validate checks a payload against the schema for the given queue.
// Returns nil if the queue has no schema or the payload is valid.
func (v *Validator) Validate(queueName string, payload json.RawMessage) *ValidationError {
	schema, ok := v.schemas[queueName]
	if !ok {
		return nil // no schema = no validation
	}

	var data interface{}
	if err := json.Unmarshal(payload, &data); err != nil {
		return &ValidationError{
			Queue:  queueName,
			Errors: []string{fmt.Sprintf("payload is not valid JSON: %v", err)},
		}
	}

	errors := validateAgainstSchema(schema, data)
	if len(errors) > 0 {
		return &ValidationError{
			Queue:  queueName,
			Errors: errors,
		}
	}

	return nil
}

// HasSchema returns true if the queue has a schema configured.
func (v *Validator) HasSchema(queueName string) bool {
	_, ok := v.schemas[queueName]
	return ok
}

// GetSchema returns the raw schema for a queue (for API introspection).
func (v *Validator) GetSchema(queueName string) interface{} {
	s, ok := v.schemas[queueName]
	if !ok {
		return nil
	}
	return s.raw
}

func compileSchema(queueName string, raw interface{}) (*Schema, error) {
	// Convert to map
	schemaMap, ok := toStringMap(raw)
	if !ok {
		return nil, fmt.Errorf("schema must be an object")
	}

	schema := &Schema{
		QueueName:  queueName,
		Properties: make(map[string]Property),
		raw:        raw,
	}

	// Parse "type" at top level
	if t, ok := schemaMap["type"].(string); ok && t != "object" {
		return nil, fmt.Errorf("top-level type must be \"object\", got %q", t)
	}

	// Parse "required"
	if req, ok := schemaMap["required"].([]interface{}); ok {
		for _, r := range req {
			if s, ok := r.(string); ok {
				schema.Required = append(schema.Required, s)
			}
		}
	}

	// Parse "properties"
	if props, ok := schemaMap["properties"]; ok {
		propsMap, ok := toStringMap(props)
		if !ok {
			return nil, fmt.Errorf("properties must be an object")
		}
		for name, propRaw := range propsMap {
			propMap, ok := toStringMap(propRaw)
			if !ok {
				continue
			}
			prop := Property{}
			if t, ok := propMap["type"].(string); ok {
				prop.Type = t
			}
			if enum, ok := propMap["enum"].([]interface{}); ok {
				prop.Enum = enum
			}
			schema.Properties[name] = prop
		}
	}

	return schema, nil
}

func validateAgainstSchema(schema *Schema, data interface{}) []string {
	var errors []string

	obj, ok := data.(map[string]interface{})
	if !ok {
		return []string{"payload must be a JSON object"}
	}

	// Check required fields
	for _, req := range schema.Required {
		if _, exists := obj[req]; !exists {
			errors = append(errors, fmt.Sprintf("missing required field %q", req))
		}
	}

	// Check property types and enums
	for name, prop := range schema.Properties {
		val, exists := obj[name]
		if !exists {
			continue // not required and not present = fine
		}

		if prop.Type != "" {
			actualType := jsonType(val)
			if actualType != prop.Type {
				errors = append(errors, fmt.Sprintf("field %q must be type %q, got %q", name, prop.Type, actualType))
			}
		}

		if len(prop.Enum) > 0 {
			found := false
			for _, allowed := range prop.Enum {
				if fmt.Sprint(val) == fmt.Sprint(allowed) {
					found = true
					break
				}
			}
			if !found {
				errors = append(errors, fmt.Sprintf("field %q must be one of %v, got %v", name, prop.Enum, val))
			}
		}
	}

	return errors
}

func jsonType(v interface{}) string {
	switch v.(type) {
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case []interface{}:
		return "array"
	case map[string]interface{}:
		return "object"
	case nil:
		return "null"
	default:
		return "unknown"
	}
}

func toStringMap(v interface{}) (map[string]interface{}, bool) {
	switch m := v.(type) {
	case map[string]interface{}:
		return m, true
	case map[interface{}]interface{}:
		result := make(map[string]interface{})
		for k, val := range m {
			result[fmt.Sprint(k)] = val
		}
		return result, true
	default:
		return nil, false
	}
}
