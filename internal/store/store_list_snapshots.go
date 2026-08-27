package store

// STORE-021 clone + snapshot helpers for List* element types with mutable
// reference fields. Each List* wraps its result in the matching snapshot*
// helper under the read lock, so a caller can't race an in-place mutation nor
// leak a write back into the stored row. Reference fields get fresh backing
// arrays/maps; write-once nested pointees (a CheckRun's Output, a manifest)
// are shared by pointer.

import "time"

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
	if x.UpvoterIDs != nil {
		c.UpvoterIDs = append([]int(nil), x.UpvoterIDs...)
	}
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
	if x.Regex != nil {
		v := *x.Regex
		c.Regex = &v
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

// The following reuse existing single-object clone helpers.

func snapshotInstallations(in []*Installation) []*Installation {
	if in == nil {
		return nil
	}
	out := make([]*Installation, len(in))
	for i, x := range in {
		out[i] = CloneInstallation(x)
	}
	return out
}

func snapshotCodespaces(in []*Codespace) []*Codespace {
	if in == nil {
		return nil
	}
	out := make([]*Codespace, len(in))
	for i, x := range in {
		out[i] = CloneCodespace(x)
	}
	return out
}

func snapshotRulesets(in []*Ruleset) []*Ruleset {
	if in == nil {
		return nil
	}
	out := make([]*Ruleset, len(in))
	for i, x := range in {
		out[i] = cloneRuleset(x)
	}
	return out
}

func snapshotSecurityAdvisories(in []*SecurityAdvisory) []*SecurityAdvisory {
	if in == nil {
		return nil
	}
	out := make([]*SecurityAdvisory, len(in))
	for i, x := range in {
		out[i] = cloneSecurityAdvisory(x)
	}
	return out
}

func snapshotSecretScanningCustomPatterns(in []*SecretScanningCustomPattern) []*SecretScanningCustomPattern {
	if in == nil {
		return nil
	}
	out := make([]*SecretScanningCustomPattern, len(in))
	for i, x := range in {
		out[i] = cloneSecretScanningCustomPattern(x)
	}
	return out
}

func snapshotMarketplacePlans(in []*MarketplacePlan) []*MarketplacePlan {
	if in == nil {
		return nil
	}
	out := make([]*MarketplacePlan, len(in))
	for i, x := range in {
		out[i] = cloneMarketplacePlan(x)
	}
	return out
}

func snapshotMarketplacePurchases(in []*MarketplacePurchase) []*MarketplacePurchase {
	if in == nil {
		return nil
	}
	out := make([]*MarketplacePurchase, len(in))
	for i, x := range in {
		out[i] = CloneMarketplacePurchase(x)
	}
	return out
}

func snapshotWebhooks(in []*Webhook) []*Webhook {
	if in == nil {
		return nil
	}
	out := make([]*Webhook, len(in))
	for i, x := range in {
		out[i] = CloneWebhook(x)
	}
	return out
}

func snapshotIssueSuggestions(in []*IssueSuggestion) []*IssueSuggestion {
	if in == nil {
		return nil
	}
	out := make([]*IssueSuggestion, len(in))
	for i, x := range in {
		out[i] = cloneIssueSuggestion(x)
	}
	return out
}

func snapshotPullRequestStacks(in []*PullRequestStack) []*PullRequestStack {
	if in == nil {
		return nil
	}
	out := make([]*PullRequestStack, len(in))
	for i, x := range in {
		out[i] = clonePullRequestStack(x)
	}
	return out
}

// cloneUser detaches a user from the stored row (STORE-021). User is the most
// shared entity — StarRepo/UnstarRepo mutate StarredRepos in place — so
// member-list callers must hold snapshots.
func cloneUser(u *User) *User {
	if u == nil {
		return nil
	}
	c := *u
	if u.StarredRepos != nil {
		m := make(map[string]time.Time, len(u.StarredRepos))
		for k, v := range u.StarredRepos {
			m[k] = v
		}
		c.StarredRepos = m
	}
	if u.Hireable != nil {
		v := *u.Hireable
		c.Hireable = &v
	}
	c.Emails = append([]UserEmail(nil), u.Emails...)
	c.InteractionLimitExpiry = cloneTimePtr(u.InteractionLimitExpiry)
	c.ExternalIdentities = append([]ExternalIdentity(nil), u.ExternalIdentities...)
	return &c
}

func snapshotUsers(in []*User) []*User {
	if in == nil {
		return nil
	}
	out := make([]*User, len(in))
	for i, x := range in {
		out[i] = cloneUser(x)
	}
	return out
}

// snapshotSlice detaches a list whose element type is all-value (verified per
// type): a shallow struct copy of each element is a full snapshot.
func snapshotSlice[T any](in []*T) []*T {
	if in == nil {
		return nil
	}
	out := make([]*T, len(in))
	for i, x := range in {
		if x == nil {
			continue
		}
		c := *x
		out[i] = &c
	}
	return out
}

func snapshotOrgBudgets(in []*OrgBudget) []*OrgBudget {
	if in == nil {
		return nil
	}
	out := make([]*OrgBudget, len(in))
	for i, x := range in {
		out[i] = CloneBudget(x)
	}
	return out
}

func snapshotGistHistory(in []*GistHistory) []*GistHistory {
	if in == nil {
		return nil
	}
	out := make([]*GistHistory, len(in))
	for i, x := range in {
		if x == nil {
			continue
		}
		c := *x
		if x.ChangeStatus != nil {
			m := make(map[string]int, len(x.ChangeStatus))
			for k, v := range x.ChangeStatus {
				m[k] = v
			}
			c.ChangeStatus = m
		}
		out[i] = &c
	}
	return out
}
