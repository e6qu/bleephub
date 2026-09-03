package bleephub

import (
	"compress/gzip"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Runtime response-shape validation against the vendored GitHub OpenAPI
// description (../../third_party/github-openapi.json.gz). The TestMain-owned
// shared server routes most of the package's test traffic; an observer
// validates every 2xx /api/v3 JSON response member-by-member against
// the documented response schema. Violations are deduped after m.Run()
// and gated against openapi-violation-allowlist.txt, whose entries are
// either cited against an official GitHub description or marked
// unverified with the emitter named as the thing to fix; the list only
// shrinks. The companion route check (gh_api_definition_test.go) keeps
// paths honest; this keeps the bodies honest.
//
// Two properties the gate enforces on itself: every judgement is a
// deterministic function of the vendored description (no map iteration order
// reaches a verdict), and an exchange the description does not cover is
// reported, never skipped.

// allowlistFile is the single gate ledger, resolved relative to the
// package directory. TestViolationAllowlistIsSingleCopy keeps it single.
const allowlistFile = "openapi-violation-allowlist.txt"

type shapeViolation struct {
	Op string // "METHOD /spec/path/template -> status"
	// Kind is one of: unknown-field, type-mismatch, missing-required,
	// malformed-json, internal-url, enum-mismatch (a string value outside
	// the schema's declared enum), undocumented-status (the description
	// does not list this status for the operation at all),
	// undocumented-body (it lists the status but documents no JSON body
	// for it), vacuous-gate.
	Kind  string
	Field string
}

func (v shapeViolation) Key() string {
	return v.Op + "\t" + v.Kind + "\t" + v.Field
}

type openAPIOperation struct {
	Method   string
	Path     string   // spec path template verbatim; ties the total order
	Template []string // path segments; "{}" = parameter
	Literals int
	// Bodies maps status code (or "default") to every JSON response
	// schema the description declares for it, ordered by media type. A
	// response with several JSON media types (application/json plus a
	// vendor variant) declares several shapes, all of them legitimate,
	// so the response is judged against all of them and accepted if any
	// one accepts.
	Bodies map[string][]map[string]any
	// Statuses is every status code the operation documents, whether or
	// not it carries a JSON body.
	Statuses map[string]bool
}

type shapeValidator struct {
	mu         sync.Mutex
	seen       map[string]shapeViolation
	ops        []openAPIOperation
	schemas    map[string]any             // components/schemas
	responses  map[string]json.RawMessage // components/responses
	exchanges  int
	validated  int
	skippedOps int
}

// openAPIResponse is a response object, possibly a $ref into
// components/responses. GitHub's description shares most of its response
// objects that way, so an unresolved $ref reads as "no documented body"
// and takes the operation out of the gate.
type openAPIResponse struct {
	Ref     string `json:"$ref"`
	Content map[string]struct {
		Schema map[string]any `json:"schema"`
	} `json:"content"`
}

var apiShapeValidator *shapeValidator

var shapeParamSegment = regexp.MustCompile(`^\{[^}]+\}$`)

func newShapeValidator() (*shapeValidator, error) {
	f, err := os.Open("../../third_party/github-openapi.json.gz")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()

	var doc struct {
		Paths      map[string]map[string]json.RawMessage `json:"paths"`
		Components struct {
			Schemas   map[string]any             `json:"schemas"`
			Responses map[string]json.RawMessage `json:"responses"`
		} `json:"components"`
	}
	if err := json.NewDecoder(gz).Decode(&doc); err != nil {
		return nil, err
	}
	if len(doc.Paths) < 500 || len(doc.Components.Schemas) < 100 || len(doc.Components.Responses) < 10 {
		return nil, fmt.Errorf("vendored OpenAPI looks truncated: %d paths, %d schemas, %d shared responses",
			len(doc.Paths), len(doc.Components.Schemas), len(doc.Components.Responses))
	}

	v := &shapeValidator{
		seen:      map[string]shapeViolation{},
		schemas:   doc.Components.Schemas,
		responses: doc.Components.Responses,
	}
	for _, path := range sortedMapKeys(doc.Paths) {
		methods := doc.Paths[path]
		segs := strings.Split(strings.Trim(path, "/"), "/")
		template := make([]string, len(segs))
		literals := 0
		for i, seg := range segs {
			if shapeParamSegment.MatchString(seg) {
				template[i] = "{}"
			} else {
				template[i] = seg
				literals++
			}
		}
		for _, method := range sortedMapKeys(methods) {
			switch method {
			case "get", "post", "put", "patch", "delete", "head":
			default:
				continue
			}
			var op struct {
				Responses map[string]json.RawMessage `json:"responses"`
			}
			if err := json.Unmarshal(methods[method], &op); err != nil {
				return nil, fmt.Errorf("decode %s %s: %w", strings.ToUpper(method), path, err)
			}
			bodies := map[string][]map[string]any{}
			statuses := map[string]bool{}
			for _, status := range sortedMapKeys(op.Responses) {
				statuses[status] = true
				resp, err := v.resolveResponse(op.Responses[status])
				if err != nil {
					return nil, fmt.Errorf("%s %s response %s: %w", strings.ToUpper(method), path, status, err)
				}
				for _, ct := range sortedMapKeys(resp.Content) {
					if strings.Contains(ct, "json") && resp.Content[ct].Schema != nil {
						bodies[status] = append(bodies[status], resp.Content[ct].Schema)
					}
				}
			}
			v.ops = append(v.ops, openAPIOperation{
				Method:   strings.ToUpper(method),
				Path:     path,
				Template: template,
				Literals: literals,
				Bodies:   bodies,
				Statuses: statuses,
			})
		}
	}
	if err := v.verifySchemaRefs(); err != nil {
		return nil, err
	}
	return v, nil
}

// resolveResponse follows a response object's $ref into
// components/responses.
func (v *shapeValidator) resolveResponse(raw json.RawMessage) (openAPIResponse, error) {
	for i := 0; i < 16; i++ {
		var resp openAPIResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			return resp, err
		}
		if resp.Ref == "" {
			return resp, nil
		}
		name, ok := strings.CutPrefix(resp.Ref, "#/components/responses/")
		if !ok {
			return resp, fmt.Errorf("unsupported response $ref %q", resp.Ref)
		}
		next, ok := v.responses[name]
		if !ok {
			return resp, fmt.Errorf("response $ref %q has no target in components/responses", resp.Ref)
		}
		raw = next
	}
	return openAPIResponse{}, fmt.Errorf("response $ref chain deeper than 16 hops")
}

