package bleephub

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type GHESMaintenanceState struct {
	Enabled                bool     `json:"enabled"`
	ScheduledTime          string   `json:"scheduled_time,omitempty"`
	IPExceptionList        []string `json:"ip_exception_list"`
	MaintenanceModeMessage string   `json:"maintenance_mode_message"`
}

type GHESManagementState struct {
	SSHKeys      []string                 `json:"ssh_keys"`
	Settings     map[string]interface{}   `json:"settings"`
	License      map[string]interface{}   `json:"license,omitempty"`
	Maintenance  GHESMaintenanceState     `json:"maintenance"`
	ConfigStatus string                   `json:"config_status"`
	ConfigRunID  string                   `json:"config_run_id,omitempty"`
	ConfigEvents []map[string]interface{} `json:"config_events,omitempty"`
	Initialized  bool                     `json:"initialized"`
}

func defaultGHESManagementState() *GHESManagementState {
	return &GHESManagementState{
		SSHKeys: []string{},
		Settings: map[string]interface{}{
			"private_mode": true, "public_pages": false, "subdomain_isolation": true,
			"signup_enabled": false, "github_hostname": "bleephub.local",
			"auth_mode": "default", "expire_sessions": false,
		},
		Maintenance:  GHESMaintenanceState{IPExceptionList: []string{}},
		ConfigStatus: "idle",
		ConfigEvents: []map[string]interface{}{},
	}
}

func (s *Server) registerGHESManageRoutes() {
	auth := s.requireGHESManagementAuth
	s.route("GET /manage/v1/access/ssh", auth(s.handleManageSSHKeys))
	s.route("POST /manage/v1/access/ssh", auth(s.handleManageSSHKeys))
	s.route("DELETE /manage/v1/access/ssh", auth(s.handleManageSSHKeys))
	s.route("GET /manage/v1/checks/system-requirements", auth(s.handleManageSystemRequirements))
	s.route("GET /manage/v1/cluster/status", auth(s.handleManageClusterStatus))
	s.route("GET /manage/v1/config/apply", auth(s.handleManageConfigApply))
	s.route("POST /manage/v1/config/apply", auth(s.handleManageConfigApply))
	s.route("GET /manage/v1/config/apply/events", auth(s.handleManageConfigApplyEvents))
	s.route("POST /manage/v1/config/init", auth(s.handleManageConfigInit))
	s.route("GET /manage/v1/config/license", auth(s.handleManageLicense))
	s.route("PUT /manage/v1/config/license", auth(s.handleManageLicense))
	s.route("GET /manage/v1/config/license/check", auth(s.handleManageLicenseCheck))
	s.route("GET /manage/v1/config/nodes", auth(s.handleManageNodes))
	s.route("GET /manage/v1/config/settings", auth(s.handleManageSettings))
	s.route("PUT /manage/v1/config/settings", auth(s.handleManageSettings))
	s.route("GET /manage/v1/maintenance", auth(s.handleManageMaintenance))
	s.route("POST /manage/v1/maintenance", auth(s.handleManageMaintenance))
	s.route("GET /manage/v1/replication/status", auth(s.handleManageReplicationStatus))
	s.route("GET /manage/v1/version", auth(s.handleManageVersion))
}

func managementPassword() string {
	if value := os.Getenv("BLEEPHUB_MANAGEMENT_PASSWORD"); value != "" {
		return value
	}
	return AdminToken()
}

