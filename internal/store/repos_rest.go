package store

import "time"

func LicenseJSON(repo *Repo) interface{} {
	if repo.LicenseKey == "" {
		return nil
	}
	return map[string]interface{}{
		"key":     repo.LicenseKey,
		"name":    repo.LicenseName,
		"spdx_id": repo.LicenseSPDX,
		"url":     nil,
		"node_id": licenseNodeID(repo.LicenseKey),
	}
}

func NilOrString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func NullableTimestamp(t time.Time) interface{} {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339)
}

// licenseNodeID returns the node ID for a license key, or a deterministic
// fallback for keys outside the catalog.
func licenseNodeID(key string) string {
	if tmpl, ok := LicenseTemplates[key]; ok {
		return tmpl.NodeID
	}
	return "MDc6TGljZW5zZTA="
}