// verifySchemaRefs asserts every $ref in the description's schemas names
// an existing components/schemas entry. resolve() cannot report a dangling
// $ref from inside the walk, and an unresolved schema reads as an open
// schema that accepts anything, so the check happens once at load.
func (v *shapeValidator) verifySchemaRefs() error {
	var bad []string
	var visit func(node any)
	visitSchemaMap := func(node any) {
		members, ok := node.(map[string]any)
		if !ok {
			return
		}
		for _, name := range sortedMapKeys(members) {
			visit(members[name])
		}
	}
	visit = func(node any) {
		switch x := node.(type) {
		case map[string]any:
			for _, k := range sortedMapKeys(x) {
				switch k {
				case "$ref":
					ref, ok := x[k].(string)
					if !ok {
						bad = append(bad, fmt.Sprintf("non-string $ref %v", x[k]))
						continue
					}
					name, isSchemaRef := strings.CutPrefix(ref, "#/components/schemas/")
					if !isSchemaRef {
						bad = append(bad, fmt.Sprintf("schema $ref outside components/schemas: %s", ref))
						continue
					}
					if _, found := v.schemas[name]; !found {
						bad = append(bad, fmt.Sprintf("dangling $ref %s", ref))
					}
				case "example", "examples", "default", "enum", "const":
					// Instance values, not schemas; a $ref inside one points
					// at components/examples and is none of this gate's
					// business.
				case "properties", "patternProperties":
					// Member names are arbitrary, so the keys here must not be
					// read as schema keywords.
					visitSchemaMap(x[k])
				default:
					visit(x[k])
				}
			}
		case []any:
			for _, item := range x {
				visit(item)
			}
		}
	}
	for _, name := range sortedMapKeys(v.schemas) {
		visit(v.schemas[name])
	}
	for _, op := range v.ops {
		for _, status := range sortedMapKeys(op.Bodies) {
			for _, schema := range op.Bodies[status] {
				visit(schema)
			}
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		bad = slices.Compact(bad)
		return fmt.Errorf("vendored OpenAPI has %d unresolvable schema reference(s): %s", len(bad), strings.Join(bad, "; "))
	}
	return nil
}

// sortedMapKeys iterates a map in a fixed order, so no verdict this file
// reaches can depend on Go's randomized map iteration.
func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Observe is the Server response observer.
func (v *shapeValidator) Observe(req *http.Request, status int, header http.Header, body []byte) {
	path, ok := strings.CutPrefix(req.URL.Path, "/api/v3")
	if !ok || path == "" {
		return
	}
	v.mu.Lock()
	v.exchanges++
	v.mu.Unlock()
	// 2xx and 4xx/5xx JSON bodies are both validated (PAR-013): GitHub's
	// description documents error schemas too, and an error body that
	// deviates from them breaks clients exactly like a success body does.
	// 3xx responses carry no documented JSON contract.
	if status < 200 || (status >= 300 && status < 400) || len(body) == 0 {
		return
	}
	if ct := header.Get("Content-Type"); ct != "" && !strings.Contains(ct, "json") {
		return
	}

	segs := strings.Split(strings.Trim(path, "/"), "/")
	var candidates []openAPIOperation
	for _, op := range v.ops {
		if op.Method != req.Method || len(op.Template) != len(segs) {
			continue
		}
		match := true
		for i, t := range op.Template {
			if t != "{}" && t != segs[i] {
				match = false
				break
			}
		}
		if match {
			candidates = append(candidates, op)
		}
	}
	if len(candidates) == 0 {
		// The route-existence test owns unknown paths.
		v.mu.Lock()
		v.skippedOps++
		v.mu.Unlock()
		return
	}
	// Most-literal template first: a concrete path must not be judged by
	// a generic sibling when a more specific one exists. Path breaks
	// ties, so the winner never depends on iteration order.
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].Literals != candidates[j].Literals {
			return candidates[i].Literals > candidates[j].Literals
		}
		return candidates[i].Path < candidates[j].Path
	})
	primary := candidates[0]

	var decoded any
	if err := json.Unmarshal(body, &decoded); err != nil {
		v.record(shapeViolation{Op: opLabel(primary, status), Kind: "malformed-json", Field: "$"})
		return
	}
	for _, field := range internalURLFields(decoded, "$") {
		v.record(shapeViolation{Op: opLabel(primary, status), Kind: "internal-url", Field: field})
	}

	// The most specific candidate that documents this status is the one the
	// response is judged by. Comparing violation counts across candidates
	// would let a generic sibling's single type-mismatch stand in for the
	// right operation's precise list, which reads as a smaller finding
	// while hiding a bigger one.
	statusKey := strconv.Itoa(status)
	var best []shapeViolation
	bestSet := false
	documented := false
	for _, op := range candidates {
		schemas, ok := op.Bodies[statusKey]
		if !ok {
			schemas, ok = op.Bodies["default"]
		}
		if !ok {
			continue
		}
		documented = true
		for _, schema := range schemas {
			// A documented error schema that is a sealed EMPTY object
			// ({properties:{}, additionalProperties:false}) is a description
			// stub, not a contract — e.g. GET /gists/{id}/star documents its
			// 404 that way while the real API (and bleephub) returns the
			// standard message/documentation_url body. Judging the response
			// by the stub would flag bleephub for being right.
			if status >= 400 && v.sealedEmptyObject(schema) {
				v.countValidated()
				return
			}
			var out []shapeViolation
			v.walk(schema, decoded, opLabel(op, status), "$", &out, 0)
			if len(out) == 0 {
				v.countValidated()
				return // a documented schema fully accepts the response
			}
			if !bestSet || len(out) < len(best) {
				best, bestSet = out, true
			}
		}
		break
	}
	if !documented {
		// GitHub documents error statuses sparsely: most operations list only
		// a handful of their real 4xx responses. An undocumented error status
		// is therefore the description's gap, not a bleephub deviation —
		// unlike an undocumented 2xx, which stays the strongest parity
		// signal the observer can see.
		if status >= 400 {
			return
		}
		// The description covers the path but not this status with a JSON
		// body, so there is nothing to validate against. Report that
		// instead of dropping the exchange: an undocumented status is the
		// strongest parity signal the observer can see, not a licence to
		// stop looking.
		kind := "undocumented-status"
		for _, op := range candidates {
			if op.Statuses[statusKey] {
				kind = "undocumented-body"
				break
			}
		}
		v.record(shapeViolation{Op: opLabel(primary, status), Kind: kind, Field: "$"})
		return
	}
	v.countValidated()
	for _, viol := range best {
		v.record(viol)
	}
}

