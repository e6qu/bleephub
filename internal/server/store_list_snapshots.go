package bleephub

// STORE-021: clone + snapshot helpers for the remaining List* element types
// that carry mutable reference fields (slices, maps, pointers) but had no
// existing clone helper. Each List* method returning one of these types wraps
// its result in the matching snapshot* helper, under the store read lock, so a
// caller iterating and rendering the list cannot race an in-place element
// mutation nor leak a write back into the stored row. Container-bearing
// reference fields get fresh backing arrays/maps; write-once nested pointees
// (e.g. a CheckRun's Output, a manifest) are shared by pointer.

func cloneCheckRun(x *CheckRun) *CheckRun {
	if x == nil {
		return nil
	}
	c := *x
	c.CompletedAt = cloneTimePtr(x.CompletedAt)
	if x.Output != nil {
		o := *x.Output
		c.Output = &o
	}
	return &c
}

func snapshotCheckRuns(in []*CheckRun) []*CheckRun {
	if in == nil {
		return nil
	}
	out := make([]*CheckRun, len(in))
	for i, x := range in {
		out[i] = cloneCheckRun(x)
	}
	return out
}

func cloneDiscussionComment(x *DiscussionComment) *DiscussionComment {
	if x == nil {
		return nil
	}
	c := *x
	c.LastEditedAt = cloneTimePtr(x.LastEditedAt)
	return &c
}

func snapshotDiscussionComments(in []*DiscussionComment) []*DiscussionComment {
	if in == nil {
		return nil
	}
	out := make([]*DiscussionComment, len(in))
	for i, x := range in {
		out[i] = cloneDiscussionComment(x)
	}
	return out
}

func cloneCustomProperty(x *CustomProperty) *CustomProperty {
	if x == nil {
		return nil
	}
	c := *x
	if x.Description != nil {
		v := *x.Description
		c.Description = &v
	}
	c.AllowedValues = append([]string(nil), x.AllowedValues...)
	return &c
}

func snapshotCustomProperties(in []*CustomProperty) []*CustomProperty {
	if in == nil {
		return nil
	}
	out := make([]*CustomProperty, len(in))
	for i, x := range in {
		out[i] = cloneCustomProperty(x)
	}
	return out
}

func cloneIssueField(x *IssueField) *IssueField {
	if x == nil {
		return nil
	}
	c := *x
	if x.Description != nil {
		v := *x.Description
		c.Description = &v
	}
	if x.Options != nil {
		c.Options = append([]*IssueFieldOption(nil), x.Options...)
	}
	return &c
}

func snapshotIssueFields(in []*IssueField) []*IssueField {
	if in == nil {
		return nil
	}
	out := make([]*IssueField, len(in))
	for i, x := range in {
		out[i] = cloneIssueField(x)
	}
	return out
}

func cloneIssueType(x *IssueType) *IssueType {
	if x == nil {
		return nil
	}
	c := *x
	if x.Description != nil {
		v := *x.Description
		c.Description = &v
	}
	if x.Color != nil {
		v := *x.Color
		c.Color = &v
	}
	return &c
}

func snapshotIssueTypes(in []*IssueType) []*IssueType {
	if in == nil {
		return nil
	}
	out := make([]*IssueType, len(in))
	for i, x := range in {
		out[i] = cloneIssueType(x)
	}
	return out
}

func cloneNetworkConfiguration(x *NetworkConfiguration) *NetworkConfiguration {
	if x == nil {
		return nil
	}
	c := *x
	c.NetworkSettingsIDs = append([]string(nil), x.NetworkSettingsIDs...)
	c.FailoverNetworkSettingsIDs = append([]string(nil), x.FailoverNetworkSettingsIDs...)
	return &c
}

func snapshotNetworkConfigurations(in []*NetworkConfiguration) []*NetworkConfiguration {
	if in == nil {
		return nil
	}
	out := make([]*NetworkConfiguration, len(in))
	for i, x := range in {
		out[i] = cloneNetworkConfiguration(x)
	}
	return out
}

func clonePrivateRegistryConfiguration(x *PrivateRegistryConfiguration) *PrivateRegistryConfiguration {
	if x == nil {
		return nil
	}
	c := *x
	if x.Username != nil {
		v := *x.Username
		c.Username = &v
	}
	c.SelectedRepositoryIDs = append([]int(nil), x.SelectedRepositoryIDs...)
	return &c
}

