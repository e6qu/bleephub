package graphqlapi

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// Issue.timelineItems / PullRequest.timelineItems, assembled from the same records as the REST `/issues/{n}/timeline`
// endpoint plus read-time cross-references. Ordered by timestamp, tiebroken by the store's monotonic id.

// timelineEventTypeNames maps a stored issue-event row's `Event` to the GraphQL union member that renders it. An event with no entry is left out.
var timelineEventTypeNames = map[string]string{
	"closed":                  "ClosedEvent",
	"reopened":                "ReopenedEvent",
	"labeled":                 "LabeledEvent",
	"unlabeled":               "UnlabeledEvent",
	"assigned":                "AssignedEvent",
	"unassigned":              "UnassignedEvent",
	"milestoned":              "MilestonedEvent",
	"demilestoned":            "DemilestonedEvent",
	"locked":                  "LockedEvent",
	"unlocked":                "UnlockedEvent",
	"pinned":                  "PinnedEvent",
	"unpinned":                "UnpinnedEvent",
	"renamed":                 "RenamedTitleEvent",
	"transferred":             "TransferredEvent",
	"converted_to_discussion": "ConvertedToDiscussionEvent",
	"mentioned":               "MentionedEvent",
	"comment_deleted":         "CommentDeletedEvent",
	"referenced":              "ReferencedEvent",
	"merged":                  "MergedEvent",
	"review_requested":        "ReviewRequestedEvent",
	"review_request_removed":  "ReviewRequestRemovedEvent",
	"review_dismissed":        "ReviewDismissedEvent",
	"ready_for_review":        "ReadyForReviewEvent",
	"convert_to_draft":        "ConvertToDraftEvent",
	"auto_merge_enabled":      "AutoMergeEnabledEvent",
	"auto_merge_disabled":     "AutoMergeDisabledEvent",
	"head_ref_deleted":        "HeadRefDeletedEvent",
	"head_ref_restored":       "HeadRefRestoredEvent",
	"base_ref_changed":        "BaseRefChangedEvent",
}

// issueTimelineMemberNames / pullRequestTimelineMemberNames are the event object types each union admits, beyond the shared non-event members.
var issueTimelineMemberNames = []string{
	"AddedToProjectV2Event",
	"AssignedEvent",
	"ClosedEvent",
	"CommentDeletedEvent",
	"ConvertedToDiscussionEvent",
	"CrossReferencedEvent",
	"DemilestonedEvent",
	"LabeledEvent",
	"LockedEvent",
	"MentionedEvent",
	"MilestonedEvent",
	"PinnedEvent",
	"ReferencedEvent",
	"RenamedTitleEvent",
	"ReopenedEvent",
	"TransferredEvent",
	"UnassignedEvent",
	"UnlabeledEvent",
	"UnlockedEvent",
	"UnpinnedEvent",
}

var pullRequestTimelineMemberNames = []string{
	"AddedToProjectV2Event",
	"AssignedEvent",
	"AutoMergeDisabledEvent",
	"AutoMergeEnabledEvent",
	"BaseRefChangedEvent",
	"ClosedEvent",
	"CommentDeletedEvent",
	"ConvertToDraftEvent",
	"ConvertedToDiscussionEvent",
	"CrossReferencedEvent",
	"DemilestonedEvent",
	"HeadRefDeletedEvent",
	"HeadRefRestoredEvent",
	"LabeledEvent",
	"LockedEvent",
	"MentionedEvent",
	"MergedEvent",
	"MilestonedEvent",
	"PinnedEvent",
	"ReadyForReviewEvent",
	"ReferencedEvent",
	"RenamedTitleEvent",
	"ReopenedEvent",
	"ReviewDismissedEvent",
	"ReviewRequestRemovedEvent",
	"ReviewRequestedEvent",
	"TransferredEvent",
	"UnassignedEvent",
	"UnlabeledEvent",
	"UnlockedEvent",
	"UnpinnedEvent",
}