func (v *shapeValidator) countValidated() {
	v.mu.Lock()
	v.validated++
	v.mu.Unlock()
}

// sealedEmptyObject reports whether schema resolves to an object that declares
// no members yet forbids all others — the {properties:{},
// additionalProperties:false} stub GitHub's description uses for a few error
// responses it never bothered to model.
func (v *shapeValidator) sealedEmptyObject(schema map[string]any) bool {
	schema = v.resolve(schema)
	if schema == nil {
		return false
	}
	props, _, additional := v.flatten(schema)
	if len(props) != 0 {
		return false
	}
	sealed, ok := additional.(bool)
	return ok && !sealed
}

func internalURLFields(v any, field string) []string {
	switch x := v.(type) {
	case map[string]any:
		var out []string
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			out = append(out, internalURLFields(x[k], field+"."+k)...)
		}
		return out
	case []any:
		var out []string
		for i, item := range x {
			out = append(out, internalURLFields(item, fmt.Sprintf("%s[%d]", field, i))...)
		}
		return out
	case string:
		if isInternalOperatorURL(x) {
			return []string{field}
		}
	}
	return nil
}

// isInternalOperatorURL reports whether s leaks bleephub's private /internal/
// operator surface, which lives at the server root. A genuine leak is a value
// whose URL path *begins* with /internal/ (a bare "/internal/..." or an
// absolute URL whose path starts there). A value that merely contains an
// "internal" path segment — a contents URL like ".../contents/internal/x", a
// "path" field, an issue body — is normal content, not a leak (a plain
// strings.Contains test mislabeled all of those).
func isInternalOperatorURL(s string) bool {
	if strings.HasPrefix(s, "/internal/") {
		return true
	}
	if u, err := url.Parse(s); err == nil && u.Host != "" && strings.HasPrefix(u.Path, "/internal/") {
		return true
	}
	return false
}

