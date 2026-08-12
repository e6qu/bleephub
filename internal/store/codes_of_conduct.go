package store

import _ "embed"

// GitHub's codes-of-conduct catalog. The two entries and their body texts
// are the real documents GitHub serves from /codes_of_conduct. Both the
// REST layer and the GraphQL resolver layer render from this one catalog.
// (Moved from the server layer in ARCH-003.)

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

// CodesOfConductCatalog is ordered the way GitHub lists it (alphabetical by
// key).
var CodesOfConductCatalog = []CodeOfConduct{
	{Key: "citizen_code_of_conduct", Name: "Citizen Code of Conduct", Body: cocCitizenCodeOfConductBody},
	{Key: "contributor_covenant", Name: "Contributor Covenant", Body: cocContributorCovenantBody},
}