// timelineItemTypeNames maps a union member's type name to its item-type enum value. Spelled out rather than derived so a mis-case cannot pass the ratchet.
var timelineItemTypeNames = map[string]string{
	"AddedToProjectV2Event":      "ADDED_TO_PROJECT_V2_EVENT",
	"AssignedEvent":              "ASSIGNED_EVENT",
	"AutoMergeDisabledEvent":     "AUTO_MERGE_DISABLED_EVENT",
	"AutoMergeEnabledEvent":      "AUTO_MERGE_ENABLED_EVENT",
	"BaseRefChangedEvent":        "BASE_REF_CHANGED_EVENT",
	"ClosedEvent":                "CLOSED_EVENT",
	"CommentDeletedEvent":        "COMMENT_DELETED_EVENT",
	"ConvertToDraftEvent":        "CONVERT_TO_DRAFT_EVENT",
	"ConvertedToDiscussionEvent": "CONVERTED_TO_DISCUSSION_EVENT",
	"CrossReferencedEvent":       "CROSS_REFERENCED_EVENT",
	"DemilestonedEvent":          "DEMILESTONED_EVENT",
	"HeadRefDeletedEvent":        "HEAD_REF_DELETED_EVENT",
	"HeadRefRestoredEvent":       "HEAD_REF_RESTORED_EVENT",
	"IssueComment":               "ISSUE_COMMENT",
	"LabeledEvent":               "LABELED_EVENT",
	"LockedEvent":                "LOCKED_EVENT",
	"MentionedEvent":             "MENTIONED_EVENT",
	"MergedEvent":                "MERGED_EVENT",
	"MilestonedEvent":            "MILESTONED_EVENT",
	"PinnedEvent":                "PINNED_EVENT",
	"PullRequestCommit":          "PULL_REQUEST_COMMIT",
	"PullRequestReview":          "PULL_REQUEST_REVIEW",
	"PullRequestReviewThread":    "PULL_REQUEST_REVIEW_THREAD",
	"ReadyForReviewEvent":        "READY_FOR_REVIEW_EVENT",
	"ReferencedEvent":            "REFERENCED_EVENT",
	"RenamedTitleEvent":          "RENAMED_TITLE_EVENT",
	"ReopenedEvent":              "REOPENED_EVENT",
	"ReviewDismissedEvent":       "REVIEW_DISMISSED_EVENT",
	"ReviewRequestRemovedEvent":  "REVIEW_REQUEST_REMOVED_EVENT",
	"ReviewRequestedEvent":       "REVIEW_REQUESTED_EVENT",
	"TransferredEvent":           "TRANSFERRED_EVENT",
	"UnassignedEvent":            "UNASSIGNED_EVENT",
	"UnlabeledEvent":             "UNLABELED_EVENT",
	"UnlockedEvent":              "UNLOCKED_EVENT",
	"UnpinnedEvent":              "UNPINNED_EVENT",
}

// issueTimelineItemTypes / pullRequestTimelineItemTypes map each union's member type names to their item-type enum values.
func issueTimelineItemTypes() map[string]string {
	return timelineItemTypesFor(append([]string{"IssueComment"}, issueTimelineMemberNames...))
}

func pullRequestTimelineItemTypes() map[string]string {
	return timelineItemTypesFor(append([]string{
		"IssueComment", "PullRequestCommit", "PullRequestReview", "PullRequestReviewThread",
	}, pullRequestTimelineMemberNames...))
}

func timelineItemTypesFor(members []string) map[string]string {
	out := make(map[string]string, len(members))
	for _, name := range members {
		out[name] = timelineItemTypeNames[name]
	}
	return out
}

// sortedItemTypeValues returns the enum values sorted, so the assembled enum is byte-identical across runs.
func sortedItemTypeValues(byName map[string]string) []string {
	values := make([]string, 0, len(byName))
	for _, value := range byName {
		values = append(values, value)
	}
	sort.Strings(values)
	return values
}

// timelineEntry is one candidate item: sort key, cursor identity, union member name (for `itemTypes` filtering) and the deferred render of its source map.
type timelineEntry struct {
	at       time.Time
	rank     int
	order    int
	typeName string
	identity string
	render   func() map[string]interface{}
}

// timelineParentResolver renders the issue or pull request an event belongs to. Resolved lazily from the parent's identity,
// since embedding the parent in every event source would re-render the whole issue once per event.
func (s *Resolver) timelineParentResolver(p graphql.ResolveParams) (interface{}, error) {
	event, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("timeline event source: unexpected type %T", p.Source)
	}
	parentType, _ := event["_parentType"].(string)
	parentID, _ := event["_parentID"].(int)
	return optionalObject(s.timelineParentSource(parentType, parentID)), nil
}

// timelineParentSource renders one timeline parent, tagged with its typename so the abstract types dispatch without re-reading the store.
func (s *Resolver) timelineParentSource(parentType string, parentID int) map[string]interface{} {
	switch parentType {
	case "pull_request":
		pr := s.store.GetPullRequest(parentID)
		if pr == nil {
			return nil
		}
		source := pullRequestToGQL(pr, s.store)
		source["__typename"] = "PullRequest"
		return source
	default:
		issue := s.store.GetIssue(parentID)
		if issue == nil {
			return nil
		}
		source := issueToGQL(issue, s.store)
		source["__typename"] = "Issue"
		return source
	}
}

// resolveTimelineItems answers Issue.timelineItems / PullRequest.timelineItems.
func (s *Resolver) resolveTimelineItems(p graphql.ResolveParams, parentType string) (interface{}, error) {
	source, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("timeline source: unexpected type %T", p.Source)
	}
	parentID, _ := source["databaseId"].(int)
	updatedAt, _ := source["updatedAt"].(string)

	entries := s.timelineEntries(p.Context, parentType, parentID)
	entries = filterTimelineEntries(entries, p.Args, s.timelineMembers(parentType))
	sortTimelineEntries(entries)
	for _, entry := range entries {
		if stamp := entry.at.UTC().Format(time.RFC3339); stamp > updatedAt {
			updatedAt = stamp
		}
	}
	return paginateTimelineEntries(entries, p.Args, updatedAt), nil
}

