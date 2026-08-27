package store

import _ "embed"

// The real documents GitHub serves from /codes_of_conduct. Both REST and
// GraphQL render from this one catalog.

//go:embed coc_contributor_covenant.md
var cocContributorCovenantBody string

//go:embed coc_citizen_code_of_conduct.md
var cocCitizenCodeOfConductBody string

// CodeOfConduct is one entry of the codes-of-conduct catalog.
type CodeOfConduct struct {
	Key  string
	Name string
	Body string
}

// CodesOfConductCatalog is ordered alphabetically by key, as GitHub lists it.
var CodesOfConductCatalog = []CodeOfConduct{
	{Key: "citizen_code_of_conduct", Name: "Citizen Code of Conduct", Body: cocCitizenCodeOfConductBody},
	{Key: "contributor_covenant", Name: "Contributor Covenant", Body: cocContributorCovenantBody},
}
