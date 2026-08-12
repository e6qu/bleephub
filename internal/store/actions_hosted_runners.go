package store

import (
	"sort"
	"strconv"
	"strings"
	"time"
)

func (st *Store) PersistHostedRunnerLocked(hr *HostedRunner) {
	if st.Persist != nil {
		st.Persist.MustPut("hosted_runners", strconv.Itoa(hr.ID), hr)
	}
}

func (st *Store) PersistHostedRunnerCustomImageLocked(img *HostedRunnerCustomImage) {
	if st.Persist != nil {
		st.Persist.MustPut("hosted_runner_custom_images", strconv.Itoa(img.ID), img)
	}
}

// CreateHostedRunnerCustomImage registers a custom hosted-runner image
// definition for an organization. On real GitHub, image definitions are
// produced by the image-generation pipeline of a hosted runner created
// with image_gen enabled; the REST v3 surface only lists, reads, and
// deletes them, so this is the store-level creation entry point.
func (st *Store) CreateHostedRunnerCustomImage(org, name, platform string) *HostedRunnerCustomImage {
	return st.createHostedRunnerCustomImage(RunnerScope{Org: org}, name, platform)
}

func (st *Store) CreateEnterpriseHostedRunnerCustomImage(enterprise, name, platform string) *HostedRunnerCustomImage {
	return st.createHostedRunnerCustomImage(RunnerScope{Enterprise: enterprise}, name, platform)
}

func (st *Store) createHostedRunnerCustomImage(target RunnerScope, name, platform string) *HostedRunnerCustomImage {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	img := &HostedRunnerCustomImage{
		ID:         st.NextHostedRunnerImageID,
		Org:        target.Org,
		Enterprise: target.Enterprise,
		Name:       name,
		Platform:   platform,
		State:      "Ready",
		Versions:   []*HostedRunnerCustomImageVersion{},
	}
	st.NextHostedRunnerImageID++
	if st.HostedRunnerCustomImages == nil {
		st.HostedRunnerCustomImages = map[int]*HostedRunnerCustomImage{}
	}
	st.HostedRunnerCustomImages[img.ID] = img
	st.PersistHostedRunnerCustomImageLocked(img)
	return img
}

// AddHostedRunnerCustomImageVersion appends a version to a custom image
// definition (the image-generation pipeline's output). Returns false
// when the image doesn't exist or the version is already present.
func (st *Store) AddHostedRunnerCustomImageVersion(imageID int, version string, sizeGB int) bool {
	st.Mu.Lock()
	defer st.Mu.Unlock()
	img := st.HostedRunnerCustomImages[imageID]
	if img == nil {
		return false
	}
	for _, v := range img.Versions {
		if v.Version == version {
			return false
		}
	}
	img.Versions = append(img.Versions, &HostedRunnerCustomImageVersion{
		Version:      version,
		State:        "Ready",
		SizeGB:       sizeGB,
		CreatedOn:    st.CurrentTime(),
		StateDetails: "None",
	})
	st.PersistHostedRunnerCustomImageLocked(img)
	return true
}

// hostedRunnersLocked returns the target's hosted runners sorted by id.
// Callers hold the store lock.
func (st *Store) HostedRunnersLocked(target RunnerScope) []*HostedRunner {
	out := make([]*HostedRunner, 0)
	for _, hr := range st.HostedRunners {
		if HostedRunnerMatchesTarget(hr, target) {
			out = append(out, hr)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// staticIPUsageLocked counts the static public IP addresses reserved by
// the target's hosted runners: each static-IP runner reserves one address
// per concurrent runner (maximum_runners). Callers hold the store lock.
func (st *Store) StaticIPUsageLocked(target RunnerScope) int {
	usage := 0
	for _, hr := range st.HostedRunnersLocked(target) {
		if hr.PublicIPEnabled {
			usage += hr.MaximumRunners
		}
	}
	return usage
}

// customImagesLocked returns the target's custom image definitions sorted by
// id. Callers hold the store lock.
func (st *Store) CustomImagesLocked(target RunnerScope) []*HostedRunnerCustomImage {
	out := make([]*HostedRunnerCustomImage, 0)
	for _, img := range st.HostedRunnerCustomImages {
		if CustomImageMatchesTarget(img, target) {
			out = append(out, img)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func CustomImageMatchesTarget(image *HostedRunnerCustomImage, target RunnerScope) bool {
	switch {
	case target.Org != "":
		return strings.EqualFold(image.Org, target.Org) && image.Enterprise == ""
	case target.Enterprise != "":
		return strings.EqualFold(image.Enterprise, target.Enterprise) && image.Org == ""
	default:
		return false
	}
}

// HostedRunnerCustomImageVersion is one version of a custom image.
type HostedRunnerCustomImageVersion struct {
	Version      string    `json:"version"`
	State        string    `json:"state"`
	SizeGB       int       `json:"size_gb"`
	CreatedOn    time.Time `json:"created_on"`
	StateDetails string    `json:"state_details"`
}

func HostedRunnerMatchesTarget(runner *HostedRunner, target RunnerScope) bool {
	switch {
	case target.Org != "":
		return strings.EqualFold(runner.Org, target.Org) && runner.Enterprise == ""
	case target.Enterprise != "":
		return strings.EqualFold(runner.Enterprise, target.Enterprise) && runner.Org == ""
	default:
		return false
	}
}