// timelineMembers is the set of union member names the parent's connection admits, so an itemTypes filter for a foreign member yields empty, not a leak.
func (s *Resolver) timelineMembers(parentType string) map[string]bool {
	if s.graphqlTypes.timeline == nil {
		return nil
	}
	if parentType == "pull_request" {
		return s.graphqlTypes.timeline.pullMemberSet
	}
	return s.graphqlTypes.timeline.issueMemberSet
}

// filterTimelineEntries applies `itemTypes`, `since` (items at or after the timestamp), and the union's own membership.
func filterTimelineEntries(entries []timelineEntry, args map[string]interface{}, members map[string]bool) []timelineEntry {
	wanted := map[string]bool{}
	if raw, ok := args["itemTypes"].([]interface{}); ok && len(raw) > 0 {
		for _, value := range raw {
			wanted[fmt.Sprintf("%v", value)] = true
		}
	}
	var since time.Time
	if raw, ok := args["since"].(string); ok && raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			since = parsed
		}
	}
	out := entries[:0]
	for _, entry := range entries {
		if members != nil && !members[entry.typeName] {
			continue
		}
		if len(wanted) > 0 && !wanted[timelineItemTypeNames[entry.typeName]] {
			continue
		}
		if !since.IsZero() && entry.at.Before(since) {
			continue
		}
		out = append(out, entry)
	}
	return out
}

// sortTimelineEntries orders oldest first. `rank` breaks ties at an identical instant (a commit before anything reacting to it);
// `order`, the store's monotonic row id, is the final tiebreaker so cursor boundaries reproduce.
func sortTimelineEntries(entries []timelineEntry) {
	sort.SliceStable(entries, func(a, b int) bool {
		if !entries[a].at.Equal(entries[b].at) {
			return entries[a].at.Before(entries[b].at)
		}
		if entries[a].rank != entries[b].rank {
			return entries[a].rank < entries[b].rank
		}
		return entries[a].order < entries[b].order
	})
}

// paginateTimelineEntries windows the timeline and adds filteredCount (window size before slicing) and pageCount (returned page size).
// `skip` advances the window start, as GitHub's `skip` argument does.
func paginateTimelineEntries(entries []timelineEntry, args map[string]interface{}, updatedAt string) map[string]interface{} {
	items := make([]gqlConnItem, len(entries))
	for i := range entries {
		entry := entries[i]
		items[i] = gqlConnItem{identity: entry.identity, render: entry.render}
	}
	total := len(items)
	start, end := 0, total
	if after, ok := args["after"].(string); ok && after != "" {
		index := resolveConnectionIndexForItems(items, after, decodeCursor(after))
		if index >= total {
			start = total
		} else {
			start = index + 1
		}
	}
	if before, ok := args["before"].(string); ok && before != "" {
		end = resolveConnectionIndexForItems(items, before, decodeCursor(before))
	}
	start, end = clampTimelineWindow(start, end, total)
	filteredCount := end - start

	if skip, ok := intArg(args, "skip"); ok && skip > 0 {
		start += skip
		start, end = clampTimelineWindow(start, end, total)
	}
	if last, ok := intArg(args, "last"); ok && last > 0 {
		if last > 100 {
			last = 100
		}
		if end-start > last {
			start = end - last
		}
	}
	if first, ok := intArg(args, "first"); ok && first >= 0 {
		if first > 100 {
			first = 100
		}
		if end-start > first {
			end = start + first
		}
	}
	if _, ok := intArg(args, "first"); !ok {
		if last, hasLast := intArg(args, "last"); !hasLast || last <= 0 {
			if end-start > 30 {
				end = start + 30
			}
		}
	}

	connection := buildConnectionWindowLazy(items, start, end, total)
	connection["filteredCount"] = filteredCount
	connection["pageCount"] = end - start
	connection["updatedAt"] = updatedAt
	return connection
}

func clampTimelineWindow(start, end, total int) (int, int) {
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	if end < 0 {
		end = 0
	}
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	return start, end
}

// timelineEntries collects every candidate item for one issue or pull request.
func (s *Resolver) timelineEntries(ctx context.Context, parentType string, parentID int) []timelineEntry {
	var repo *store.Repo
	var parentNumber int
	switch parentType {
	case "pull_request":
		pr := s.store.GetPullRequest(parentID)
		if pr == nil {
			return nil
		}
		repo, parentNumber = s.store.GetRepoByID(pr.RepoID), pr.Number
	default:
		issue := s.store.GetIssue(parentID)
		if issue == nil {
			return nil
		}
		repo, parentNumber = s.store.GetRepoByID(issue.RepoID), issue.Number
	}
	if repo == nil {
		return nil
	}

	entries := s.timelineEventEntries(parentType, parentID, repo)
	entries = append(entries, s.timelineCommentEntries(parentType, parentID)...)
	if parentType == "pull_request" {
		entries = append(entries, s.timelinePullRequestEntries(parentID, repo)...)
	}
	entries = append(entries, s.timelineCrossReferenceEntries(ctx, repo, parentType, parentID, parentNumber)...)
	entries = append(entries, s.timelineProjectV2Entries(ctx, parentType, parentID)...)
	return entries
}

