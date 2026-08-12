package bleephub

// GitHub-hosted runners REST surface for organizations and enterprises:
// runner CRUD, the GitHub-owned /
// partner image catalogs, custom image definitions + versions, machine
// sizes, platforms, static-IP limits, and the runner-group hosted-runner
// listing. Hosted runners are org-scoped configuration resources backed
// by the store and persisted; the catalogs return GitHub's documented
// catalog values.

import (
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// hostedRunnerMachineSpec mirrors the actions-hosted-runner-machine-spec
// schema (SDK-shape names kept verbatim).
type hostedRunnerMachineSpec struct {
	ID        string
	CPUCores  int
	MemoryGB  int
	StorageGB int
}

// hostedRunnerCuratedImage mirrors the actions-hosted-runner-curated-image
// schema for the GitHub-owned and partner image catalogs.
type hostedRunnerCuratedImage struct {
	ID          string
	Platform    string
	SizeGB      int
	DisplayName string
	Source      string
}

// hostedRunnerMachineSpecs is GitHub's documented larger-runner machine
// size ladder ("About larger runners": vCPU / RAM / SSD per size).
var hostedRunnerMachineSpecs = []hostedRunnerMachineSpec{
	{ID: "4-core", CPUCores: 4, MemoryGB: 16, StorageGB: 150},
	{ID: "8-core", CPUCores: 8, MemoryGB: 32, StorageGB: 300},
	{ID: "16-core", CPUCores: 16, MemoryGB: 64, StorageGB: 600},
	{ID: "32-core", CPUCores: 32, MemoryGB: 128, StorageGB: 1200},
	{ID: "64-core", CPUCores: 64, MemoryGB: 208, StorageGB: 2040},
}

// hostedRunnerGitHubOwnedImages lists the GitHub-owned runner images
// (the actions/runner-images Ubuntu and Windows Server images), with
// the image sizes GitHub documents for its hosted-runner image catalog.
var hostedRunnerGitHubOwnedImages = []hostedRunnerCuratedImage{
	{ID: "ubuntu-24.04", Platform: "linux-x64", SizeGB: 86, DisplayName: "24.04", Source: "github"},
	{ID: "ubuntu-22.04", Platform: "linux-x64", SizeGB: 86, DisplayName: "22.04", Source: "github"},
	{ID: "windows-2025", Platform: "win-x64", SizeGB: 256, DisplayName: "2025", Source: "github"},
	{ID: "windows-2022", Platform: "win-x64", SizeGB: 256, DisplayName: "2022", Source: "github"},
}

// hostedRunnerPartnerImages is the partner image catalog as documented
// in GitHub's REST API description for the partner-images endpoint.
var hostedRunnerPartnerImages = []hostedRunnerCuratedImage{
	{ID: "ubuntu-20.04", Platform: "linux-x64", SizeGB: 86, DisplayName: "20.04", Source: "partner"},
}

// hostedRunnerPlatforms are the platform identifiers GitHub-hosted
// runners can be created for.
var hostedRunnerPlatforms = []string{"linux-x64", "linux-arm64", "win-x64", "win-arm64"}

// hostedRunnerStaticIPMaximum is GitHub's documented limit on static
// public IP addresses across an organization's hosted runners.
const hostedRunnerStaticIPMaximum = 50

func (s *Server) registerGHHostedRunnerRoutes() {
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleListHostedRunners)))
	s.route("POST /api/v3/orgs/{org}/actions/hosted-runners",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleCreateHostedRunner)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/images/github-owned",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleHostedRunnerGitHubOwnedImages)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/images/partner",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleHostedRunnerPartnerImages)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/images/custom",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleListHostedRunnerCustomImages)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/images/custom/{image_definition_id}",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetHostedRunnerCustomImage)))
	s.route("DELETE /api/v3/orgs/{org}/actions/hosted-runners/images/custom/{image_definition_id}",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleDeleteHostedRunnerCustomImage)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/images/custom/{image_definition_id}/versions",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleListHostedRunnerCustomImageVersions)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/images/custom/{image_definition_id}/versions/{version}",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetHostedRunnerCustomImageVersion)))
	s.route("DELETE /api/v3/orgs/{org}/actions/hosted-runners/images/custom/{image_definition_id}/versions/{version}",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleDeleteHostedRunnerCustomImageVersion)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/limits",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleHostedRunnerLimits)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/machine-sizes",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleHostedRunnerMachineSizes)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/platforms",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleHostedRunnerPlatforms)))
	s.route("GET /api/v3/orgs/{org}/actions/hosted-runners/{hosted_runner_id}",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleGetHostedRunner)))
	s.route("PATCH /api/v3/orgs/{org}/actions/hosted-runners/{hosted_runner_id}",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleUpdateHostedRunner)))
	s.route("DELETE /api/v3/orgs/{org}/actions/hosted-runners/{hosted_runner_id}",
		s.requirePerm(scopeOrgAdministration, permWrite, s.orgGated(s.handleDeleteHostedRunner)))
	s.route("GET /api/v3/orgs/{org}/actions/runner-groups/{runner_group_id}/hosted-runners",
		s.requirePerm(scopeOrgAdministration, permRead, s.orgGated(s.handleListRunnerGroupHostedRunners)))
}