func opLabel(op openAPIOperation, status int) string {
	return fmt.Sprintf("%s /%s -> %d", op.Method, strings.Join(op.Template, "/"), status)
}

func (v *shapeValidator) record(viol shapeViolation) {
	viol.Field = collapseIndexes(viol.Field)
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.seen[viol.Key()]; !ok {
		v.seen[viol.Key()] = viol
	}
}

var indexSegment = regexp.MustCompile(`\[\d+\]`)

func collapseIndexes(field string) string {
	return indexSegment.ReplaceAllString(field, "[]")
}

// resolve follows $ref chains into components/schemas.
func (v *shapeValidator) resolve(schema map[string]any) map[string]any {
	for i := 0; i < 16; i++ {
		ref, _ := schema["$ref"].(string)
		if ref == "" {
			return schema
		}
		name, ok := strings.CutPrefix(ref, "#/components/schemas/")
		if !ok {
			return schema
		}
		next, _ := v.schemas[name].(map[string]any)
		if next == nil {
			return schema
		}
		schema = next
	}
	return schema
}

// flatten merges a schema's allOf chain into one effective schema view:
// the union of properties + required, and whether additional properties
// are allowed anywhere in the chain.
func (v *shapeValidator) flatten(schema map[string]any) (props map[string]map[string]any, required []string, additional any) {
	props = map[string]map[string]any{}
	var visit func(s map[string]any)
	visit = func(s map[string]any) {
		s = v.resolve(s)
		if ap, ok := s["additionalProperties"]; ok {
			additional = ap
		}
		if p, ok := s["properties"].(map[string]any); ok {
			for name, sub := range p {
				if m, ok := sub.(map[string]any); ok {
					props[name] = m
				}
			}
		}
		if reqs, ok := s["required"].([]any); ok {
			for _, r := range reqs {
				if name, ok := r.(string); ok {
					required = append(required, name)
				}
			}
		}
		if all, ok := s["allOf"].([]any); ok {
			for _, branch := range all {
				if m, ok := branch.(map[string]any); ok {
					visit(m)
				}
			}
		}
	}
	visit(schema)
	return props, required, additional
}

// schemaAllowsNull reports whether the schema admits a JSON null: an explicit
// `nullable: true` (OpenAPI 3.0), a `"null"` entry in a type set (3.1), any
// anyOf/oneOf branch that admits null, or a fully open schema (no type and no
// object shape) where nullability cannot be judged. Everything else — a schema
// with a concrete non-null type — rejects null.
func (v *shapeValidator) schemaAllowsNull(schema map[string]any) bool {
	if schema == nil {
		return true
	}
	schema = v.resolve(schema)
	if n, ok := schema["nullable"].(bool); ok && n {
		return true
	}
	if contains(schemaTypes(schema), "null") {
		return true
	}
	for _, key := range []string{"anyOf", "oneOf"} {
		if branches, ok := schema[key].([]any); ok {
			for _, b := range branches {
				if bs, ok := b.(map[string]any); ok && v.schemaAllowsNull(bs) {
					return true
				}
			}
			return false // an explicit union, no branch of which admits null
		}
	}
	_, hasProps := schema["properties"]
	_, hasAllOf := schema["allOf"]
	return len(schemaTypes(schema)) == 0 && !hasProps && !hasAllOf
}