// timelineEventEntries renders the stored issue-event rows.
func (s *Resolver) timelineEventEntries(parentType string, parentID int, repo *store.Repo) []timelineEntry {
	var events []*store.IssueEvent
	if parentType == "pull_request" {
		events = s.store.ListPullRequestEvents(repo.ID, parentID)
	} else {
		events = s.store.ListIssueEvents(repo.ID, parentID)
	}

	// State reason and closing subject belong only to the close that produced
	// the current state, not to every close in the history.
	latestClose := 0
	for _, event := range events {
		if event.Event == "closed" {
			latestClose = event.ID
		}
	}
	if !s.timelineParentIsClosed(parentType, parentID) {
		latestClose = 0
	}

	entries := make([]timelineEntry, 0, len(events))
	for _, event := range events {
		typeName, ok := timelineEventTypeNames[event.Event]
		if !ok {
			continue
		}
		// PinnedEvent/UnpinnedEvent/TransferredEvent name `issue: Issue!`, so a pull request behind that field would misreport its type.
		if parentType != "issue" && (typeName == "PinnedEvent" || typeName == "UnpinnedEvent" || typeName == "TransferredEvent") {
			continue
		}
		event := event
		entries = append(entries, timelineEntry{
			at:       event.CreatedAt,
			rank:     1,
			order:    event.ID,
			typeName: typeName,
			identity: event.NodeID,
			render: func() map[string]interface{} {
				return s.timelineEventSource(event, typeName, parentType, parentID, repo, event.ID == latestClose)
			},
		})
	}
	return entries
}

func (s *Resolver) timelineParentIsClosed(parentType string, parentID int) bool {
	if parentType == "pull_request" {
		pr := s.store.GetPullRequest(parentID)
		return pr != nil && pr.State != "OPEN"
	}
	issue := s.store.GetIssue(parentID)
	return issue != nil && issue.State == "CLOSED"
}