// --- Store methods ---

// --- JSON renderers ---

func hostedRunnerMachineSpecJSON(spec hostedRunnerMachineSpec) map[string]any {
	return map[string]any{
		"id":         spec.ID,
		"cpu_cores":  spec.CPUCores,
		"memory_gb":  spec.MemoryGB,
		"storage_gb": spec.StorageGB,
	}
}

func hostedRunnerCuratedImageJSON(img hostedRunnerCuratedImage) map[string]any {
	return map[string]any{
		"id":           img.ID,
		"platform":     img.Platform,
		"size_gb":      img.SizeGB,
		"display_name": img.DisplayName,
		"source":       img.Source,
	}
}

func machineSpecByID(id string) (hostedRunnerMachineSpec, bool) {
	for _, spec := range hostedRunnerMachineSpecs {
		if spec.ID == id {
			return spec, true
		}
	}
	return hostedRunnerMachineSpec{}, false
}

func curatedImageByID(catalog []hostedRunnerCuratedImage, id string) (hostedRunnerCuratedImage, bool) {
	for _, img := range catalog {
		if img.ID == id {
			return img, true
		}
	}
	return hostedRunnerCuratedImage{}, false
}

// hostedRunnerJSON renders the actions-hosted-runner shape. status
// overrides the runner's steady state (e.g. "Deleting" on the DELETE
// response); pass "" for the stored state.
func hostedRunnerJSON(hr *HostedRunner, status string) map[string]any {
	if status == "" {
		status = "Ready"
	}
	imageDetails := map[string]any{
		"id":           hr.ImageID,
		"size_gb":      hr.ImageSizeGB,
		"display_name": hr.ImageDisplayName,
		"source":       hr.ImageSource,
	}
	if hr.ImageVersion != "" {
		imageDetails["version"] = hr.ImageVersion
	}
	spec, _ := machineSpecByID(hr.MachineSizeID)
	var lastActive any
	if hr.LastActiveOn != nil {
		lastActive = hr.LastActiveOn.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id":                   hr.ID,
		"name":                 hr.Name,
		"runner_group_id":      hr.RunnerGroupID,
		"image_details":        imageDetails,
		"machine_size_details": hostedRunnerMachineSpecJSON(spec),
		"status":               status,
		"platform":             hr.Platform,
		"maximum_runners":      hr.MaximumRunners,
		"public_ip_enabled":    hr.PublicIPEnabled,
		"public_ips":           []any{},
		"last_active_on":       lastActive,
		"image_gen":            hr.ImageGen,
	}
}

func hostedRunnerCustomImageJSON(img *HostedRunnerCustomImage) map[string]any {
	totalSize := 0
	latest := ""
	var latestCreated time.Time
	// Versions append in generation order, so on equal timestamps the
	// later entry is the newer version.
	for _, v := range img.Versions {
		totalSize += v.SizeGB
		if latest == "" || !v.CreatedOn.Before(latestCreated) {
			latest = v.Version
			latestCreated = v.CreatedOn
		}
	}
	return map[string]any{
		"id":                  img.ID,
		"platform":            img.Platform,
		"name":                img.Name,
		"source":              "custom",
		"versions_count":      len(img.Versions),
		"total_versions_size": totalSize,
		"latest_version":      latest,
		"state":               img.State,
	}
}

func hostedRunnerCustomImageVersionJSON(v *HostedRunnerCustomImageVersion) map[string]any {
	return map[string]any{
		"version":       v.Version,
		"state":         v.State,
		"size_gb":       v.SizeGB,
		"created_on":    v.CreatedOn.UTC().Format(time.RFC3339),
		"state_details": v.StateDetails,
	}
}

