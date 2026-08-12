package store

import "time"

// OrgScopedItem abstracts the selected-repositories surface shared by
// organization secrets and organization variables, so the per-repo
// add/remove endpoints run through one core.
type OrgScopedItem interface {
	ItemVisibility() string
	SelectedIDs() []int
	SetSelectedIDs([]int)
	TouchUpdated(time.Time)
}

func (sec *OrgSecret) ItemVisibility() string     { return sec.Visibility }
func (sec *OrgSecret) SelectedIDs() []int         { return sec.SelectedRepoIDs }
func (sec *OrgSecret) SetSelectedIDs(ids []int)   { sec.SelectedRepoIDs = ids }
func (sec *OrgSecret) TouchUpdated(now time.Time) { sec.UpdatedAt = now }
func (v *ActionsVariable) ItemVisibility() string { return v.Visibility }
func (v *ActionsVariable) SelectedIDs() []int     { return v.SelectedRepoIDs }
func (v *ActionsVariable) SetSelectedIDs(ids []int) {
	v.SelectedRepoIDs = ids
}
func (v *ActionsVariable) TouchUpdated(now time.Time) { v.UpdatedAt = now }

func (sec *DependabotOrgSecret) ItemVisibility() string     { return sec.Visibility }
func (sec *DependabotOrgSecret) SelectedIDs() []int         { return sec.SelectedRepoIDs }
func (sec *DependabotOrgSecret) SetSelectedIDs(ids []int)   { sec.SelectedRepoIDs = ids }
func (sec *DependabotOrgSecret) TouchUpdated(now time.Time) { sec.UpdatedAt = now }

func (sec *CodespaceSecret) ItemVisibility() string {
	if sec.Visibility == "" {
		return "all"
	}
	return sec.Visibility
}
func (sec *CodespaceSecret) SelectedIDs() []int         { return sec.SelectedRepoIDs }
func (sec *CodespaceSecret) SetSelectedIDs(ids []int)   { sec.SelectedRepoIDs = ids }
func (sec *CodespaceSecret) TouchUpdated(now time.Time) { sec.UpdatedAt = now }
