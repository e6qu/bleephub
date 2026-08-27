package graphqlapi

// Late residue-field installers for the Gist, App and Topic objects. They
// share one precondition — every cross-family type they reach must already be
// assembled — so addMiscGraphQLFields runs from the branch-protection installer,
// after the account, gist, sponsors, marketplace and enterprise families.

// addMiscGraphQLFields installs the residue fields for the misc families.
func (s *Resolver) addMiscGraphQLFields() {
	s.addGistResidueFields()
	s.addAppResidueFields()
	s.addTopicResidueFields()
}
