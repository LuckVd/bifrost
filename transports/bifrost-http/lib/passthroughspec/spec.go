// Package passthroughspec compiles the optional OpenAPI document of a generic passthrough
// provider into what the relay checks per request: whether a method and path name an operation
// the document lists, and whether a JSON request body satisfies that operation's schema. The
// document is parsed once per provider build (framework/openapimcp does the version-independent
// parse); the per-request work is a segment match and, when enabled, one schema validation.
package passthroughspec

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"sort"
	"strings"

	"github.com/maximhq/bifrost/core/schemas"
	"github.com/maximhq/bifrost/framework/openapimcp"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// OperationSummary is one operation of a compiled document, as the management API lists them.
type OperationSummary struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	OperationID string `json:"operation_id,omitempty"`
	Summary     string `json:"summary,omitempty"`
	HasBody     bool   `json:"has_body"`
}

// Spec is a compiled document.
type Spec struct {
	Title          string
	Version        string
	OpenAPIVersion string
	ops            []operation
}

type operation struct {
	op       *openapimcp.Operation
	segments []segment
	body     *jsonschema.Schema // nil when the body is not JSON or declares no schema
	bodyType string
}

// segment is one path-template segment: a literal, or a parameter that matches any one segment.
type segment struct {
	literal string
	param   bool
}

// Compile parses the configured document and prepares every operation for matching. It returns
// an error for a document that cannot be parsed or a JSON body schema that cannot be compiled.
func Compile(cfg *schemas.GenericOpenAPIConfig) (*Spec, error) {
	if cfg == nil {
		return nil, errors.New("no openapi config")
	}
	data, err := openapimcp.LoadBytes([]byte(cfg.Spec))
	if err != nil {
		return nil, err
	}
	specURL := ""
	if cfg.SpecURL != nil {
		specURL = *cfg.SpecURL
	}
	doc, err := openapimcp.Parse(data, openapimcp.ParseOptions{SpecURL: specURL})
	if err != nil {
		return nil, err
	}
	return compileDocument(doc, cfg.ValidateRequestBody)
}

func compileDocument(doc *openapimcp.Document, compileBodies bool) (*Spec, error) {
	spec := &Spec{Title: doc.Title, Version: doc.Version, OpenAPIVersion: doc.OpenAPIVersion, ops: make([]operation, 0, len(doc.Operations))}
	for i := range doc.Operations {
		op := &doc.Operations[i]
		compiled := operation{op: op, segments: splitTemplate(op.Path)}
		if compileBodies && op.Body != nil && op.Body.Schema != nil && isJSONMediaType(op.Body.ContentType) {
			schema, err := compileBodySchema(i, op.Body.Schema)
			if err != nil {
				return nil, fmt.Errorf("%s %s: request body schema: %w", op.Method, op.Path, err)
			}
			compiled.body = schema
			compiled.bodyType = op.Body.ContentType
		}
		spec.ops = append(spec.ops, compiled)
	}
	// Literal segments sort before parameters so /users/me wins over /users/{id}.
	sort.SliceStable(spec.ops, func(a, b int) bool {
		return literalCount(spec.ops[a].segments) > literalCount(spec.ops[b].segments)
	})
	return spec, nil
}

func literalCount(segments []segment) int {
	n := 0
	for _, s := range segments {
		if !s.param {
			n++
		}
	}
	return n
}

func splitTemplate(path string) []segment {
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	segments := make([]segment, 0, len(parts))
	for _, part := range parts {
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			segments = append(segments, segment{param: true})
			continue
		}
		segments = append(segments, segment{literal: part})
	}
	return segments
}

// compileBodySchema turns an operation's (dereferenced) body schema into a validator. The schema
// is normalized into the 3.0-style dialect first, then round-tripped through JSON so the
// validator sees exactly the value types it expects.
func compileBodySchema(index int, raw map[string]any) (*jsonschema.Schema, error) {
	normalized := openapimcp.NormalizeSchema(raw)
	allowNullable(normalized)
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("inmemory://passthrough/op-%d.json", index)
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(url, doc); err != nil {
		return nil, err
	}
	return compiler.Compile(url)
}

