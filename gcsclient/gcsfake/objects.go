package gcsfake

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// rewrite is a copy that objects.rewrite has started and not finished.
type rewrite struct {
	sourceBucket, sourceName           string
	destinationBucket, destinationName string
	// generation is the source's when the copy began: a source that changes under
	// a copy is a different object, and the token is no longer good for it.
	generation int64
	copied     int64
}

// serveObjects is the JSON API below /storage/v1: objects.get, objects.delete,
// objects.list and objects.rewrite.
func (f *Server) serveObjects(w http.ResponseWriter, r *http.Request, body []byte) {
	if refusal := f.authorize(r); refusal != nil {
		writeError(w, refusal)
		return
	}
	parts, ok := segments(r.URL.EscapedPath(), "/storage/v1/")
	if !ok {
		writeError(w, &apiError{http.StatusBadRequest, "badRequest", "the path is not percent-encoded"})
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, &apiError{http.StatusBadRequest, "badRequest", "the query string is malformed"})
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	var answer map[string]any
	var refusal *apiError
	status := http.StatusOK
	switch {
	case len(parts) == 3 && parts[0] == "b" && parts[2] == "o" && r.Method == http.MethodGet:
		answer, refusal = f.listObjects(parts[1], query)
	case len(parts) == 4 && parts[0] == "b" && parts[2] == "o" && r.Method == http.MethodGet:
		answer, refusal = f.getObject(parts[1], parts[3], query)
	case len(parts) == 4 && parts[0] == "b" && parts[2] == "o" && r.Method == http.MethodDelete:
		status = http.StatusNoContent
		refusal = f.deleteObject(parts[1], parts[3], query)
	case len(parts) == 9 && parts[0] == "b" && parts[2] == "o" && parts[4] == "rewriteTo" && parts[5] == "b" && parts[7] == "o" && r.Method == http.MethodPost:
		answer, refusal = f.rewriteObject(parts[1], parts[3], parts[6], parts[8], query, body)
	default:
		refusal = errNoSuchRoute
	}
	if refusal == nil && answer != nil {
		answer, refusal = selectFields(answer, query.Get("fields"))
	}
	switch {
	case refusal != nil:
		writeError(w, refusal)
	case answer == nil:
		w.WriteHeader(status)
	default:
		writeJSON(w, status, answer)
	}
}

// resource is the JSON API's object resource, as much of it as an object here
// has anything to say for. The 64-bit numbers are strings, as the API sends
// them.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects#resource
func resource(bucket, name string, held *object) map[string]any {
	generation := strconv.FormatInt(held.generation, 10)
	described := map[string]any{
		"kind":           "storage#object",
		"id":             bucket + "/" + name + "/" + generation,
		"name":           name,
		"bucket":         bucket,
		"generation":     generation,
		"metageneration": "1",
		"contentType":    held.contentType,
		"size":           strconv.Itoa(len(held.data)),
		"timeCreated":    held.updated.Format(timeFormat),
		"updated":        held.updated.Format(timeFormat),
		"etag":           base64.StdEncoding.EncodeToString([]byte(generation)),
	}
	if len(held.metadata) > 0 {
		metadata := map[string]any{}
		for item, value := range held.metadata {
			metadata[item] = value
		}
		described["metadata"] = metadata
	}
	return described
}

// timeFormat is RFC 3339 to the millisecond, which is how the API writes a time.
const timeFormat = "2006-01-02T15:04:05.000Z"

func (f *Server) getObject(bucket, name string, query url.Values) (map[string]any, *apiError) {
	if refusal := unknownParameter(query, "alt", "fields"); refusal != nil {
		return nil, refusal
	}
	// alt=media is the JSON API's download. gcsclient downloads through the XML
	// API, so it is not served here.
	if alt := query.Get("alt"); alt != "" && alt != "json" {
		return nil, &apiError{http.StatusBadRequest, "invalidParameter", "gcsfake serves only alt=json of objects.get"}
	}
	held, ok := f.buckets[bucket]
	if !ok {
		return nil, errNoSuchBucket
	}
	found, ok := held.objects[name]
	if !ok {
		return nil, errNoSuchObject(bucket, name)
	}
	return resource(bucket, name, found), nil
}