// timelineEventSource renders one stored event into its union member's source map, carrying the id/createdAt/actor triple plus the parent identity.
func (s *Resolver) timelineEventSource(event *store.IssueEvent, typeName, parentType string, parentID int, repo *store.Repo, isLatestClose bool) map[string]interface{} {
	s.store.Mu.RLock()
	actor := optionalRendered(s.store.Users[event.ActorID], userToGraphQL)
	s.store.Mu.RUnlock()

	source := map[string]interface{}{
		"__typename":  typeName,
		"nodeID":      event.NodeID,
		"databaseId":  event.ID,
		"createdAt":   event.CreatedAt.UTC().Format(time.RFC3339),
		"actor":       actor,
		"_parentType": parentType,
		"_parentID":   parentID,
	}
	subjectPath := fmt.Sprintf("/%s/issues/%d", repo.FullName, s.timelineParentNumber(parentType, parentID))
	if parentType == "pull_request" {
		subjectPath = fmt.Sprintf("/%s/pull/%d", repo.FullName, s.timelineParentNumber(parentType, parentID))
	}

	switch typeName {
	case "ClosedEvent":
		source["resourcePath"] = fmt.Sprintf("%s#event-%d", subjectPath, event.ID)
		source["url"] = externalURL(source["resourcePath"].(string))
		source["duplicateOf"] = nil
		source["intent"] = nil
		source["stateReason"] = nil
		source["closer"] = nil
		if isLatestClose {
			source["stateReason"] = s.timelineCloseStateReason(parentType, parentID)
			source["closer"] = optionalObject(s.timelineCloser(parentType, parentID, repo))
		}
	case "ReopenedEvent":
		source["stateReason"] = nil
	case "LabeledEvent", "UnlabeledEvent":
		s.store.Mu.RLock()
		label := optionalRendered(s.store.Labels[event.LabelID], labelToGQL)
		s.store.Mu.RUnlock()
		source["label"] = label
		source["intent"] = nil
		source["rationale"] = nil
	case "AssignedEvent", "UnassignedEvent":
		s.store.Mu.RLock()
		assignee := optionalRendered(s.store.Users[event.AssigneeID], userToGraphQL)
		s.store.Mu.RUnlock()
		if assigned, ok := assignee.(map[string]interface{}); ok {
			tagged := make(map[string]interface{}, len(assigned)+1)
			for key, value := range assigned {
				tagged[key] = value
			}
			tagged["__typename"] = "User"
			assignee = tagged
		}
		source["assignee"] = assignee
		source["user"] = assignee
		source["intent"] = nil
	case "MilestonedEvent", "DemilestonedEvent":
		s.store.Mu.RLock()
		title := ""
		if milestone := s.store.Milestones[event.MilestoneID]; milestone != nil {
			title = milestone.Title
		}
		s.store.Mu.RUnlock()
		source["milestoneTitle"] = title
	case "RenamedTitleEvent":
		source["previousTitle"] = event.RenameFrom
		source["currentTitle"] = event.RenameTo
	case "LockedEvent":
		source["lockReason"] = graphQLLockReason(event.LockReason)
	case "TransferredEvent":
		source["fromRepository"] = optionalObject(s.timelineTransferSource(event, repo))
	case "ConvertedToDiscussionEvent":
		source["discussion"] = optionalObject(s.timelineConvertedDiscussion(event, repo, parentID))
	case "CommentDeletedEvent":
		// The comment row is gone, so the event carries the deleted comment's id and author itself.
		source["databaseId"] = event.CommentID
		s.store.Mu.RLock()
		source["deletedCommentAuthor"] = optionalRendered(s.store.Users[event.AssigneeID], userToGraphQL)
		s.store.Mu.RUnlock()
	case "MentionedEvent":
		source["databaseId"] = event.ID
	case "ReferencedEvent":
		source["isCrossRepository"] = false
		source["isDirectReference"] = true
		source["commitRepository"] = repoToGraphQL(s.store, s.store.SnapRepo(repo))
		source["commit"] = optionalObject(s.timelineCommitSource(repo, event.CommitID))
	case "MergedEvent":
		source["resourcePath"] = fmt.Sprintf("%s#event-%d", subjectPath, event.ID)
		source["url"] = externalURL(source["resourcePath"].(string))
		source["mergeRefName"], source["mergeRef"], source["commit"] = s.timelineMergeSubject(parentID, repo, event)
	case "ReviewRequestedEvent", "ReviewRequestRemovedEvent":
		s.store.Mu.RLock()
		reviewer := optionalRendered(s.store.Users[event.RequestedReviewerID], userToGraphQL)
		s.store.Mu.RUnlock()
		source["requestedReviewer"] = reviewer
	case "ReviewDismissedEvent":
		source["resourcePath"] = fmt.Sprintf("%s#event-%d", subjectPath, event.ID)
		source["url"] = externalURL(source["resourcePath"].(string))
		s.timelineFillDismissal(source, parentID, event)
	case "ReadyForReviewEvent", "ConvertToDraftEvent":
		source["resourcePath"] = fmt.Sprintf("%s#event-%d", subjectPath, event.ID)
		source["url"] = externalURL(source["resourcePath"].(string))
	case "AutoMergeEnabledEvent":
		source["enabler"] = actor
	case "AutoMergeDisabledEvent":
		source["disabler"] = actor
		source["reason"] = nil
		source["reasonCode"] = nil
	case "HeadRefDeletedEvent":
		source["headRefName"], source["headRef"] = s.timelineHeadRef(parentID, repo)
	case "BaseRefChangedEvent":
		source["previousRefName"] = event.RenameFrom
		source["currentRefName"] = event.RenameTo
	}
	return source
}

func (s *Resolver) timelineParentNumber(parentType string, parentID int) int {
	if parentType == "pull_request" {
		if pr := s.store.GetPullRequest(parentID); pr != nil {
			return pr.Number
		}
		return 0
	}
	if issue := s.store.GetIssue(parentID); issue != nil {
		return issue.Number
	}
	return 0
}

func (s *Resolver) timelineCloseStateReason(parentType string, parentID int) interface{} {
	if parentType == "pull_request" {
		return nil
	}
	issue := s.store.GetIssue(parentID)
	if issue == nil || issue.StateReason == "" {
		return nil
	}
	return issue.StateReason
}

// timelineCloser is the pull request whose closing reference closed this issue (ClosedEvent.closer).
func (s *Resolver) timelineCloser(parentType string, parentID int, repo *store.Repo) map[string]interface{} {
	if parentType != "issue" {
		return nil
	}
	issue := s.store.GetIssue(parentID)
	if issue == nil {
		return nil
	}
	closing := closedByPullRequestsForIssue(s.store, repo, issue, true)
	if len(closing) == 0 {
		return nil
	}
	source := pullRequestToGQL(closing[0], s.store)
	source["__typename"] = "PullRequest"
	return source
}

// timelineTransferSource is the repository an issue was transferred out of; the event records its id in CommitID. Null when that slot holds no repository id.
func (s *Resolver) timelineTransferSource(event *store.IssueEvent, repo *store.Repo) map[string]interface{} {
	id, err := strconv.Atoi(event.CommitID)
	if err != nil || id == 0 || id == repo.ID {
		return nil
	}
	from := s.store.GetRepoByID(id)
	if from == nil {
		return nil
	}
	return repoToGraphQL(s.store, s.store.SnapRepo(from))
}