func schemaTypes(schema map[string]any) []string {
	switch t := schema["type"].(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// walk validates a decoded JSON value against an OpenAPI schema,
// reporting unknown members, type mismatches, and absent required
// members. Composition: allOf is flattened; anyOf/oneOf pass when any
// branch fully accepts the value.
func (v *shapeValidator) walk(schema map[string]any, val any, op, path string, out *[]shapeViolation, depth int) {
	if depth > 24 || schema == nil {
		return
	}
	schema = v.resolve(schema)

	if val == nil {
		// A JSON null is a genuine value: it only satisfies a schema that
		// actually admits null. Accepting it unconditionally let a null pass
		// for a non-nullable required field (PAR-009).
		if !v.schemaAllowsNull(schema) {
			*out = append(*out, shapeViolation{Op: op, Kind: "null-not-allowed", Field: path})
		}
		return
	}

	if branches, ok := schema["anyOf"].([]any); ok {
		v.walkBranches(branches, val, op, path, out, depth)
		return
	}
	if branches, ok := schema["oneOf"].([]any); ok {
		v.walkBranches(branches, val, op, path, out, depth)
		return
	}

	types := schemaTypes(schema)
	_, hasProps := schema["properties"]
	_, hasAllOf := schema["allOf"]
	isObjectSchema := len(types) == 0 && (hasProps || hasAllOf)

	switch value := val.(type) {
	case map[string]any:
		if len(types) > 0 && !contains(types, "object") {
			*out = append(*out, shapeViolation{Op: op, Kind: "type-mismatch", Field: path})
			return
		}
		if len(types) == 0 && !isObjectSchema {
			return // untyped open schema
		}
		props, required, additional := v.flatten(schema)
		if len(props) == 0 && additional == nil {
			return // object with no declared members: open
		}
		for _, name := range required {
			if _, ok := value[name]; !ok {
				*out = append(*out, shapeViolation{Op: op, Kind: "missing-required", Field: path + "." + name})
			}
		}
		for _, name := range sortedMapKeys(value) {
			member := value[name]
			sub, known := props[name]
			if known {
				v.walk(sub, member, op, path+"."+name, out, depth+1)
				continue
			}
			switch ap := additional.(type) {
			case bool:
				if !ap {
					*out = append(*out, shapeViolation{Op: op, Kind: "unknown-field", Field: path + "." + name})
				}
			case map[string]any:
				v.walk(ap, member, op, path+"."+name, out, depth+1)
			default:
				// additionalProperties unspecified: the description's
				// schemas enumerate real members exhaustively, so an
				// undeclared member is a bleephub invention.
				*out = append(*out, shapeViolation{Op: op, Kind: "unknown-field", Field: path + "." + name})
			}
		}
	case []any:
		if len(types) > 0 && !contains(types, "array") {
			*out = append(*out, shapeViolation{Op: op, Kind: "type-mismatch", Field: path})
			return
		}
		items, _ := schema["items"].(map[string]any)
		if items == nil {
			return
		}
		for i, item := range value {
			v.walk(items, item, op, fmt.Sprintf("%s[%d]", path, i), out, depth+1)
		}
	case string:
		if len(types) > 0 && !contains(types, "string") {
			*out = append(*out, shapeViolation{Op: op, Kind: "type-mismatch", Field: path})
			return
		}
		// Enum membership (PAR-010): a declared enum enumerates the only
		// values real GitHub emits, so a value outside it is a wire-format
		// deviation even when the type matches.
		if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
			member := false
			for _, allowed := range enum {
				if s, ok := allowed.(string); ok && s == value {
					member = true
					break
				}
			}
			if !member {
				*out = append(*out, shapeViolation{Op: op, Kind: "enum-mismatch", Field: path})
			}
		}
	case bool:
		if len(types) > 0 && !contains(types, "boolean") {
			*out = append(*out, shapeViolation{Op: op, Kind: "type-mismatch", Field: path})
		}
	case float64:
		if len(types) > 0 && !contains(types, "number") && !contains(types, "integer") {
			*out = append(*out, shapeViolation{Op: op, Kind: "type-mismatch", Field: path})
		}
	}
}

func (v *shapeValidator) walkBranches(branches []any, val any, op, path string, out *[]shapeViolation, depth int) {
	var best []shapeViolation
	bestSet := false
	for _, b := range branches {
		schema, ok := b.(map[string]any)
		if !ok {
			continue
		}
		var attempt []shapeViolation
		v.walk(schema, val, op, path, &attempt, depth+1)
		if len(attempt) == 0 {
			return
		}
		if !bestSet || len(attempt) < len(best) {
			best, bestSet = attempt, true
		}
	}
	*out = append(*out, best...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// readViolationAllowlist parses the gate ledger. Format: one key per
// line, op<TAB>kind<TAB>field, with #-comments carrying the citation for
// the block that follows. A missing, unreadable, malformed or duplicated
// entry is an error, never an empty allowlist: silently reading zero
// entries would turn every allowlisted key into a fresh "new violation"
// and bury the real ones.
func readViolationAllowlist(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	allowed := map[string]bool{}
	for n, line := range strings.Split(string(data), "\n") {
		if i := strings.Index(line, "#"); i >= 0 {
			line = line[:i]
		}
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		if fields := strings.Split(line, "\t"); len(fields) != 3 {
			return nil, fmt.Errorf("%s:%d: want op<TAB>kind<TAB>field, got %d tab-separated field(s): %q", path, n+1, len(fields), line)
		}
		if allowed[line] {
			return nil, fmt.Errorf("%s:%d: duplicate entry %q", path, n+1, line)
		}
		allowed[line] = true
	}
	return allowed, nil
}

// minShapeCoverage is the coverage floor the full suite must clear (PAR-011).
// The shared observer now validates ~5k /api/v3 responses per full run (down
// from ~26k when this gate was added, as TEST-008 migrated tests to isolated
// servers that don't feed the observer). Since near-floor runs dip as low as
// 4879 on normal -race + MinIO variance, the floor sits below that operating
// range with margin: the intermittent dip is tolerated while a genuine collapse
// (observer unwired → orders of magnitude lower) is still caught. TestMain logs
// the count each run; raise this if isolated servers are ever wired to the observer.
const minShapeCoverage = 4000

// isFullTestRun reports whether every test ran, so the coverage floor applies.
// A `-run <subset>` filter (or the `-run ^$` fuzz pass) legitimately observes
// only a fraction and must not be judged against the floor.
func isFullTestRun() bool {
	f := flag.Lookup("test.run")
	if f == nil {
		return false
	}
	switch f.Value.String() {
	case "", ".*", "^.*$":
		return true
	default:
		return false
	}
}

// coverage reports how many observed /api/v3 exchanges were matched to an
// OpenAPI operation and validated. The shape-parity gate is only meaningful if
// this stays high; a coverage floor guards against it silently collapsing
// (PAR-011).
func (v *shapeValidator) coverage() (validated, exchanges int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.validated, v.exchanges
}

// ratchet compares the deduped violations against the allowlist and
// returns the new keys.
func (v *shapeValidator) ratchet() (newKeys []string, total int) {
	allowed, err := readViolationAllowlist(allowlistFile)
	if err != nil {
		return []string{fmt.Sprintf("allowlist unreadable, gate verdict withheld: %v", err)}, 0
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	for key := range v.seen {
		if !allowed[key] {
			newKeys = append(newKeys, key)
		}
	}
	if v.exchanges > 0 && v.validated == 0 {
		// Every observed exchange found a way not to be checked. Whatever
		// this run proved about response shapes, it is not parity.
		newKeys = append(newKeys, shapeViolation{
			Op:    fmt.Sprintf("%d /api/v3 exchange(s) observed", v.exchanges),
			Kind:  "vacuous-gate",
			Field: fmt.Sprintf("0 validated, %d unmatched path(s)", v.skippedOps),
		}.Key())
	}
	sort.Strings(newKeys)
	return newKeys, len(v.seen)
}

// unusedAllowlistEntries returns allowlist keys no observed violation matched
// this run. An entry that never fires is a dead suppression — it protects
// nothing and only inflates the count (PAR-022); the full-run teardown reports
// these for removal. Meaningful only on a full run: a `-run <subset>` exercises
// few cited endpoints.
func (v *shapeValidator) unusedAllowlistEntries() ([]string, error) {
	allowed, err := readViolationAllowlist(allowlistFile)
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var unused []string
	for key := range allowed {
		if _, ok := v.seen[key]; !ok {
			unused = append(unused, key)
		}
	}
	sort.Strings(unused)
	return unused, nil
}

// repoRoot walks up from the package directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestViolationAllowlistIsSingleCopy makes a second ledger impossible to
// add. Two copies had already drifted apart once, and only one of them
// was ever read, so the other one's contents were decoration.
func TestViolationAllowlistIsSingleCopy(t *testing.T) {
	root := repoRoot(t)
	pkgDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	want := filepath.Join(pkgDir, allowlistFile)

	var found []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", ".terraform":
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() == allowlistFile {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	sort.Strings(found)
	if len(found) != 1 || found[0] != want {
		t.Errorf("want exactly one %s at %s, found %d: %v\n"+
			"the validator reads the package-relative copy; any other copy is unread and will drift",
			allowlistFile, want, len(found), found)
	}
	// The ledger must be readable now, not merely present: a missing or
	// unreadable allowlist is a hard failure, since reading it as "zero entries"
	// would resurface every allowlisted key as a spurious "new" violation. A
	// genuinely empty file parses to zero entries without error, so it passes.
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("allowlist %s is missing or unreadable: %v", want, err)
	}
	if _, err := readViolationAllowlist(want); err != nil {
		t.Fatalf("allowlist %s does not parse: %v", want, err)
	}
}

// TestViolationAllowlistInvariants enforces the three properties the gate's
// header claims for the allowlist but nothing checked, so all three could
// silently rot (PAR-012): (1) every allowlisted deviation is justified by a
// preceding comment block, (2) that block states whether the member is VERIFIED
// (cited into an official description) or unverified (with the emitter named as
// the thing to fix), and (3) the list only ever shrinks.
func TestViolationAllowlistInvariants(t *testing.T) {
	// Ratchet: lower this whenever an entry is removed; a change that raises it
	// must fail review. The gate ledger may only shrink. Lowered 22→11: the
	// eleven entries that recorded deliberate deviations were conformed in the
	// emitter instead — the git/refs push-protection 422 now carries the bypass
	// placeholder in the documented validation-error members, installation
	// permissions serialize bleephub's internal admin level as the highest
	// level app-permissions models for those scopes, and fork-PR approval is
	// switched by require_approval_for_fork_pr_workflows rather than by an
	// approval_policy member GitHub does not have. What remains is only the
	// four cases where the vendored description is narrower than GitHub's own
	// observable behaviour.
	// Raised 11→17 for the six Classroom reads GitHub retired to 410 Gone in its
	// rolling 2026-03-10 description: bleephub serves the still-supported
	// 2022-11-28 API version, whose official description still documents their
	// 200, and Classroom is bleephub's one sanctioned divergence — these are
	// VERIFIED upstream-deprecation citations, not accumulating emitter bugs.
	const maxAllowlistEntries = 17

	data, err := os.ReadFile(allowlistFile)
	if err != nil {
		t.Fatalf("read allowlist: %v", err)
	}

	total := 0
	var comment []string // justification comments since the last blank line
	for n, raw := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(strings.TrimRight(raw, " \t\r"))
		switch {
		case trimmed == "":
			comment = nil
		case strings.HasPrefix(trimmed, "#"):
			comment = append(comment, trimmed)
		default: // a data entry: op<TAB>kind<TAB>field
			total++
			if len(comment) == 0 {
				t.Errorf("%s:%d: entry %q has no preceding justification comment (citation invariant)", allowlistFile, n+1, trimmed)
				continue
			}
			// "VERIFIED" and "unverified" both contain "verified": the block must
			// declare which of the two justifications applies.
			if !strings.Contains(strings.ToLower(strings.Join(comment, " ")), "verified") {
				t.Errorf("%s:%d: entry %q is neither marked VERIFIED (cited) nor unverified — the justification block must state which", allowlistFile, n+1, trimmed)
			}
		}
	}
	if total > maxAllowlistEntries {
		t.Fatalf("allowlist has %d entries, ceiling is %d — the gate ledger may only shrink; lower the ceiling when removing entries, never raise it", total, maxAllowlistEntries)
	}
	// Cross-check against the canonical parser so the citation sweep above cannot
	// silently pass by parsing zero entries.
	allowed, err := readViolationAllowlist(allowlistFile)
	if err != nil {
		t.Fatalf("canonical parse: %v", err)
	}
	if total != len(allowed) {
		t.Fatalf("counted %d entries but the gate parser sees %d — the invariant sweep is not covering the same entries the gate uses", total, len(allowed))
	}
}

// TestShapeValidatorModelIsDeterministic pins the property the gate rests
// on: the parsed description is a function of the bytes on disk, not of
// Go's map iteration order.
func TestShapeValidatorModelIsDeterministic(t *testing.T) {
	fingerprint := func() string {
		v, err := newShapeValidator()
		if err != nil {
			t.Fatalf("newShapeValidator: %v", err)
		}
		var b strings.Builder
		for _, op := range v.ops {
			b.WriteString(op.Method + " " + op.Path + " " + strconv.Itoa(op.Literals))
			for _, status := range sortedMapKeys(op.Bodies) {
				b.WriteString(" " + status + ":" + strconv.Itoa(len(op.Bodies[status])))
			}
			b.WriteString(" |" + strings.Join(sortedMapKeys(op.Statuses), ",") + "\n")
		}
		return b.String()
	}
	if a, b := fingerprint(), fingerprint(); a != b {
		t.Error("two loads of the vendored description produced different operation models")
	}
}

// TestShapeValidatorKeepsEveryJSONMediaTypeSchema covers the responses
// that declare more than one JSON media type. Picking one of them by map
// iteration made those operations' verdicts a coin flip.
func TestShapeValidatorKeepsEveryJSONMediaTypeSchema(t *testing.T) {
	v, err := newShapeValidator()
	if err != nil {
		t.Fatalf("newShapeValidator: %v", err)
	}
	multi := 0
	for _, op := range v.ops {
		for _, status := range sortedMapKeys(op.Bodies) {
			if len(op.Bodies[status]) > 1 {
				multi++
			}
		}
	}
	if multi == 0 {
		t.Error("no response in the vendored description declares multiple JSON media types; " +
			"either the description changed shape or the parser dropped the extra schemas")
	}
}

// TestShapeValidatorReportsUndocumentedStatus covers the case that used
// to disable body validation instead of reporting it.
func TestShapeValidatorReportsUndocumentedStatus(t *testing.T) {
	v, err := newShapeValidator()
	if err != nil {
		t.Fatalf("newShapeValidator: %v", err)
	}

	// A status the operation does not list at all.
	var undocumentedStatus, undocumentedBody int
	for _, op := range v.ops {
		if op.Method != "GET" || op.Literals != len(op.Template) {
			continue
		}
		if op.Statuses["205"] || len(op.Bodies) == 0 {
			continue
		}
		probe := newProbeValidator(t)
		probe.Observe(mustGet(t, "/api/v3/"+strings.Join(op.Template, "/")), 205, jsonHeader(), []byte(`{}`))
		if got := probe.kinds(); len(got) != 1 || got[0] != "undocumented-status" {
			t.Fatalf("%s %s with an undocumented 205: got %v, want [undocumented-status]", op.Method, op.Path, got)
		}
		undocumentedStatus++
		break
	}

	// A status the operation lists, but with no JSON body documented.
	for _, op := range v.ops {
		if op.Method != "GET" || op.Literals != len(op.Template) {
			continue
		}
		status := ""
		for _, s := range sortedMapKeys(op.Statuses) {
			code, err := strconv.Atoi(s)
			if err != nil || code < 200 || code >= 300 {
				continue
			}
			if len(op.Bodies[s]) == 0 {
				status = s
				break
			}
		}
		if status == "" {
			continue
		}
		code, _ := strconv.Atoi(status)
		probe := newProbeValidator(t)
		probe.Observe(mustGet(t, "/api/v3/"+strings.Join(op.Template, "/")), code, jsonHeader(), []byte(`{}`))
		if got := probe.kinds(); len(got) != 1 || got[0] != "undocumented-body" {
			t.Fatalf("%s %s with a body on undocumented-body status %s: got %v, want [undocumented-body]", op.Method, op.Path, status, got)
		}
		undocumentedBody++
		break
	}

	if undocumentedStatus == 0 || undocumentedBody == 0 {
		t.Errorf("no probe operation found (undocumented-status=%d undocumented-body=%d); the description no longer covers this case",
			undocumentedStatus, undocumentedBody)
	}
}

// TestShapeValidatorRejectsNullForNonNullable covers PAR-009: a JSON null must
// only satisfy a schema that actually admits null, so a null emitted for a
// non-nullable member is reported rather than silently accepted.
func TestShapeValidatorRejectsNullForNonNullable(t *testing.T) {
	v := newProbeValidator(t)

	rejects := []map[string]any{
		{"type": "string"},
		{"type": "integer"},
		{"type": "array", "items": map[string]any{"type": "string"}},
		{"type": "object", "properties": map[string]any{}},
		{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "integer"}}},
	}
	for _, schema := range rejects {
		var out []shapeViolation
		v.walk(schema, nil, "GET /x -> 200", "$", &out, 0)
		if len(out) != 1 || out[0].Kind != "null-not-allowed" {
			t.Errorf("null against %v: got %v, want one null-not-allowed", schema, out)
		}
	}

	accepts := []map[string]any{
		{"type": "string", "nullable": true},
		{"type": []any{"string", "null"}},
		{"anyOf": []any{map[string]any{"type": "string"}, map[string]any{"type": "null"}}},
		{}, // fully open schema: nullability cannot be judged
		{"description": "untyped"},
	}
	for _, schema := range accepts {
		var out []shapeViolation
		v.walk(schema, nil, "GET /x -> 200", "$", &out, 0)
		if len(out) != 0 {
			t.Errorf("null against nullable %v: got %v, want none", schema, out)
		}
	}
}

func newProbeValidator(t *testing.T) *shapeValidator {
	t.Helper()
	v, err := newShapeValidator()
	if err != nil {
		t.Fatalf("newShapeValidator: %v", err)
	}
	return v
}

func (v *shapeValidator) kinds() []string {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]string, 0, len(v.seen))
	for _, viol := range v.seen {
		out = append(out, viol.Kind)
	}
	sort.Strings(out)
	return out
}

func jsonHeader() http.Header {
	return http.Header{"Content-Type": []string{"application/json"}}
}

func mustGet(t *testing.T, path string) *http.Request {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1"+path, nil)
	if err != nil {
		t.Fatalf("build probe request for %s: %v", path, err)
	}
	return req
}