func (s *Server) requireGHESManagementAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, password, ok := r.BasicAuth()
		expected := managementPassword()
		if !ok || username != "api_key" ||
			subtle.ConstantTimeCompare([]byte(password), []byte(expected)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="GitHub Enterprise Management Console"`)
			writeGHError(w, http.StatusUnauthorized, "Bad credentials")
			return
		}
		next(w, r)
	}
}

func (s *Server) ghesNodeJSON() map[string]interface{} {
	return map[string]interface{}{
		"hostname": "bleephub-primary", "uuid": "00000000-0000-0000-0000-000000000001",
		"cluster_roles": []string{"primary"},
	}
}

func managementFingerprint(key string) string {
	sum := sha256.Sum256([]byte(key))
	parts := make([]string, 16)
	for i := range parts {
		parts[i] = hex.EncodeToString(sum[i : i+1])
	}
	return strings.Join(parts, ":")
}

func (s *Server) handleManageSSHKeys(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.store.mu.RLock()
		keys := append([]string(nil), s.store.EnterpriseSettings.GHESManagement.SSHKeys...)
		s.store.mu.RUnlock()
		rows := make([]map[string]interface{}, len(keys))
		for i, key := range keys {
			rows[i] = map[string]interface{}{"key": key, "fingerprint": managementFingerprint(key)}
		}
		writeJSON(w, http.StatusOK, rows)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	req.Key = strings.TrimSpace(req.Key)
	if !strings.HasPrefix(req.Key, "ssh-") {
		writeGHError(w, http.StatusBadRequest, "Invalid SSH key")
		return
	}
	modified := false
	s.store.mu.Lock()
	keys := s.store.EnterpriseSettings.GHESManagement.SSHKeys
	index := -1
	for i, key := range keys {
		if key == req.Key {
			index = i
			break
		}
	}
	message := "SSH key added successfully"
	if r.Method == http.MethodPost && index < 0 {
		s.store.EnterpriseSettings.GHESManagement.SSHKeys = append(keys, req.Key)
		modified = true
	}
	if r.Method == http.MethodDelete {
		message = "SSH key removed successfully"
		if index >= 0 {
			s.store.EnterpriseSettings.GHESManagement.SSHKeys = append(keys[:index], keys[index+1:]...)
			modified = true
		}
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, []map[string]interface{}{{
		"hostname": "bleephub-primary", "uuid": "00000000-0000-0000-0000-000000000001",
		"message": message, "modified": modified,
	}})
}

func (s *Server) handleManageSystemRequirements(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]interface{}{{
		"hostname": "bleephub-primary", "status": "passed",
		"checks": []map[string]interface{}{
			{"name": "storage", "status": "passed"}, {"name": "services", "status": "passed"},
		},
	}})
}

func (s *Server) handleManageClusterStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "healthy", "nodes": []map[string]interface{}{s.ghesNodeJSON()},
	})
}

func (s *Server) completeConfigApplyLocked() {
	state := s.store.EnterpriseSettings.GHESManagement
	if state.ConfigStatus != "running" {
		return
	}
	now := s.currentTime()
	state.ConfigStatus = "success"
	state.ConfigEvents = append(state.ConfigEvents, map[string]interface{}{
		"timestamp": now.Format(time.RFC3339), "severity_text": "INFO",
		"body": "Configuration applied successfully", "event_name": "Enterprise::ConfigApply::Completed",
		"topology": "standalone", "hostname": "bleephub-primary", "config_run_id": state.ConfigRunID,
	})
	s.store.persistEnterpriseSettings()
}

func (s *Server) handleManageConfigApply(w http.ResponseWriter, r *http.Request) {
	s.store.mu.Lock()
	state := s.store.EnterpriseSettings.GHESManagement
	if r.Method == http.MethodPost {
		if state.ConfigStatus == "running" {
			s.store.mu.Unlock()
			writeGHError(w, http.StatusConflict, "Configuration apply is already running")
			return
		}
		state.ConfigStatus = "running"
		state.ConfigRunID = uuid.NewString()
		state.ConfigEvents = []map[string]interface{}{{
			"timestamp": s.currentTime().Format(time.RFC3339), "severity_text": "INFO",
			"body": "Starting configuration apply", "event_name": "Enterprise::ConfigApply::Started",
			"topology": "standalone", "hostname": "bleephub-primary", "config_run_id": state.ConfigRunID,
		}}
		s.store.persistEnterpriseSettings()
		runID := state.ConfigRunID
		s.store.mu.Unlock()
		writeJSON(w, http.StatusAccepted, map[string]interface{}{"run_id": runID, "status": "running"})
		return
	}
	s.completeConfigApplyLocked()
	out := map[string]interface{}{"run_id": state.ConfigRunID, "status": state.ConfigStatus}
	s.store.mu.Unlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleManageConfigApplyEvents(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.Lock()
	s.completeConfigApplyLocked()
	state := s.store.EnterpriseSettings.GHESManagement
	events := append([]map[string]interface{}(nil), state.ConfigEvents...)
	runID := state.ConfigRunID
	s.store.mu.Unlock()
	lastID := runID + ":0000000000000000"
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"nodes": []map[string]interface{}{{
			"node": "bleephub-primary", "last_request_id": lastID, "events": events,
		}},
	})
}

func (s *Server) handleManageConfigInit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(8 << 20); err != nil {
		writeGHError(w, http.StatusBadRequest, "Invalid multipart form")
		return
	}
	password := r.FormValue("password")
	license := r.FormValue("license")
	if password == "" || license == "" {
		writeGHError(w, http.StatusBadRequest, "license and password are required")
		return
	}
	s.store.mu.Lock()
	state := s.store.EnterpriseSettings.GHESManagement
	if state.Initialized {
		s.store.mu.Unlock()
		writeGHError(w, http.StatusConflict, "Instance is already initialized")
		return
	}
	state.Initialized = true
	state.License = map[string]interface{}{
		"seats": 0, "evaluation": false, "perpetual": true, "unlimited_seating": true,
		"uploaded_at": s.currentTime().Format(time.RFC3339),
	}
	s.store.persistEnterpriseSettings()
	s.store.mu.Unlock()
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleManageLicense(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var req map[string]interface{}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if len(req) == 0 {
			writeGHError(w, http.StatusBadRequest, "License is required")
			return
		}
		s.store.mu.Lock()
		s.store.EnterpriseSettings.GHESManagement.License = req
		s.store.persistEnterpriseSettings()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.store.mu.RLock()
	license := cloneInterfaceMap(s.store.EnterpriseSettings.GHESManagement.License)
	s.store.mu.RUnlock()
	if license == nil {
		license = map[string]interface{}{"seats": 0, "evaluation": false, "perpetual": true, "unlimited_seating": true}
	}
	writeJSON(w, http.StatusOK, license)
}

func (s *Server) handleManageLicenseCheck(w http.ResponseWriter, _ *http.Request) {
	s.store.mu.RLock()
	installed := s.store.EnterpriseSettings.GHESManagement.License != nil
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]interface{}{"valid": installed, "errors": []string{}})
}

func (s *Server) handleManageNodes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]interface{}{s.ghesNodeJSON()})
}

func cloneInterfaceMap(input map[string]interface{}) map[string]interface{} {
	if input == nil {
		return nil
	}
	out := make(map[string]interface{}, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func (s *Server) handleManageSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPut {
		var req map[string]interface{}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		delete(req, "password")
		delete(req, "root_password")
		s.store.mu.Lock()
		for key, value := range req {
			s.store.EnterpriseSettings.GHESManagement.Settings[key] = value
		}
		s.store.persistEnterpriseSettings()
		s.store.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.store.mu.RLock()
	settings := cloneInterfaceMap(s.store.EnterpriseSettings.GHESManagement.Settings)
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) maintenanceNodeJSON(state GHESMaintenanceState) map[string]interface{} {
	status := "off"
	if state.Enabled {
		status = "on"
	}
	if state.ScheduledTime != "" && !state.Enabled {
		status = "scheduled"
	}
	return map[string]interface{}{
		"hostname": "bleephub-primary", "uuid": "00000000-0000-0000-0000-000000000001",
		"status": status, "scheduled_time": state.ScheduledTime,
		"connection_services":   []map[string]interface{}{},
		"can_unset_maintenance": true, "ip_exception_list": append([]string(nil), state.IPExceptionList...),
		"maintenance_mode_message": state.MaintenanceModeMessage,
	}
}

func (s *Server) handleManageMaintenance(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var req struct {
			Enabled                *bool    `json:"enabled"`
			When                   string   `json:"when"`
			IPExceptionList        []string `json:"ip_exception_list"`
			MaintenanceModeMessage string   `json:"maintenance_mode_message"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if req.Enabled == nil {
			writeGHError(w, http.StatusBadRequest, "enabled is required")
			return
		}
		if req.When != "" && req.When != "now" {
			if _, err := time.Parse(time.RFC3339, req.When); err != nil {
				writeGHError(w, http.StatusBadRequest, "Invalid maintenance time")
				return
			}
		}
		s.store.mu.Lock()
		state := &s.store.EnterpriseSettings.GHESManagement.Maintenance
		state.Enabled = *req.Enabled && (req.When == "" || req.When == "now")
		state.ScheduledTime = ""
		if *req.Enabled && req.When != "" && req.When != "now" {
			state.ScheduledTime = req.When
		}
		state.IPExceptionList = append([]string(nil), req.IPExceptionList...)
		state.MaintenanceModeMessage = req.MaintenanceModeMessage
		copy := *state
		s.store.persistEnterpriseSettings()
		s.store.mu.Unlock()
		writeJSON(w, http.StatusOK, []map[string]interface{}{s.maintenanceNodeJSON(copy)})
		return
	}
	s.store.mu.RLock()
	state := s.store.EnterpriseSettings.GHESManagement.Maintenance
	s.store.mu.RUnlock()
	writeJSON(w, http.StatusOK, []map[string]interface{}{s.maintenanceNodeJSON(state)})
}

func (s *Server) handleManageReplicationStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, []map[string]interface{}{{
		"hostname": "bleephub-primary", "status": "healthy", "services": []map[string]interface{}{},
	}})
}

func (s *Server) handleManageVersion(w http.ResponseWriter, _ *http.Request) {
	buildID := "development"
	if value := os.Getenv("BLEEPHUB_COMMIT"); value != "" {
		buildID = value
	}
	writeJSON(w, http.StatusOK, []map[string]interface{}{{
		"hostname": "bleephub-primary",
		"version": map[string]interface{}{
			"version": "3.21.0", "platform": "bleephub", "build_id": buildID,
			"build_date": s.currentTime().Format("2006-01-02"),
		},
	}})
}

func sortedManagementKeys(state *GHESManagementState) []string {
	keys := append([]string(nil), state.SSHKeys...)
	sort.Strings(keys)
	return keys
}