// timelineConvertedDiscussion is the discussion an issue was converted into; the event records its number in CommitID.
func (s *Resolver) timelineConvertedDiscussion(event *store.IssueEvent, repo *store.Repo, parentID int) map[string]interface{} {
	number, err := strconv.Atoi(event.CommitID)
	if err != nil || number <= 0 {
		return nil
	}
	discussion := s.store.GetDiscussionByNumber(repo.ID, number)
	if discussion == nil {
		return nil
	}
	return discussionToGQL(discussion, s.store)
}

// timelineMergeSubject is a MergedEvent's base ref name, its Ref and the merge commit that landed on it.
func (s *Resolver) timelineMergeSubject(prID int, repo *store.Repo, event *store.IssueEvent) (string, interface{}, interface{}) {
	pr := s.store.GetPullRequest(prID)
	if pr == nil {
		return "", nil, nil
	}
	ref := gitRefSource(repo.FullName, "refs/heads/"+pr.BaseRefName, "")
	sha := pr.MergeCommitSHA
	if sha == "" {
		sha = event.CommitID
	}
	return pr.BaseRefName, ref, optionalObject(s.timelineCommitSource(repo, sha))
}

// timelineHeadRef is a HeadRefDeletedEvent's branch name and, when the branch has since been restored, its Ref.
func (s *Resolver) timelineHeadRef(prID int, repo *store.Repo) (string, interface{}) {
	pr := s.store.GetPullRequest(prID)
	if pr == nil {
		return "", nil
	}
	head := s.store.GetRepoByID(pr.HeadRepoID)
	if head == nil {
		head = repo
	}
	owner, name, found := strings.Cut(head.FullName, "/")
	if !found {
		return pr.HeadRefName, nil
	}
	storage := s.store.GetGitStorage(owner, name)
	if storage == nil {
		return pr.HeadRefName, nil
	}
	sha := store.ResolveBranchSha(storage, pr.HeadRefName)
	if sha == "" {
		// Branch still gone (the ordinary post-deletion state): name, null ref.
		return pr.HeadRefName, nil
	}
	return pr.HeadRefName, gitRefSource(head.FullName, "refs/heads/"+pr.HeadRefName, sha)
}

// timelineFillDismissal resolves the review a `review_dismissed` event dismissed. The two rows are not linked, so it matches by nearest DismissedAt timestamp.
func (s *Resolver) timelineFillDismissal(source map[string]interface{}, prID int, event *store.IssueEvent) {
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	var best *store.PullRequestReview
	var bestGap time.Duration
	for _, review := range s.store.PRReviewsByPR[prID] {
		if review.DismissedAt == nil {
			continue
		}
		gap := review.DismissedAt.Sub(event.CreatedAt)
		if gap < 0 {
			gap = -gap
		}
		if best == nil || gap < bestGap {
			best, bestGap = review, gap
		}
	}
	source["previousReviewState"] = "COMMENTED"
	source["dismissalMessage"] = nil
	source["dismissalMessageHTML"] = nil
	source["review"] = nil
	if best == nil {
		return
	}
	source["review"] = prReviewSourceLocked(best, s.store)
	source["dismissalMessage"] = nilStr(best.DismissalMessage)
	source["dismissalMessageHTML"] = nilStr(best.DismissalMessage)
	if best.PreviousState != "" {
		source["previousReviewState"] = best.PreviousState
	}
}

// timelineCommitSource renders a commit by sha from git storage, or nil when the
// repository holds no such object.
func (s *Resolver) timelineCommitSource(repo *store.Repo, sha string) map[string]interface{} {
	if sha == "" {
		return nil
	}
	owner, name, found := strings.Cut(repo.FullName, "/")
	if !found {
		return nil
	}
	storage := s.store.GetGitStorage(owner, name)
	if storage == nil {
		return nil
	}
	repository, err := git.Open(storage, nil)
	if err != nil {
		return nil
	}
	commit, err := repository.CommitObject(plumbing.NewHash(sha))
	if err != nil || commit == nil {
		return nil
	}
	s.store.Mu.RLock()
	defer s.store.Mu.RUnlock()
	return gitCommitToGQLLocked(commit, s.store, repo.FullName)
}

// timelineCommentEntries renders the conversation comments as IssueComment members.
func (s *Resolver) timelineCommentEntries(parentType string, parentID int) []timelineEntry {
	comments := s.store.ListCommentsFor(parentType, parentID)
	entries := make([]timelineEntry, 0, len(comments))
	for _, comment := range comments {
		comment := comment
		entries = append(entries, timelineEntry{
			at:       comment.CreatedAt,
			rank:     2,
			order:    comment.ID,
			typeName: "IssueComment",
			identity: comment.NodeID,
			render: func() map[string]interface{} {
				source := commentToGQL(comment, s.store)
				source["__typename"] = "IssueComment"
				return source
			},
		})
	}
	return entries
}

