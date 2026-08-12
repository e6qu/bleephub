package bleephub

import (
	"testing"

	"github.com/e6qu/bleephub/internal/store"
)

// TestGQLNodeFindersDecodeDBID covers the GQL-024 follow-up: every GraphQL
// node finder that keys its collection by database id now decodes the id
// embedded in the node ID for an O(1) lookup, guarded by an equality check so
// a prefix collision or a foreign-shaped id can never mis-resolve, and falls
// back to the full scan otherwise. Each finder is exercised for a real hit
// (the fast path) and for an unknown same-prefix id (the guard + fallback
// miss), so a wrong prefix or a dropped equality guard fails the test.
func TestGQLNodeFindersDecodeDBID(t *testing.T) {
	// pullsTestServer seeds a repo with a "feature" head branch and a "main"
	// base so CreatePullRequest resolves real head/base SHAs.
	s, admin, repo := pullsTestServer(t)
	st := s.store

	issue := st.CreateIssue(repo.ID, admin.ID, "issue", "", nil, nil, 0)
	label := st.CreateLabel(repo.ID, "bug", "", "ff0000")
	milestone := st.CreateMilestone(repo.ID, admin.ID, "v1", "", "open", nil)
	comment := st.CreateComment(issue.ID, admin.ID, "hello")
	pr := st.CreatePullRequest(repo.ID, admin.ID, "pr", "", "feature", "main", false, nil, nil, 0)

	category := st.CreateDiscussionCategory(repo.ID, "General", "", "", false)
	discussion := st.CreateDiscussion(repo.ID, category.ID, admin.ID, "topic", "body")
	discComment := st.CreateDiscussionComment(discussion.ID, admin.ID, "reply", 0)

	project := st.ProjectsV2.CreateProject(admin.ID, "User", "Roadmap", admin.ID)
	item := st.ProjectsV2.AddItem(project.ID, "Issue", issue.ID, admin.ID)
	field := st.ProjectsV2.CreateField(project.ID, "Status", store.ProjectV2FieldText, nil, nil)

	// Each case resolves a real record by its node id (fast path) and rejects an
	// unknown id that shares the same prefix (equality guard + fallback miss).
	t.Run("issue", func(t *testing.T) {
		if got := store.FindIssueByNodeID(st, issue.NodeID); got == nil || got.ID != issue.ID {
			t.Fatalf("findIssueByNodeID(%q) = %v, want issue %d", issue.NodeID, got, issue.ID)
		}
		if got := store.FindIssueByNodeID(st, "I_kgDO99999999"); got != nil {
			t.Fatalf("findIssueByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("label", func(t *testing.T) {
		if got := store.FindLabelByNodeID(st, label.NodeID); got == nil || got.ID != label.ID {
			t.Fatalf("findLabelByNodeID(%q) = %v, want label %d", label.NodeID, got, label.ID)
		}
		if got := store.FindLabelByNodeID(st, "LA_kgDO99999999"); got != nil {
			t.Fatalf("findLabelByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("milestone", func(t *testing.T) {
		if got := store.FindMilestoneByNodeID(st, milestone.NodeID); got == nil || got.ID != milestone.ID {
			t.Fatalf("findMilestoneByNodeID(%q) = %v, want milestone %d", milestone.NodeID, got, milestone.ID)
		}
		if got := store.FindMilestoneByNodeID(st, "MI_kgDO99999999"); got != nil {
			t.Fatalf("findMilestoneByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("comment", func(t *testing.T) {
		if got := st.LookupCommentByNodeID(comment.NodeID); got == nil || got.ID != comment.ID {
			t.Fatalf("LookupCommentByNodeID(%q) = %v, want comment %d", comment.NodeID, got, comment.ID)
		}
		if got := st.LookupCommentByNodeID("IC_kgDO99999999"); got != nil {
			t.Fatalf("LookupCommentByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("pull_request", func(t *testing.T) {
		if got := store.FindPullRequestByNodeID(st, pr.NodeID); got == nil || got.ID != pr.ID {
			t.Fatalf("findPullRequestByNodeID(%q) = %v, want pr %d", pr.NodeID, got, pr.ID)
		}
		if got := store.FindPullRequestByNodeID(st, "PR_kgDO99999999"); got != nil {
			t.Fatalf("findPullRequestByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("discussion", func(t *testing.T) {
		if got := store.FindDiscussionByNodeID(st, discussion.NodeID); got == nil || got.ID != discussion.ID {
			t.Fatalf("findDiscussionByNodeID(%q) = %v, want discussion %d", discussion.NodeID, got, discussion.ID)
		}
		if got := store.FindDiscussionByNodeID(st, "D_kgDO99999999"); got != nil {
			t.Fatalf("findDiscussionByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("discussion_category", func(t *testing.T) {
		if got := store.FindDiscussionCategoryByNodeID(st, category.NodeID); got == nil || got.ID != category.ID {
			t.Fatalf("findDiscussionCategoryByNodeID(%q) = %v, want category %d", category.NodeID, got, category.ID)
		}
		if got := store.FindDiscussionCategoryByNodeID(st, "DGC_kgDO99999999"); got != nil {
			t.Fatalf("findDiscussionCategoryByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("discussion_comment", func(t *testing.T) {
		if got := store.FindDiscussionCommentByNodeID(st, discComment.NodeID); got == nil || got.ID != discComment.ID {
			t.Fatalf("findDiscussionCommentByNodeID(%q) = %v, want comment %d", discComment.NodeID, got, discComment.ID)
		}
		if got := store.FindDiscussionCommentByNodeID(st, "DC_kgDO99999999"); got != nil {
			t.Fatalf("findDiscussionCommentByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("project_v2", func(t *testing.T) {
		if got := st.ProjectsV2.LookupProjectByNodeID(project.NodeID); got == nil || got.ID != project.ID {
			t.Fatalf("LookupProjectByNodeID(%q) = %v, want project %d", project.NodeID, got, project.ID)
		}
		if got := st.ProjectsV2.LookupProjectByNodeID("PVT_kgDO99999999"); got != nil {
			t.Fatalf("LookupProjectByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("project_v2_item", func(t *testing.T) {
		if got := st.ProjectsV2.LookupItemByNodeID(item.NodeID); got == nil || got.ID != item.ID {
			t.Fatalf("LookupItemByNodeID(%q) = %v, want item %d", item.NodeID, got, item.ID)
		}
		if got := st.ProjectsV2.LookupItemByNodeID("PVTI_kgDO99999999"); got != nil {
			t.Fatalf("LookupItemByNodeID(unknown) = %v, want nil", got)
		}
	})
	t.Run("project_v2_field", func(t *testing.T) {
		if got := st.ProjectsV2.LookupFieldByNodeID(field.NodeID); got == nil || got.ID != field.ID {
			t.Fatalf("LookupFieldByNodeID(%q) = %v, want field %d", field.NodeID, got, field.ID)
		}
		if got := st.ProjectsV2.LookupFieldByNodeID("PVTF_kgDO99999999"); got != nil {
			t.Fatalf("LookupFieldByNodeID(unknown) = %v, want nil", got)
		}
	})

	// The PVT_ / PVTI_ / PVTF_ prefixes share a leading run; the equality guard
	// must keep each finder from resolving another project-v2 kind's node id.
	t.Run("prefix_overlap_is_rejected", func(t *testing.T) {
		if got := st.ProjectsV2.LookupProjectByNodeID(item.NodeID); got != nil {
			t.Fatalf("LookupProjectByNodeID(item node id) = %v, want nil", got)
		}
		if got := st.ProjectsV2.LookupItemByNodeID(field.NodeID); got != nil {
			t.Fatalf("LookupItemByNodeID(field node id) = %v, want nil", got)
		}
	})
}