// allowNullable rewrites the OpenAPI 3.0 "nullable: true" marker, which NormalizeSchema emits and
// JSON Schema validators ignore, into a null-admitting type so a null value validates.
func allowNullable(node any) {
	switch v := node.(type) {
	case map[string]any:
		if nullable, ok := v["nullable"].(bool); ok {
			delete(v, "nullable")
			if nullable {
				switch typ := v["type"].(type) {
				case string:
					v["type"] = []any{typ, "null"}
				case []any:
					v["type"] = append(typ, "null")
				}
			}
		}
		for _, child := range v {
			allowNullable(child)
		}
	case []any:
		for _, child := range v {
			allowNullable(child)
		}
	}
}

func isJSONMediaType(mediaType string) bool {
	mt := strings.ToLower(strings.TrimSpace(mediaType))
	return mt == "application/json" || strings.HasSuffix(mt, "+json")
}

// Match returns the operation a method and stripped request path name, if the document lists one.
// A trailing slash is ignored; a query string must already be removed.
func (s *Spec) Match(method, path string) (*openapimcp.Operation, bool) {
	if s == nil {
		return nil, false
	}
	method = strings.ToUpper(method)
	parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), "/")
	for i := range s.ops {
		op := &s.ops[i]
		if op.op.Method != method || len(op.segments) != len(parts) {
			continue
		}
		matched := true
		for j, seg := range op.segments {
			if seg.param {
				if parts[j] == "" {
					matched = false
					break
				}
				continue
			}
			if seg.literal != parts[j] {
				matched = false
				break
			}
		}
		if matched {
			return op.op, true
		}
	}
	return nil, false
}

// ValidateBody checks a request body against the matched operation's JSON schema. Bodies in other
// media types are not checked; a required body that is missing is an error.
func (s *Spec) ValidateBody(op *openapimcp.Operation, contentType string, body []byte) error {
	if s == nil || op == nil {
		return nil
	}
	compiled := s.find(op)
	if compiled == nil || compiled.body == nil {
		if op.Body != nil && op.Body.Required && len(bytes.TrimSpace(body)) == 0 {
			return errors.New("request body is required")
		}
		return nil
	}
	if len(bytes.TrimSpace(body)) == 0 {
		if op.Body.Required {
			return errors.New("request body is required")
		}
		return nil
	}
	mediaType := contentType
	if parsed, _, err := mime.ParseMediaType(contentType); err == nil {
		mediaType = parsed
	}
	if mediaType != "" && !isJSONMediaType(mediaType) {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("request body is not valid JSON: %w", err)
	}
	if err := compiled.body.Validate(value); err != nil {
		return formatValidationError(err)
	}
	return nil
}

func (s *Spec) find(op *openapimcp.Operation) *operation {
	for i := range s.ops {
		if s.ops[i].op == op {
			return &s.ops[i]
		}
	}
	return nil
}

// formatValidationError bounds the validator's own rendering so a 400 names what was wrong
// without echoing a deep tree.
func formatValidationError(err error) error {
	msg := strings.ReplaceAll(err.Error(), "\n", " ")
	if len(msg) > 500 {
		msg = msg[:500] + "..."
	}
	return fmt.Errorf("request body does not match the operation schema: %s", msg)
}

// Operations lists the compiled operations in document order of precedence.
func (s *Spec) Operations() []OperationSummary {
	if s == nil {
		return nil
	}
	out := make([]OperationSummary, 0, len(s.ops))
	for _, op := range s.ops {
		out = append(out, OperationSummary{Method: op.op.Method, Path: op.op.Path, OperationID: op.op.ID, Summary: op.op.Summary, HasBody: op.op.Body != nil})
	}
	return out
}

// Metadata fills the server-computed fields of cfg from the document it was compiled from.
func Metadata(cfg *schemas.GenericOpenAPIConfig, spec *Spec) {
	if cfg == nil || spec == nil {
		return
	}
	data := []byte(cfg.Spec)
	cfg.SpecSize = len(data)
	cfg.SpecHash = openapimcp.SpecHash(data)
	cfg.SpecTitle = spec.Title
	cfg.OpenAPIVersion = spec.OpenAPIVersion
	cfg.OperationCount = len(spec.ops)
}