// timelinePullRequestEntries adds the members only a pull request's timeline has: its real git commits, its submitted reviews and its review threads.
func (s *Resolver) timelinePullRequestEntries(prID int, repo *store.Repo) []timelineEntry {
	pr := s.store.GetPullRequest(prID)
	if pr == nil {
		return nil
	}
	var entries []timelineEntry

	if storage, repoFullName := store.PullRequestGitStorage(s.store, repo, pr); storage != nil {
		commits, err := store.PullRequestCommitObjectsFromStorage(storage, pr)
		if err == nil {
			for index, commit := range commits {
				commit, index := commit, index
				sha := commit.Hash.String()
				entries = append(entries, timelineEntry{
					at:       commit.Author.When.UTC(),
					rank:     0,
					order:    index,
					typeName: "PullRequestCommit",
					identity: "PRC_" + sha,
					render: func() map[string]interface{} {
						s.store.Mu.RLock()
						defer s.store.Mu.RUnlock()
						return map[string]interface{}{
							"__typename": "PullRequestCommit",
							"nodeID":     "PRC_" + sha,
							"commit":     gitCommitToGQLLocked(commit, s.store, repoFullName),
						}
					},
				})
			}
		}
	}

	for _, review := range s.store.ListPullRequestReviews(repo.FullName, pr.Number) {
		if review.State == "PENDING" {
			continue
		}
		review := review
		at := review.CreatedAt
		if review.SubmittedAt != nil {
			at = *review.SubmittedAt
		}
		entries = append(entries, timelineEntry{
			at:       at,
			rank:     2,
			order:    review.ID,
			typeName: "PullRequestReview",
			identity: review.NodeID,
			render: func() map[string]interface{} {
				s.store.Mu.RLock()
				defer s.store.Mu.RUnlock()
				source := prReviewSourceLocked(review, s.store)
				source["__typename"] = "PullRequestReview"
				return source
			},
		})
	}

	threads := s.store.PRReviewComments.ListThreads(pr.ID)
	s.store.Mu.RLock()
	rendered := reviewThreadsForGraphQL(threads, s.store)
	s.store.Mu.RUnlock()
	for index, thread := range threads {
		if len(thread.Comments) == 0 || index >= len(rendered) {
			continue
		}
		source := rendered[index]
		source["__typename"] = "PullRequestReviewThread"
		entries = append(entries, timelineEntry{
			at:       thread.Comments[0].CreatedAt,
			rank:     2,
			order:    thread.ID,
			typeName: "PullRequestReviewThread",
			identity: fmt.Sprintf("%v", source["id"]),
			render:   func() map[string]interface{} { return source },
		})
	}
	return entries
}

// timelineReferenceRE matches an issue/PR reference: optional owner/repo prefix then #<number>. Same grammar as the REST timeline's cross-reference derivation.
var timelineReferenceRE = regexp.MustCompile(`(?:\b([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+))?#([0-9]+)`)

