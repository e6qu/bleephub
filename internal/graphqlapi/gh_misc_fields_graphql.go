package graphqlapi

// Late residue-field installers that complete the Gist, App and Topic objects.
// They are grouped here and driven from one entry point because they share the
// same precondition: every cross-family type they reach read-only — the gist
// comment connection, the IP allow-list connection, the stargazer and
// repository connections — must already be assembled. addMiscGraphQLFields is
// therefore called from the branch-protection family installer, which runs
// after the account, gist, sponsors, marketplace and enterprise families.

// addMiscGraphQLFields installs the residue fields for the misc families.
func (s *Resolver) addMiscGraphQLFields() {
	s.addGistResidueFields()
	s.addAppResidueFields()
	s.addTopicResidueFields()
}