func (f *Server) deleteObject(bucket, name string, query url.Values) *apiError {
	if refusal := unknownParameter(query); refusal != nil {
		return refusal
	}
	held, ok := f.buckets[bucket]
	if !ok {
		return errNoSuchBucket
	}
	if _, ok := held.objects[name]; !ok {
		return errNoSuchObject(bucket, name)
	}
	delete(held.objects, name)
	return nil
}

// listObjects is one page of objects.list. Objects go in items[] and folded
// prefixes in prefixes[], each in order, and a page is a contiguous run of the
// whole ordered listing: maxResults bounds "the combined number of entries in
// items[] and prefixes[]".
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/list
func (f *Server) listObjects(bucket string, query url.Values) (map[string]any, *apiError) {
	if refusal := unknownParameter(query, "prefix", "delimiter", "pageToken", "maxResults", "fields"); refusal != nil {
		return nil, refusal
	}
	held, ok := f.buckets[bucket]
	if !ok {
		return nil, errNoSuchBucket
	}
	limit := pageSize
	if asked := query.Get("maxResults"); asked != "" {
		parsed, err := strconv.Atoi(asked)
		if err != nil || parsed < 1 {
			return nil, &apiError{http.StatusBadRequest, "invalid", "maxResults is not a positive number"}
		}
		limit = min(parsed, pageSize)
	}
	// A page token here is the last entry of the page before, which is all that
	// is needed to go on from it. The service's is opaque, and so is this to a
	// client that treats it as it should.
	var after string
	if token := query.Get("pageToken"); token != "" {
		decoded, err := base64.URLEncoding.DecodeString(token)
		if err != nil {
			return nil, &apiError{http.StatusBadRequest, "invalid", "Invalid page token."}
		}
		after = string(decoded)
	}

	prefix, delimiter := query.Get("prefix"), query.Get("delimiter")
	items, prefixes := []any{}, []any{}
	var last, next string
	for _, name := range held.names(prefix) {
		entry := name
		if delimiter != "" {
			if cut := strings.Index(name[len(prefix):], delimiter); cut >= 0 {
				entry = name[:len(prefix)+cut+len(delimiter)]
			}
		}
		// Everything folded into a prefix already listed sorts after it and begins
		// with it, and is skipped with it.
		if entry <= after || entry == last {
			continue
		}
		if len(items)+len(prefixes) == limit {
			next = base64.URLEncoding.EncodeToString([]byte(last))
			break
		}
		if entry == name {
			items = append(items, resource(bucket, name, held.objects[name]))
		} else {
			prefixes = append(prefixes, entry)
		}
		last = entry
	}
	page := map[string]any{"kind": "storage#objects"}
	if len(items) > 0 {
		page["items"] = items
	}
	if len(prefixes) > 0 {
		page["prefixes"] = prefixes
	}
	if next != "" {
		page["nextPageToken"] = next
	}
	return page, nil
}

// rewriteObject is one call of objects.rewrite. An empty request body gives the
// destination the source's metadata, which is the only form gcsclient uses.
// https://docs.cloud.google.com/storage/docs/json_api/v1/objects/rewrite
func (f *Server) rewriteObject(sourceBucket, sourceName, destinationBucket, destinationName string, query url.Values, body []byte) (map[string]any, *apiError) {
	if refusal := unknownParameter(query, "rewriteToken", "fields"); refusal != nil {
		return nil, refusal
	}
	if len(body) != 0 {
		return nil, &apiError{http.StatusBadRequest, "invalid", "gcsfake copies an object with its metadata, and takes no description of the destination"}
	}
	source, ok := f.buckets[sourceBucket]
	destination, destinationOK := f.buckets[destinationBucket]
	if !ok || !destinationOK {
		return nil, errNoSuchBucket
	}
	found, ok := source.objects[sourceName]
	if !ok {
		return nil, errNoSuchObject(sourceBucket, sourceName)
	}

	progress := &rewrite{sourceBucket, sourceName, destinationBucket, destinationName, found.generation, 0}
	if token := query.Get("rewriteToken"); token != "" {
		begun, ok := f.rewrites[token]
		if !ok || begun.sourceBucket != sourceBucket || begun.sourceName != sourceName || begun.destinationBucket != destinationBucket || begun.destinationName != destinationName || begun.generation != found.generation {
			return nil, &apiError{http.StatusBadRequest, "invalid", "Invalid rewrite token."}
		}
		delete(f.rewrites, token)
		progress = begun
	}

	size := int64(len(found.data))
	if f.rewriteBytesPerCall > 0 {
		progress.copied = min(size, progress.copied+f.rewriteBytesPerCall)
	} else {
		progress.copied = size
	}
	answer := map[string]any{
		"kind":                "storage#rewriteResponse",
		"totalBytesRewritten": strconv.FormatInt(progress.copied, 10),
		"objectSize":          strconv.FormatInt(size, 10),
		"done":                progress.copied == size,
	}
	if progress.copied < size {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			panic(err)
		}
		token := hex.EncodeToString(random)
		f.rewrites[token] = progress
		answer["rewriteToken"] = token
		return answer, nil
	}
	written := f.store(destination, destinationName, found.data, found.contentType, found.metadata)
	answer["resource"] = resource(destinationBucket, destinationName, written)
	return answer, nil
}

