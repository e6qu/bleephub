package bleephub

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

// decodeIssueLabelsBody decodes the add-labels body. GitHub accepts either a
// bare array `["bug"]` or the object form `{"labels":[...]}`; an empty body
// means no labels. Returns false (after writing a 400) on malformed JSON.
func decodeIssueLabelsBody(w http.ResponseWriter, r *http.Request) ([]string, bool) {
	raw, ok := readLimitedBody(w, r, maxJSONBodyBytes)
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, true
	}
	if trimmed[0] == '[' {
		var labels []string
		if err := json.Unmarshal(trimmed, &labels); err != nil {
			writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
			return nil, false
		}
		return labels, true
	}
	var obj struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal(trimmed, &obj); err != nil {
		writeGHError(w, http.StatusBadRequest, "Problems parsing JSON")
		return nil, false
	}
	return obj.Labels, true
}

// GitHub's REST API accepts both typed JSON booleans/ints and string-coerced
// ones (`{"private":"false"}`, what `gh api -f` sends). Use flexBool/flexInt in
// request structs for parity; they Marshal back to the typed form.

// flexBool decodes `true`/`false` or `"true"`/`"false"`; empty string → false.
type flexBool bool

func (b *flexBool) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*b = false
		return nil
	}
	if data[0] == 't' || data[0] == 'f' {
		var v bool
		if err := json.Unmarshal(data, &v); err != nil {
			return err
		}
		*b = flexBool(v)
		return nil
	}
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return err
	}
	switch s {
	case "true", "1", "yes":
		*b = true
	case "false", "0", "no", "":
		*b = false
	default:
		// Deliberately not json.UnmarshalTypeError: its Error method
		// dereferences Type unconditionally, so a nil-Type value panics when
		// formatted.
		return fmt.Errorf("invalid boolean value %q", s)
	}
	return nil
}

func (b flexBool) MarshalJSON() ([]byte, error) {
	if b {
		return []byte("true"), nil
	}
	return []byte("false"), nil
}

// flexInt decodes either typed numbers or string-coerced ints.
type flexInt int

func (i *flexInt) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*i = 0
		return nil
	}
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		if s == "" {
			*i = 0
			return nil
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			return err
		}
		*i = flexInt(n)
		return nil
	}
	var n int
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*i = flexInt(n)
	return nil
}

func (i flexInt) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Itoa(int(i))), nil
}

// coerceBool extracts a bool from an interface{} that may be a bool, a
// "true"/"false" string, or a 0/1 number, for handlers decoding into
// map[string]interface{}. Returns (value, found).
func coerceBool(v interface{}) (bool, bool) {
	switch x := v.(type) {
	case nil:
		return false, false
	case bool:
		return x, true
	case string:
		switch x {
		case "true", "1", "yes":
			return true, true
		case "false", "0", "no":
			return false, true
		}
	case float64:
		return x != 0, true
	}
	return false, false
}

// flexIntSlice decodes []int but tolerates a single int, matching how
// `gh api -f key=val` sends each `-f` as a separate field.
type flexIntSlice []int

func (s *flexIntSlice) UnmarshalJSON(data []byte) error {
	if len(data) == 0 || string(data) == "null" {
		*s = nil
		return nil
	}
	if data[0] == '[' {
		var raw []json.RawMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			return err
		}
		out := make([]int, 0, len(raw))
		for _, r := range raw {
			var n flexInt
			if err := json.Unmarshal(r, &n); err != nil {
				return err
			}
			out = append(out, int(n))
		}
		*s = out
		return nil
	}
	var n flexInt
	if err := json.Unmarshal(data, &n); err != nil {
		return err
	}
	*s = []int{int(n)}
	return nil
}