func snapshotPrivateRegistryConfigurations(in []*PrivateRegistryConfiguration) []*PrivateRegistryConfiguration {
	if in == nil {
		return nil
	}
	out := make([]*PrivateRegistryConfiguration, len(in))
	for i, x := range in {
		out[i] = clonePrivateRegistryConfiguration(x)
	}
	return out
}

func cloneCampaign(x *Campaign) *Campaign {
	if x == nil {
		return nil
	}
	c := *x
	c.ManagerLogins = append([]string(nil), x.ManagerLogins...)
	c.TeamManagerSlugs = append([]string(nil), x.TeamManagerSlugs...)
	if x.ContactLink != nil {
		v := *x.ContactLink
		c.ContactLink = &v
	}
	c.ClosedAt = cloneTimePtr(x.ClosedAt)
	if x.CodeScanningAlerts != nil {
		m := make(map[int][]int, len(x.CodeScanningAlerts))
		for k, v := range x.CodeScanningAlerts {
			m[k] = append([]int(nil), v...)
		}
		c.CodeScanningAlerts = m
	}
	return &c
}

func snapshotCampaigns(in []*Campaign) []*Campaign {
	if in == nil {
		return nil
	}
	out := make([]*Campaign, len(in))
	for i, x := range in {
		out[i] = cloneCampaign(x)
	}
	return out
}

func cloneDependencySnapshot(x *DependencySnapshot) *DependencySnapshot {
	if x == nil {
		return nil
	}
	c := *x
	if x.Manifests != nil {
		m := make(map[string]*SnapshotManifest, len(x.Manifests))
		for k, v := range x.Manifests {
			m[k] = v
		}
		c.Manifests = m
	}
	return &c
}

func snapshotDependencySnapshots(in []*DependencySnapshot) []*DependencySnapshot {
	if in == nil {
		return nil
	}
	out := make([]*DependencySnapshot, len(in))
	for i, x := range in {
		out[i] = cloneDependencySnapshot(x)
	}
	return out
}

func cloneArtifactDeploymentRecord(x *ArtifactDeploymentRecord) *ArtifactDeploymentRecord {
	if x == nil {
		return nil
	}
	c := *x
	if x.Tags != nil {
		m := make(map[string]string, len(x.Tags))
		for k, v := range x.Tags {
			m[k] = v
		}
		c.Tags = m
	}
	c.RuntimeRisks = append([]string(nil), x.RuntimeRisks...)
	return &c
}

func snapshotArtifactDeploymentRecords(in []*ArtifactDeploymentRecord) []*ArtifactDeploymentRecord {
	if in == nil {
		return nil
	}
	out := make([]*ArtifactDeploymentRecord, len(in))
	for i, x := range in {
		out[i] = cloneArtifactDeploymentRecord(x)
	}
	return out
}

func cloneCodespaceSecret(x *CodespaceSecret) *CodespaceSecret {
	if x == nil {
		return nil
	}
	c := *x
	c.SelectedRepoIDs = append([]int(nil), x.SelectedRepoIDs...)
	return &c
}

func snapshotCodespaceSecrets(in []*CodespaceSecret) []*CodespaceSecret {
	if in == nil {
		return nil
	}
	out := make([]*CodespaceSecret, len(in))
	for i, x := range in {
		out[i] = cloneCodespaceSecret(x)
	}
	return out
}

func cloneRepoTrafficBucket(x *RepoTrafficBucket) *RepoTrafficBucket {
	if x == nil {
		return nil
	}
	c := *x
	if x.Actors != nil {
		m := make(map[string]bool, len(x.Actors))
		for k, v := range x.Actors {
			m[k] = v
		}
		c.Actors = m
	}
	return &c
}

func snapshotRepoTrafficBuckets(in []*RepoTrafficBucket) []*RepoTrafficBucket {
	if in == nil {
		return nil
	}
	out := make([]*RepoTrafficBucket, len(in))
	for i, x := range in {
		out[i] = cloneRepoTrafficBucket(x)
	}
	return out
}