// selection is a parsed fields parameter: the names asked for, and under each
// the names asked for within it, nil where the whole of it was.
type selection map[string]selection

// knownFields are the names a fields parameter may use here: those of the
// resources this fake answers with. The service refuses a name that is not a
// field of the resource, and so must this, or a misspelt selection would pass
// here and fail there.
var knownFields = map[string]bool{
	"kind": true, "id": true, "name": true, "bucket": true, "generation": true, "metageneration": true,
	"contentType": true, "size": true, "timeCreated": true, "updated": true, "etag": true, "metadata": true,
	"items": true, "prefixes": true, "nextPageToken": true,
	"totalBytesRewritten": true, "objectSize": true, "done": true, "rewriteToken": true, "resource": true,
}

// selectFields applies a fields parameter to an answer: a partial response
// holds what was asked for and nothing else, so a client that reads a field it
// forgot to ask for finds it missing here as it would there.
// https://docs.cloud.google.com/storage/docs/json_api#partial-response
func selectFields(answer map[string]any, fields string) (map[string]any, *apiError) {
	if fields == "" {
		return answer, nil
	}
	chosen, rest, err := parseSelection(fields)
	if err == nil && rest != "" {
		err = fmt.Errorf("unexpected %q", rest)
	}
	if err != nil {
		return nil, &apiError{http.StatusBadRequest, "invalidParameter", "Invalid field selection " + fields + ": " + err.Error()}
	}
	// The selection is applied to a copy made through JSON, so that whatever the
	// answer was built from, what is walked here is maps and slices.
	encoded, err := json.Marshal(answer)
	if err != nil {
		panic(err)
	}
	var plain map[string]any
	if err := json.Unmarshal(encoded, &plain); err != nil {
		panic(err)
	}
	return chosen.apply(plain).(map[string]any), nil
}

// parseSelection reads "a,b(c,d),e" up to the end or an unmatched ")".
func parseSelection(text string) (selection, string, error) {
	chosen := selection{}
	for {
		end := strings.IndexAny(text, ",()")
		if end < 0 {
			end = len(text)
		}
		name := text[:end]
		if !knownFields[name] {
			return nil, "", fmt.Errorf("unknown field %q", name)
		}
		text = text[end:]
		chosen[name] = nil
		if strings.HasPrefix(text, "(") {
			within, rest, err := parseSelection(text[1:])
			if err != nil {
				return nil, "", err
			}
			if !strings.HasPrefix(rest, ")") {
				return nil, "", fmt.Errorf("unclosed ( after %q", name)
			}
			chosen[name], text = within, rest[1:]
		}
		if !strings.HasPrefix(text, ",") {
			return chosen, text, nil
		}
		text = text[1:]
	}
}

// apply keeps what the selection names, reaching into each element of a list.
func (s selection) apply(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		kept := map[string]any{}
		for name, within := range s {
			if member, ok := typed[name]; ok {
				kept[name] = member
				if within != nil {
					kept[name] = within.apply(member)
				}
			}
		}
		return kept
	case []any:
		for i, element := range typed {
			typed[i] = s.apply(element)
		}
	}
	return value
}