// --- Runner handlers ---

func (s *Server) handleListHostedRunners(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	s.store.Mu.RLock()
	all := s.store.HostedRunnersLocked(target)
	s.store.Mu.RUnlock()
	page := paginateAndLink(w, r, all)
	runners := make([]map[string]any, 0, len(page))
	for _, hr := range page {
		runners = append(runners, hostedRunnerJSON(hr, ""))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(all),
		"runners":     runners,
	})
}

// resolveHostedRunnerImage resolves an image reference (id + source +
// optional version) against the catalogs / the org's custom image
// definitions. Returns a filled-in template or an error message.
func (s *Server) resolveHostedRunnerImage(target runnerScope, id, source, version string) (*HostedRunner, string) {
	out := &HostedRunner{ImageID: id, ImageSource: source, ImageVersion: version}
	switch source {
	case "github":
		img, ok := curatedImageByID(hostedRunnerGitHubOwnedImages, id)
		if !ok {
			return nil, fmt.Sprintf("image %q not found in the GitHub-owned image catalog", id)
		}
		out.ImageSizeGB, out.ImageDisplayName, out.Platform = img.SizeGB, img.DisplayName, img.Platform
	case "partner":
		img, ok := curatedImageByID(hostedRunnerPartnerImages, id)
		if !ok {
			return nil, fmt.Sprintf("image %q not found in the partner image catalog", id)
		}
		out.ImageSizeGB, out.ImageDisplayName, out.Platform = img.SizeGB, img.DisplayName, img.Platform
	case "custom":
		imgID, err := strconv.Atoi(id)
		if err != nil {
			return nil, fmt.Sprintf("custom image id %q is not a valid image definition id", id)
		}
		s.store.Mu.RLock()
		img := s.store.HostedRunnerCustomImages[imgID]
		var ver *HostedRunnerCustomImageVersion
		if img != nil && customImageMatchesTarget(img, target) {
			for _, v := range img.Versions {
				if version == "" || version == "latest" || v.Version == version {
					// Versions append in generation order, so on equal
					// timestamps the later entry is the newer version.
					if ver == nil || !v.CreatedOn.Before(ver.CreatedOn) {
						ver = v
					}
				}
			}
		}
		s.store.Mu.RUnlock()
		if img == nil || !customImageMatchesTarget(img, target) {
			return nil, fmt.Sprintf("custom image definition %q not found", id)
		}
		if ver == nil {
			return nil, fmt.Sprintf("custom image definition %q has no version %q", id, version)
		}
		out.ImageSizeGB, out.ImageDisplayName, out.Platform = ver.SizeGB, img.Name, img.Platform
		out.ImageVersion = ver.Version
	default:
		return nil, fmt.Sprintf("image source %q must be github, partner, or custom", source)
	}
	return out, ""
}

