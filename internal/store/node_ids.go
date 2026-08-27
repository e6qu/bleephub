package store

import (
	"strconv"
	"strings"
)

// DecodeNodeDBID extracts the trailing database id from a node ID shaped
// "<prefix><digits>" (e.g. "R_kgDO00000123"). It returns false when the id
// lacks the prefix or doesn't end in digits, so a foreign-shaped id (e.g.
// "U_bleephub_<login>") falls through to a scan rather than mis-resolving.
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
