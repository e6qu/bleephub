package store

import (
	"bytes"
	"sort"
	"strings"
)

// registerJobLogMasksLocked records every secret variable delivered in a job
// message. This is a server-side backstop for the official runner's own mask
// handling: uploaded logs remain safe even when a runner is buggy or hostile.
// The caller must hold Store.Mu.
func (st *Store) RegisterJobLogMasksLocked(planID string, message map[string]interface{}) {
	variables, _ := message["variables"].(map[string]interface{})
	for _, raw := range variables {
		variable, _ := raw.(map[string]interface{})
		secret, _ := variable["isSecret"].(bool)
		value, _ := variable["value"].(string)
		if secret {
			st.addLogMaskLocked(planID, value)
		}
	}
}

func (st *Store) addLogMaskLocked(planID, value string) {
	if planID == "" || value == "" {
		return
	}
	for _, existing := range st.LogMasks[planID] {
		if existing == value {
			return
		}
	}
	st.LogMasks[planID] = append(st.LogMasks[planID], value)
	sort.SliceStable(st.LogMasks[planID], func(i, j int) bool {
		return len(st.LogMasks[planID][i]) > len(st.LogMasks[planID][j])
	})
}

func (st *Store) discoverLogMasksLocked(planID string, data []byte) {
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := workflowCommandMask(line); ok {
			st.addLogMaskLocked(planID, value)
		}
	}
}

func (st *Store) RedactLogBytesLocked(planID string, data []byte) []byte {
	st.discoverLogMasksLocked(planID, data)
	lines := strings.Split(string(data), "\n")
	for index, line := range lines {
		if _, ok := workflowCommandMask(line); ok {
			lines[index] = redactWorkflowCommandLine(line)
		}
	}
	out := []byte(strings.Join(lines, "\n"))
	for _, mask := range st.LogMasks[planID] {
		out = bytes.ReplaceAll(out, []byte(mask), []byte(redactedLogValue))
	}
	return out
}

func (st *Store) RedactLogLinesLocked(planID string, lines []string) []string {
	for _, line := range lines {
		if value, ok := workflowCommandMask(line); ok {
			st.addLogMaskLocked(planID, value)
		}
	}
	out := make([]string, len(lines))
	for index, line := range lines {
		if _, ok := workflowCommandMask(line); ok {
			line = redactWorkflowCommandLine(line)
		}
		for _, mask := range st.LogMasks[planID] {
			line = strings.ReplaceAll(line, mask, redactedLogValue)
		}
		out[index] = line
	}
	return out
}

const redactedLogValue = "***"

func redactWorkflowCommandLine(line string) string {
	suffix := ""
	if strings.HasSuffix(line, "\r") {
		line = strings.TrimSuffix(line, "\r")
		suffix = "\r"
	}
	for _, marker := range []string{"::add-mask::", "##[add-mask]"} {
		if index := strings.Index(line, marker); index >= 0 {
			return line[:index+len(marker)] + redactedLogValue + suffix
		}
	}
	return line + suffix
}

func workflowCommandMask(line string) (string, bool) {
	line = strings.TrimSuffix(line, "\r")
	for _, marker := range []string{"::add-mask::", "##[add-mask]"} {
		if index := strings.Index(line, marker); index >= 0 {
			value := line[index+len(marker):]
			value = strings.ReplaceAll(value, "%0D", "\r")
			value = strings.ReplaceAll(value, "%0A", "\n")
			value = strings.ReplaceAll(value, "%25", "%")
			return value, value != ""
		}
	}
	return "", false
}