func (s *Server) handleCreateHostedRunner(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Name  string `json:"name"`
		Image struct {
			ID      string `json:"id"`
			Source  string `json:"source"`
			Version string `json:"version"`
		} `json:"image"`
		Size           string `json:"size"`
		RunnerGroupID  *int   `json:"runner_group_id"`
		MaximumRunners int    `json:"maximum_runners"`
		EnableStaticIP bool   `json:"enable_static_ip"`
		ImageGen       bool   `json:"image_gen"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Name == "" || len(req.Name) > 64 {
		writeGHValidationError(w, "HostedRunner", "name", "invalid")
		return
	}
	if req.Image.ID == "" {
		writeGHValidationError(w, "HostedRunner", "image", "missing_field")
		return
	}
	if req.RunnerGroupID == nil {
		writeGHValidationError(w, "HostedRunner", "runner_group_id", "missing_field")
		return
	}
	if _, ok := machineSpecByID(req.Size); !ok {
		writeGHValidationError(w, "HostedRunner", "size", "invalid")
		return
	}
	source := req.Image.Source
	if source == "" {
		source = "github"
	}
	resolved, errMsg := s.resolveHostedRunnerImage(target, req.Image.ID, source, req.Image.Version)
	if errMsg != "" {
		writeGHError(w, http.StatusUnprocessableEntity, errMsg)
		return
	}
	maxRunners := req.MaximumRunners
	if maxRunners <= 0 {
		maxRunners = 10
	}

	s.store.Mu.Lock()
	s.ensureDefaultRunnerGroupLocked(target)
	group := s.store.RunnerGroups[*req.RunnerGroupID]
	if group == nil || !runnerGroupMatchesTarget(group, target) {
		s.store.Mu.Unlock()
		writeGHValidationError(w, "HostedRunner", "runner_group_id", "invalid")
		return
	}
	if req.EnableStaticIP && s.store.StaticIPUsageLocked(target)+maxRunners > hostedRunnerStaticIPMaximum {
		s.store.Mu.Unlock()
		writeGHError(w, http.StatusUnprocessableEntity,
			fmt.Sprintf("enabling static IPs would exceed the target's %d static public IP address limit", hostedRunnerStaticIPMaximum))
		return
	}
	hr := &HostedRunner{
		ID:               s.store.NextHostedRunnerID,
		Org:              target.Org,
		Enterprise:       target.Enterprise,
		Name:             req.Name,
		RunnerGroupID:    *req.RunnerGroupID,
		ImageID:          resolved.ImageID,
		ImageSource:      resolved.ImageSource,
		ImageVersion:     resolved.ImageVersion,
		ImageSizeGB:      resolved.ImageSizeGB,
		ImageDisplayName: resolved.ImageDisplayName,
		Platform:         resolved.Platform,
		MachineSizeID:    req.Size,
		MaximumRunners:   maxRunners,
		PublicIPEnabled:  req.EnableStaticIP,
		ImageGen:         req.ImageGen,
		CreatedAt:        s.store.CurrentTime(),
	}
	s.store.NextHostedRunnerID++
	s.store.HostedRunners[hr.ID] = hr
	s.store.PersistHostedRunnerLocked(hr)
	s.store.Mu.Unlock()

	writeJSON(w, http.StatusCreated, hostedRunnerJSON(hr, ""))
}

// lookupHostedRunner resolves the path's hosted_runner_id within the owning
// organization or enterprise; nil + handled response when missing.
func (s *Server) lookupHostedRunner(w http.ResponseWriter, r *http.Request) *HostedRunner {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return nil
	}
	id, err := strconv.Atoi(r.PathValue("hosted_runner_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	s.store.Mu.RLock()
	hr := s.store.HostedRunners[id]
	s.store.Mu.RUnlock()
	if hr == nil || !hostedRunnerMatchesTarget(hr, target) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return hr
}

func (s *Server) handleGetHostedRunner(w http.ResponseWriter, r *http.Request) {
	hr := s.lookupHostedRunner(w, r)
	if hr == nil {
		return
	}
	writeJSON(w, http.StatusOK, hostedRunnerJSON(hr, ""))
}

func (s *Server) handleUpdateHostedRunner(w http.ResponseWriter, r *http.Request) {
	hr := s.lookupHostedRunner(w, r)
	if hr == nil {
		return
	}
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	var req struct {
		Name           *string `json:"name"`
		Size           *string `json:"size"`
		RunnerGroupID  *int    `json:"runner_group_id"`
		MaximumRunners *int    `json:"maximum_runners"`
		EnableStaticIP *bool   `json:"enable_static_ip"`
		ImageGen       *bool   `json:"image_gen"`
		ImageID        *string `json:"image_id"`
		ImageSource    *string `json:"image_source"`
		ImageVersion   *string `json:"image_version"`
	}
	if !decodeJSONBody(w, r, &req) {
		return
	}
	if req.Size != nil {
		if _, ok := machineSpecByID(*req.Size); !ok {
			writeGHValidationError(w, "HostedRunner", "size", "invalid")
			return
		}
	}
	var resolvedImage *HostedRunner
	if req.ImageID != nil {
		source := hr.ImageSource
		if req.ImageSource != nil {
			source = *req.ImageSource
		}
		version := ""
		if req.ImageVersion != nil {
			version = *req.ImageVersion
		}
		resolved, errMsg := s.resolveHostedRunnerImage(target, *req.ImageID, source, version)
		if errMsg != "" {
			writeGHError(w, http.StatusUnprocessableEntity, errMsg)
			return
		}
		resolvedImage = resolved
	}

	s.store.Mu.Lock()
	if req.RunnerGroupID != nil {
		s.ensureDefaultRunnerGroupLocked(target)
		group := s.store.RunnerGroups[*req.RunnerGroupID]
		if group == nil || !runnerGroupMatchesTarget(group, target) {
			s.store.Mu.Unlock()
			writeGHValidationError(w, "HostedRunner", "runner_group_id", "invalid")
			return
		}
		hr.RunnerGroupID = *req.RunnerGroupID
	}
	if req.EnableStaticIP != nil && *req.EnableStaticIP && !hr.PublicIPEnabled {
		max := hr.MaximumRunners
		if req.MaximumRunners != nil && *req.MaximumRunners > 0 {
			max = *req.MaximumRunners
		}
		if s.store.StaticIPUsageLocked(target)+max > hostedRunnerStaticIPMaximum {
			s.store.Mu.Unlock()
			writeGHError(w, http.StatusUnprocessableEntity,
				fmt.Sprintf("enabling static IPs would exceed the target's %d static public IP address limit", hostedRunnerStaticIPMaximum))
			return
		}
	}
	if req.Name != nil && *req.Name != "" {
		hr.Name = *req.Name
	}
	if req.Size != nil {
		hr.MachineSizeID = *req.Size
	}
	if req.MaximumRunners != nil && *req.MaximumRunners > 0 {
		hr.MaximumRunners = *req.MaximumRunners
	}
	if req.EnableStaticIP != nil {
		hr.PublicIPEnabled = *req.EnableStaticIP
	}
	if req.ImageGen != nil {
		hr.ImageGen = *req.ImageGen
	}
	if resolvedImage != nil {
		hr.ImageID = resolvedImage.ImageID
		hr.ImageSource = resolvedImage.ImageSource
		hr.ImageVersion = resolvedImage.ImageVersion
		hr.ImageSizeGB = resolvedImage.ImageSizeGB
		hr.ImageDisplayName = resolvedImage.ImageDisplayName
		hr.Platform = resolvedImage.Platform
	}
	s.store.PersistHostedRunnerLocked(hr)
	s.store.Mu.Unlock()

	writeJSON(w, http.StatusOK, hostedRunnerJSON(hr, ""))
}

func (s *Server) handleDeleteHostedRunner(w http.ResponseWriter, r *http.Request) {
	hr := s.lookupHostedRunner(w, r)
	if hr == nil {
		return
	}
	s.store.Mu.Lock()
	delete(s.store.HostedRunners, hr.ID)
	if s.store.Persist != nil {
		s.store.Persist.MustDelete("hosted_runners", strconv.Itoa(hr.ID))
	}
	s.store.Mu.Unlock()
	// Real GitHub deletes asynchronously and answers 202 with the runner
	// in its Deleting state.
	writeJSON(w, http.StatusAccepted, hostedRunnerJSON(hr, "Deleting"))
}

func (s *Server) handleListRunnerGroupHostedRunners(w http.ResponseWriter, r *http.Request) {
	g := s.lookupRunnerGroup(w, r)
	if g == nil {
		return
	}
	s.store.Mu.RLock()
	members := make([]*HostedRunner, 0)
	for _, hr := range s.store.HostedRunnersLocked(g.Scope) {
		if hr.RunnerGroupID == g.ID {
			members = append(members, hr)
		}
	}
	s.store.Mu.RUnlock()
	page := paginateAndLink(w, r, members)
	runners := make([]map[string]any, 0, len(page))
	for _, hr := range page {
		runners = append(runners, hostedRunnerJSON(hr, ""))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(members),
		"runners":     runners,
	})
}

// --- Catalog handlers ---

func (s *Server) handleHostedRunnerGitHubOwnedImages(w http.ResponseWriter, r *http.Request) {
	images := make([]map[string]any, 0, len(hostedRunnerGitHubOwnedImages))
	for _, img := range hostedRunnerGitHubOwnedImages {
		images = append(images, hostedRunnerCuratedImageJSON(img))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(images),
		"images":      images,
	})
}

func (s *Server) handleHostedRunnerPartnerImages(w http.ResponseWriter, r *http.Request) {
	images := make([]map[string]any, 0, len(hostedRunnerPartnerImages))
	for _, img := range hostedRunnerPartnerImages {
		images = append(images, hostedRunnerCuratedImageJSON(img))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(images),
		"images":      images,
	})
}

func (s *Server) handleHostedRunnerMachineSizes(w http.ResponseWriter, r *http.Request) {
	specs := make([]map[string]any, 0, len(hostedRunnerMachineSpecs))
	for _, spec := range hostedRunnerMachineSpecs {
		specs = append(specs, hostedRunnerMachineSpecJSON(spec))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":   len(specs),
		"machine_specs": specs,
	})
}

func (s *Server) handleHostedRunnerPlatforms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(hostedRunnerPlatforms),
		"platforms":   hostedRunnerPlatforms,
	})
}

func (s *Server) handleHostedRunnerLimits(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	s.store.Mu.RLock()
	usage := s.store.StaticIPUsageLocked(target)
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"public_ips": map[string]any{
			"maximum":       hostedRunnerStaticIPMaximum,
			"current_usage": usage,
		},
	})
}

// --- Custom image handlers ---

func (s *Server) handleListHostedRunnerCustomImages(w http.ResponseWriter, r *http.Request) {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return
	}
	s.store.Mu.RLock()
	all := s.store.CustomImagesLocked(target)
	images := make([]map[string]any, 0, len(all))
	for _, img := range all {
		images = append(images, hostedRunnerCustomImageJSON(img))
	}
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count": len(images),
		"images":      images,
	})
}

// lookupCustomImage resolves the path's image_definition_id within the owning
// organization or enterprise; nil + handled response when missing.
func (s *Server) lookupCustomImage(w http.ResponseWriter, r *http.Request) *HostedRunnerCustomImage {
	target, ok := s.runnerGroupTarget(w, r)
	if !ok {
		return nil
	}
	id, err := strconv.Atoi(r.PathValue("image_definition_id"))
	if err != nil {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	s.store.Mu.RLock()
	img := s.store.HostedRunnerCustomImages[id]
	s.store.Mu.RUnlock()
	if img == nil || !customImageMatchesTarget(img, target) {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return nil
	}
	return img
}

func (s *Server) handleGetHostedRunnerCustomImage(w http.ResponseWriter, r *http.Request) {
	img := s.lookupCustomImage(w, r)
	if img == nil {
		return
	}
	s.store.Mu.RLock()
	out := hostedRunnerCustomImageJSON(img)
	s.store.Mu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleDeleteHostedRunnerCustomImage(w http.ResponseWriter, r *http.Request) {
	img := s.lookupCustomImage(w, r)
	if img == nil {
		return
	}
	s.store.Mu.Lock()
	delete(s.store.HostedRunnerCustomImages, img.ID)
	if s.store.Persist != nil {
		s.store.Persist.MustDelete("hosted_runner_custom_images", strconv.Itoa(img.ID))
	}
	s.store.Mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListHostedRunnerCustomImageVersions(w http.ResponseWriter, r *http.Request) {
	img := s.lookupCustomImage(w, r)
	if img == nil {
		return
	}
	s.store.Mu.RLock()
	versions := append([]*HostedRunnerCustomImageVersion(nil), img.Versions...)
	s.store.Mu.RUnlock()
	// Newest first, matching real GitHub's version listing. Versions
	// append in generation order, so reversing the copy orders equal
	// timestamps correctly too.
	for i, j := 0, len(versions)-1; i < j; i, j = i+1, j-1 {
		versions[i], versions[j] = versions[j], versions[i]
	}
	sort.SliceStable(versions, func(i, j int) bool { return versions[i].CreatedOn.After(versions[j].CreatedOn) })
	out := make([]map[string]any, 0, len(versions))
	for _, v := range versions {
		out = append(out, hostedRunnerCustomImageVersionJSON(v))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"total_count":    len(out),
		"image_versions": out,
	})
}

func (s *Server) handleGetHostedRunnerCustomImageVersion(w http.ResponseWriter, r *http.Request) {
	img := s.lookupCustomImage(w, r)
	if img == nil {
		return
	}
	version := r.PathValue("version")
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	for _, v := range img.Versions {
		if v.Version == version {
			writeJSON(w, http.StatusOK, hostedRunnerCustomImageVersionJSON(v))
			return
		}
	}
	writeGHError(w, http.StatusNotFound, "Not Found")
}

func (s *Server) handleDeleteHostedRunnerCustomImageVersion(w http.ResponseWriter, r *http.Request) {
	img := s.lookupCustomImage(w, r)
	if img == nil {
		return
	}
	version := r.PathValue("version")
	s.store.Mu.Lock()
	found := false
	kept := img.Versions[:0:0]
	for _, v := range img.Versions {
		if v.Version == version {
			found = true
			continue
		}
		kept = append(kept, v)
	}
	img.Versions = kept
	if found {
		s.store.PersistHostedRunnerCustomImageLocked(img)
	}
	s.store.Mu.Unlock()
	if !found {
		writeGHError(w, http.StatusNotFound, "Not Found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
