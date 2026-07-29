package bleephub

import "strings"

// safeLogText preserves diagnostic context while preventing a caller-controlled
// value from terminating the current plain-text log entry and forging another.
func safeLogText(value string) string {
	value = strings.ReplaceAll(value, "\r", `\r`)
	return strings.ReplaceAll(value, "\n", `\n`)
}

func safeLogError(err error) string {
	if err == nil {
		return ""
	}
	return safeLogText(err.Error())
}
