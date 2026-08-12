package store

import (
	"strconv"
	"strings"
)

// DecodeNodeDBID extracts the trailing database id from a GraphQL node ID of the
// form "<prefix><digits>" (e.g. "R_kgDO00000123", prefix "R_kgDO"). It returns
// false when the id lacks that prefix or does not end in digits — so a legacy or
// foreign-shaped id (e.g. the "U_bleephub_<login>" identifiers) falls through to
// a scan rather than being mis-resolved. Callers pair the O(1) map lookup with a
// node-id equality check, keeping behavior identical to the old full scan
// (GQL-024).
func DecodeNodeDBID(nodeID, prefix string) (int, bool) {
	rest, ok := strings.CutPrefix(nodeID, prefix)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(rest)
	if err != nil {
		return 0, false
	}
	return id, true
}