// timelineCrossReferenceEntries derives the CrossReferencedEvent members, one per other issue or PR whose body names this one,
// at read time as the REST timeline does. A reference from a repository the viewer cannot read is dropped before pagination, so neither its contents nor its count leak.
func (s *Resolver) timelineCrossReferenceEntries(ctx context.Context, repo *store.Repo, parentType string, parentID, parentNumber int) []timelineEntry {
	type candidate struct {
		repoID     int
		isPull     bool
		id         int
		nodeID     string
		authorID   int
		createdAt  time.Time
		willClose  bool
		crossRepo  bool
		sourceRepo string
	}
	var candidates []candidate

	s.store.Mu.RLock()
	repoNames := map[int]string{}
	for id, candidateRepo := range s.store.Repos {
		repoNames[id] = candidateRepo.FullName
	}
	refersToTarget := func(sourceRepoID int, body string) (bool, bool) {
		sourceRepoName := repoNames[sourceRepoID]
		found := false
		for _, match := range timelineReferenceRE.FindAllStringSubmatch(body, -1) {
			qualified := match[1]
			if qualified == "" {
				qualified = sourceRepoName
			}
			if !strings.EqualFold(qualified, repo.FullName) {
				continue
			}
			if number, err := strconv.Atoi(match[2]); err == nil && number == parentNumber {
				found = true
			}
		}
		if !found {
			return false, false
		}
		for _, closing := range closingIssueRefs(sourceRepoName, body) {
			if strings.EqualFold(closing.repoFullName, repo.FullName) && closing.number == parentNumber {
				return true, true
			}
		}
		return true, false
	}
	for _, issue := range s.store.Issues {
		if parentType == "issue" && issue.ID == parentID {
			continue
		}
		refers, willClose := refersToTarget(issue.RepoID, issue.Body)
		if !refers {
			continue
		}
		candidates = append(candidates, candidate{
			repoID: issue.RepoID, id: issue.ID, nodeID: issue.NodeID, authorID: issue.AuthorID,
			createdAt: issue.CreatedAt, willClose: willClose,
			crossRepo: issue.RepoID != repo.ID, sourceRepo: repoNames[issue.RepoID],
		})
	}
	for _, pr := range s.store.PullRequests {
		if parentType == "pull_request" && pr.ID == parentID {
			continue
		}
		refers, willClose := refersToTarget(pr.RepoID, pr.Body)
		if !refers {
			continue
		}
		candidates = append(candidates, candidate{
			repoID: pr.RepoID, isPull: true, id: pr.ID, nodeID: pr.NodeID, authorID: pr.AuthorID,
			createdAt: pr.CreatedAt, willClose: willClose,
			crossRepo: pr.RepoID != repo.ID, sourceRepo: repoNames[pr.RepoID],
		})
	}
	s.store.Mu.RUnlock()

	visible := map[int]bool{}
	entries := make([]timelineEntry, 0, len(candidates))
	for _, item := range candidates {
		readable, decided := visible[item.repoID]
		if !decided {
			sourceRepo := s.store.GetRepoByID(item.repoID)
			readable = sourceRepo != nil && (!sourceRepo.Private || s.viewerCanReadRepo(ctx, sourceRepo))
			visible[item.repoID] = readable
		}
		if !readable {
			continue
		}
		item := item
		targetPath := fmt.Sprintf("/%s/issues/%d", repo.FullName, parentNumber)
		if parentType == "pull_request" {
			targetPath = fmt.Sprintf("/%s/pull/%d", repo.FullName, parentNumber)
		}
		identity := "CRE_" + item.nodeID
		entries = append(entries, timelineEntry{
			at:       item.createdAt,
			rank:     3,
			order:    item.id,
			typeName: "CrossReferencedEvent",
			identity: identity,
			render: func() map[string]interface{} {
				s.store.Mu.RLock()
				actor := optionalRendered(s.store.Users[item.authorID], userToGraphQL)
				s.store.Mu.RUnlock()
				return map[string]interface{}{
					"__typename":        "CrossReferencedEvent",
					"nodeID":            identity,
					"createdAt":         item.createdAt.UTC().Format(time.RFC3339),
					"referencedAt":      item.createdAt.UTC().Format(time.RFC3339),
					"actor":             actor,
					"isCrossRepository": item.crossRepo,
					"willCloseTarget":   item.willClose,
					"resourcePath":      targetPath,
					"url":               externalURL(targetPath),
					"source":            optionalObject(s.timelineReferenceSource(item.isPull, item.id)),
					"_parentType":       parentType,
					"_parentID":         parentID,
				}
			},
		})
	}
	return entries
}

// timelineReferenceSource renders the issue or PR that made a cross-reference, tagged for ReferencedSubject dispatch.
func (s *Resolver) timelineReferenceSource(isPull bool, id int) map[string]interface{} {
	if isPull {
		return s.timelineParentSource("pull_request", id)
	}
	return s.timelineParentSource("issue", id)
}

// timelineProjectV2Entries renders one AddedToProjectV2Event per project the issue or PR was added to. A project the viewer cannot read is dropped.
func (s *Resolver) timelineProjectV2Entries(ctx context.Context, parentType string, parentID int) []timelineEntry {
	contentType := "Issue"
	if parentType == "pull_request" {
		contentType = "PullRequest"
	}
	items := s.store.ProjectsV2.ListItemsForIssue(parentID)
	if contentType == "PullRequest" {
		items = s.store.ProjectsV2.ListItemsForPR(parentID)
	}
	entries := make([]timelineEntry, 0, len(items))
	for _, item := range items {
		project := s.store.ProjectsV2.GetProject(item.ProjectID)
		if project == nil {
			continue
		}
		owner := s.projectV2OwnerByID(project.OwnerID, project.OwnerType)
		if !s.canReadProjectV2(ctx, s.ghUserFromContext(ctx), owner, project) {
			continue
		}
		item, project := item, project
		entries = append(entries, timelineEntry{
			at:       item.CreatedAt,
			rank:     4,
			order:    item.ID,
			typeName: "AddedToProjectV2Event",
			identity: "APV2_" + item.NodeID,
			render: func() map[string]interface{} {
				s.store.Mu.RLock()
				actor := optionalRendered(s.store.Users[item.CreatorID], userToGraphQL)
				s.store.Mu.RUnlock()
				return map[string]interface{}{
					"__typename":   "AddedToProjectV2Event",
					"nodeID":       "APV2_" + item.NodeID,
					"createdAt":    item.CreatedAt.UTC().Format(time.RFC3339),
					"actor":        actor,
					"project":      optionalObject(projectV2ToGQL(s.store, project)),
					"wasAutomated": false,
					"_parentType":  parentType,
					"_parentID":    parentID,
				}
			},
		})
	}
	return entries
}
