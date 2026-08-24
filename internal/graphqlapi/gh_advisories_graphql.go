package graphqlapi

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/graphql-go/graphql"

	"github.com/e6qu/bleephub/internal/store"
)

// The security-advisory, vulnerability-alert and dependency-graph GraphQL
// surface: GitHub's global advisory database, a repository's Dependabot
// alerts, the manifests those alerts are derived from, and the one mutation
// that dismisses an alert.
//
// # What is public and what is not
//
// The three root fields — securityAdvisory, securityAdvisories and
// securityVulnerabilities — read the global advisory database, which is
// public on GitHub and public here. They take no viewer and apply no
// repository filter, because a published advisory is a public fact about a
// package rather than about the repository that happened to draft it. What
// they cannot reach is anything unpublished: a draft repository advisory is
// private to the repository's security team, so it never enters the listings
// these fields walk.
//
// Repository.vulnerabilityAlerts is the opposite: an alert says which
// vulnerable dependency a specific repository is running, which is exactly
// the fact a private repository is entitled to keep. The field answers only
// for a viewer holding read on security events for that repository, and
// answers null — not an empty connection — for everyone else, so its silence
// cannot be read as "this repository is clean".
//
// # Where the data comes from
//
// Nothing here is seeded. An advisory exists because somebody drafted and
// published one through the repository security-advisory API; an alert
// exists because a submitted dependency snapshot declares a package whose
// resolved version falls inside a published advisory's vulnerable range,
// under that ecosystem's own version ordering (see
// store.VersionInVulnerableRange). Both the manifests connection and the
// alerts connection therefore read the same dependency set the derivation
// matched against.

// advisorySchema holds the types this family builds. They are constructed
// once, in dependency order, inside addAdvisoryFieldsToSchema — no
// memoization is needed because nothing outside this file mints them and the
// assembly runs once per Resolver.
type advisorySchema struct {
	severityEnum       *graphql.Enum
	ecosystemEnum      *graphql.Enum
	identifierTypeEnum *graphql.Enum
	classificationEnum *graphql.Enum
	alertStateEnum     *graphql.Enum

	cvss                  *graphql.Object
	advisory              *graphql.Object
	advisoryConnection    *graphql.Object
	vulnerability         *graphql.Object
	vulnConnection        *graphql.Object
	alert                 *graphql.Object
	alertConnection       *graphql.Object
	manifest              *graphql.Object
	manifestConnection    *graphql.Object
	dependency            *graphql.Object
	dependencyConnection  *graphql.Object
	repositoryNode        *graphql.Interface
	dependabotUpdate      *graphql.Object
	cwe                   *graphql.Object
	cweConnection         *graphql.Object
	packageVersion        *graphql.Object
	advisoryPackage       *graphql.Object
	advisoryIdentifier    *graphql.Object
	advisoryReference     *graphql.Object
	epss                  *graphql.Object
	cvssSeverities        *graphql.Object
	dependabotUpdateError *graphql.Object

	// vulnerabilityOrder is minted once and shared: both Query and
	// SecurityAdvisory name SecurityVulnerabilityOrder, and a schema may hold
	// only one type per name.
	vulnerabilityOrder *graphql.InputObject
}

// addAdvisoryFieldsToSchema builds the advisory type graph, hangs the three
// repository fields off Repository, registers the three root fields and the
// dismissal mutation, and records the four Node implementors for
// Query.node dispatch.
//
// It must run after the pull-request family, because DependabotUpdate names
// PullRequest, and after the repository family, because four types name
// Repository.
func (s *Resolver) addAdvisoryFieldsToSchema(
	userType, repoType, mutationType, queryType *graphql.Object,
	nodeInterface *graphql.Interface,
	nodeTypes map[string]*graphql.Object,
) {
	dateTime := s.graphQLStringScalar("DateTime")
	uri := s.graphQLStringScalar("URI")
	types := &advisorySchema{}

	types.vulnerabilityOrder = s.securityVulnerabilityOrderInput()
	s.buildAdvisoryEnums(types)
	s.buildAdvisoryLeafTypes(types, uri)
	s.buildAdvisoryObject(types, dateTime, uri, nodeInterface)
	s.buildVulnerabilityObject(types, dateTime)
	s.buildDependencyGraphTypes(types, repoType, uri, nodeInterface)
	s.buildVulnerabilityAlertTypes(types, userType, repoType, dateTime, nodeInterface)

	s.addAdvisoryRootFields(queryType, types, dateTime)
	s.addRepositoryAdvisoryFields(repoType, types)
	s.addVulnerabilityAlertMutation(mutationType, types)

	nodeTypes["SecurityAdvisory"] = types.advisory
	nodeTypes["CWE"] = types.cwe
	nodeTypes["RepositoryVulnerabilityAlert"] = types.alert
	nodeTypes["DependencyGraphManifest"] = types.manifest
}

// buildAdvisoryEnums mints every enum the family names.
func (s *Resolver) buildAdvisoryEnums(types *advisorySchema) {
	types.severityEnum = s.graphQLEnum("SecurityAdvisorySeverity",
		"CRITICAL", "HIGH", "LOW", "MODERATE", "UNKNOWN")
	types.ecosystemEnum = s.graphQLEnum("SecurityAdvisoryEcosystem",
		"ACTIONS", "COMPOSER", "ERLANG", "GO", "MAVEN", "NPM", "NUGET",
		"PIP", "PUB", "RUBYGEMS", "RUST", "SWIFT")
	types.identifierTypeEnum = s.graphQLEnum("SecurityAdvisoryIdentifierType", "CVE", "GHSA")
	types.classificationEnum = s.graphQLEnum("SecurityAdvisoryClassification", "GENERAL", "MALWARE")
	types.alertStateEnum = s.graphQLEnum("RepositoryVulnerabilityAlertState",
		"AUTO_DISMISSED", "DISMISSED", "FIXED", "OPEN")
}

// buildAdvisoryLeafTypes mints the small value objects an advisory is
// composed of.
func (s *Resolver) buildAdvisoryLeafTypes(types *advisorySchema, uri *graphql.Scalar) {
	types.cvss = graphql.NewObject(graphql.ObjectConfig{
		Name: "CVSS",
		Fields: graphql.Fields{
			"score":        &graphql.Field{Type: graphql.NewNonNull(graphql.Float)},
			"vectorString": &graphql.Field{Type: graphql.String},
		},
	})
	types.cvssSeverities = graphql.NewObject(graphql.ObjectConfig{
		Name: "CvssSeverities",
		Fields: graphql.Fields{
			"cvssV3": &graphql.Field{Type: types.cvss},
			"cvssV4": &graphql.Field{Type: types.cvss},
		},
	})
	types.epss = graphql.NewObject(graphql.ObjectConfig{
		Name: "EPSS",
		Fields: graphql.Fields{
			"percentage": &graphql.Field{Type: graphql.Float},
			"percentile": &graphql.Field{Type: graphql.Float},
		},
	})
	types.advisoryIdentifier = graphql.NewObject(graphql.ObjectConfig{
		Name: "SecurityAdvisoryIdentifier",
		Fields: graphql.Fields{
			"type":  &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"value": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	types.advisoryReference = graphql.NewObject(graphql.ObjectConfig{
		Name: "SecurityAdvisoryReference",
		Fields: graphql.Fields{
			"url": &graphql.Field{Type: graphql.NewNonNull(uri)},
		},
	})
	types.advisoryPackage = graphql.NewObject(graphql.ObjectConfig{
		Name: "SecurityAdvisoryPackage",
		Fields: graphql.Fields{
			"ecosystem": &graphql.Field{Type: graphql.NewNonNull(types.ecosystemEnum)},
			"name":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	types.packageVersion = graphql.NewObject(graphql.ObjectConfig{
		Name: "SecurityAdvisoryPackageVersion",
		Fields: graphql.Fields{
			"identifier": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
}

// buildAdvisoryObject mints CWE, SecurityAdvisory and their connections.
func (s *Resolver) buildAdvisoryObject(types *advisorySchema, dateTime *graphql.Scalar, uri *graphql.Scalar, nodeInterface *graphql.Interface) {
	types.cwe = graphql.NewObject(graphql.ObjectConfig{
		Name:       "CWE",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":          &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"cweId":       &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"name":        &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	types.cweConnection = advisoryConnectionType("CWE", types.cwe, s.gqlPageInfoType())

	types.advisory = graphql.NewObject(graphql.ObjectConfig{
		Name:       "SecurityAdvisory",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":                     &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"databaseId":             &graphql.Field{Type: graphql.Int},
			"ghsaId":                 &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"summary":                &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"description":            &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"severity":               &graphql.Field{Type: graphql.NewNonNull(types.severityEnum)},
			"classification":         &graphql.Field{Type: graphql.NewNonNull(types.classificationEnum)},
			"origin":                 &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"publishedAt":            &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"updatedAt":              &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"withdrawnAt":            &graphql.Field{Type: dateTime},
			"permalink":              &graphql.Field{Type: uri},
			"notificationsPermalink": &graphql.Field{Type: uri},
			"cvss":                   &graphql.Field{Type: graphql.NewNonNull(types.cvss)},
			"cvssSeverities":         &graphql.Field{Type: graphql.NewNonNull(types.cvssSeverities)},
			"epss":                   &graphql.Field{Type: types.epss},
			"identifiers": &graphql.Field{
				Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(types.advisoryIdentifier))),
			},
			"references": &graphql.Field{
				Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(types.advisoryReference))),
			},
			"cwes": &graphql.Field{
				Type: graphql.NewNonNull(types.cweConnection),
				Args: relayArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return paginateGQLMaps(advisorySourceNodes(p.Source, "_cwes"), p.Args), nil
				},
			},
		},
	})
	types.advisoryConnection = advisoryConnectionType("SecurityAdvisory", types.advisory, s.gqlPageInfoType())
}

// buildVulnerabilityObject mints SecurityVulnerability, whose advisory field
// closes the cycle back onto SecurityAdvisory, and then adds the advisory's
// own vulnerabilities connection.
func (s *Resolver) buildVulnerabilityObject(types *advisorySchema, dateTime *graphql.Scalar) {
	types.vulnerability = graphql.NewObject(graphql.ObjectConfig{
		Name: "SecurityVulnerability",
		Fields: graphql.Fields{
			// advisory is resolved on demand from the vulnerability's GHSA id
			// rather than carried as an embedded map. Embedding it made the
			// two source maps mutually referential — the advisory holds its
			// vulnerabilities, each of which held the advisory — and any %v
			// or reflective walk over one of those values recurses until the
			// stack is gone. Resolving it here also means the advisory is
			// rendered only when the field is actually selected.
			"advisory": &graphql.Field{
				Type:    graphql.NewNonNull(types.advisory),
				Resolve: s.resolveVulnerabilityAdvisory,
			},
			"package":                &graphql.Field{Type: graphql.NewNonNull(types.advisoryPackage)},
			"severity":               &graphql.Field{Type: graphql.NewNonNull(types.severityEnum)},
			"updatedAt":              &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"vulnerableVersionRange": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"firstPatchedVersion":    &graphql.Field{Type: types.packageVersion},
		},
	})
	types.vulnConnection = advisoryConnectionType("SecurityVulnerability", types.vulnerability, s.gqlPageInfoType())

	// The advisory's own vulnerabilities connection is added after the
	// vulnerability type exists, which is the cycle graphql-go cannot express
	// through a field map literal.
	types.advisory.AddFieldConfig("vulnerabilities", &graphql.Field{
		Type: graphql.NewNonNull(types.vulnConnection),
		Args: relayArgs(graphql.FieldConfigArgument{
			"ecosystem":       &graphql.ArgumentConfig{Type: types.ecosystemEnum},
			"package":         &graphql.ArgumentConfig{Type: graphql.String},
			"severities":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.severityEnum))},
			"classifications": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.classificationEnum))},
			"orderBy":         &graphql.ArgumentConfig{Type: types.vulnerabilityOrder},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			nodes := filterVulnerabilityNodes(advisorySourceNodes(p.Source, "_vulnerabilities"), p.Args)
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// buildDependencyGraphTypes mints the RepositoryNode interface,
// DependencyGraphDependency and DependencyGraphManifest with their
// connections.
func (s *Resolver) buildDependencyGraphTypes(types *advisorySchema, repoType *graphql.Object, uri *graphql.Scalar, nodeInterface *graphql.Interface) {
	// RepositoryNode is declared here because the two types that claim it in
	// this build are declared here. graphql-go reads an object's interface
	// list once at construction, so the interface has to exist before either.
	types.repositoryNode = graphql.NewInterface(graphql.InterfaceConfig{
		Name: "RepositoryNode",
		Fields: graphql.Fields{
			"repository": &graphql.Field{Type: graphql.NewNonNull(repoType)},
		},
		ResolveType: func(p graphql.ResolveTypeParams) *graphql.Object {
			source, _ := p.Value.(map[string]interface{})
			switch source["__typename"] {
			case "DependabotUpdate":
				return types.dependabotUpdate
			default:
				return types.alert
			}
		},
	})

	types.dependency = graphql.NewObject(graphql.ObjectConfig{
		Name: "DependencyGraphDependency",
		Fields: graphql.Fields{
			"packageName":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"packageManager":  &graphql.Field{Type: graphql.String},
			"packageUrl":      &graphql.Field{Type: uri},
			"relationship":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"requirements":    &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"hasDependencies": &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"repository":      &graphql.Field{Type: repoType},
			"packageLabel": &graphql.Field{
				Type: graphql.NewNonNull(graphql.String),
				DeprecationReason: "`packageLabel` will be removed. Use normalized `packageName` " +
					"field instead. Removal on 2022-10-01 UTC.",
			},
		},
	})
	types.dependencyConnection = advisoryConnectionType("DependencyGraphDependency", types.dependency, s.gqlPageInfoType())

	types.manifest = graphql.NewObject(graphql.ObjectConfig{
		Name:       "DependencyGraphManifest",
		Interfaces: []*graphql.Interface{nodeInterface},
		Fields: graphql.Fields{
			"id":                &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"filename":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"blobPath":          &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"parseable":         &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"exceedsMaxSize":    &graphql.Field{Type: graphql.NewNonNull(graphql.Boolean)},
			"dependenciesCount": &graphql.Field{Type: graphql.Int},
			"repository":        &graphql.Field{Type: graphql.NewNonNull(repoType)},
			"dependencies": &graphql.Field{
				Type: types.dependencyConnection,
				Args: relayArgs(nil),
				Resolve: func(p graphql.ResolveParams) (interface{}, error) {
					return paginateGQLMaps(advisorySourceNodes(p.Source, "_dependencies"), p.Args), nil
				},
			},
		},
	})
	types.manifestConnection = advisoryConnectionType("DependencyGraphManifest", types.manifest, s.gqlPageInfoType())
}

// buildVulnerabilityAlertTypes mints DependabotUpdate and
// RepositoryVulnerabilityAlert with its connection.
func (s *Resolver) buildVulnerabilityAlertTypes(types *advisorySchema, userType, repoType *graphql.Object, dateTime *graphql.Scalar, nodeInterface *graphql.Interface) {
	types.dependabotUpdateError = graphql.NewObject(graphql.ObjectConfig{
		Name: "DependabotUpdateError",
		Fields: graphql.Fields{
			"body":      &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"errorType": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"title":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	types.dependabotUpdate = graphql.NewObject(graphql.ObjectConfig{
		Name:       "DependabotUpdate",
		Interfaces: []*graphql.Interface{types.repositoryNode},
		Fields: graphql.Fields{
			"error":       &graphql.Field{Type: types.dependabotUpdateError},
			"pullRequest": &graphql.Field{Type: s.graphqlTypes.pullRequest},
			"repository":  &graphql.Field{Type: graphql.NewNonNull(repoType)},
		},
	})

	dependencyScopeEnum := s.graphQLEnum("RepositoryVulnerabilityAlertDependencyScope", "DEVELOPMENT", "RUNTIME")
	dependencyRelationshipEnum := s.graphQLEnum("RepositoryVulnerabilityAlertDependencyRelationship",
		"DIRECT", "INCONCLUSIVE", "TRANSITIVE", "UNKNOWN")

	types.alert = graphql.NewObject(graphql.ObjectConfig{
		Name:       "RepositoryVulnerabilityAlert",
		Interfaces: []*graphql.Interface{nodeInterface, types.repositoryNode},
		Fields: graphql.Fields{
			"id":                         &graphql.Field{Type: graphql.NewNonNull(graphql.ID)},
			"number":                     &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"state":                      &graphql.Field{Type: graphql.NewNonNull(types.alertStateEnum)},
			"createdAt":                  &graphql.Field{Type: graphql.NewNonNull(dateTime)},
			"dismissedAt":                &graphql.Field{Type: dateTime},
			"autoDismissedAt":            &graphql.Field{Type: dateTime},
			"fixedAt":                    &graphql.Field{Type: dateTime},
			"dismissReason":              &graphql.Field{Type: graphql.String},
			"dismissComment":             &graphql.Field{Type: graphql.String},
			"dismisser":                  &graphql.Field{Type: userType},
			"repository":                 &graphql.Field{Type: graphql.NewNonNull(repoType)},
			"securityAdvisory":           &graphql.Field{Type: types.advisory},
			"securityVulnerability":      &graphql.Field{Type: types.vulnerability},
			"vulnerableManifestFilename": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"vulnerableManifestPath":     &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
			"vulnerableRequirements":     &graphql.Field{Type: graphql.String},
			"dependencyScope":            &graphql.Field{Type: dependencyScopeEnum},
			"dependencyRelationship":     &graphql.Field{Type: dependencyRelationshipEnum},
			"dependabotUpdate":           &graphql.Field{Type: types.dependabotUpdate},
		},
	})
	types.alertConnection = advisoryConnectionType("RepositoryVulnerabilityAlert", types.alert, s.gqlPageInfoType())
}

// ---------------------------------------------------------------------------
// Root fields
// ---------------------------------------------------------------------------

// addAdvisoryRootFields registers securityAdvisory, securityAdvisories and
// securityVulnerabilities. All three read the public advisory database, so
// none takes a viewer.
func (s *Resolver) addAdvisoryRootFields(queryType *graphql.Object, types *advisorySchema, dateTime *graphql.Scalar) {
	identifierFilter := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "SecurityAdvisoryIdentifierFilter",
		Fields: graphql.InputObjectConfigFieldMap{
			"type":  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(types.identifierTypeEnum)},
			"value": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		},
	})

	queryType.AddFieldConfig("securityAdvisory", &graphql.Field{
		Type: types.advisory,
		Args: graphql.FieldConfigArgument{
			"ghsaId": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ghsaID, _ := p.Args["ghsaId"].(string)
			advisory := s.store.GetGlobalAdvisoryByGHSA(ghsaID)
			if advisory == nil {
				// A withdrawn advisory is still addressable — the lookup above
				// finds it — so this is genuinely "no such published
				// advisory", which is the nullable field's honest answer.
				return nil, nil
			}
			return s.advisoryToGQL(advisory), nil
		},
	})

	queryType.AddFieldConfig("securityAdvisories", &graphql.Field{
		Type: graphql.NewNonNull(types.advisoryConnection),
		Args: relayArgs(graphql.FieldConfigArgument{
			"identifier":      &graphql.ArgumentConfig{Type: identifierFilter},
			"classifications": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.classificationEnum))},
			"publishedSince":  &graphql.ArgumentConfig{Type: dateTime},
			"updatedSince":    &graphql.ArgumentConfig{Type: dateTime},
			"epssPercentage":  &graphql.ArgumentConfig{Type: graphql.Float},
			"epssPercentile":  &graphql.ArgumentConfig{Type: graphql.Float},
			"orderBy":         &graphql.ArgumentConfig{Type: s.securityAdvisoryOrderInput()},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			advisories := s.store.ListGlobalAdvisoriesFiltered(advisoryFilterFromArgs(p.Args))
			sortAdvisoriesForArgs(advisories, p.Args)
			nodes := make([]map[string]interface{}, 0, len(advisories))
			for _, advisory := range advisories {
				nodes = append(nodes, s.advisoryToGQL(advisory))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	queryType.AddFieldConfig("securityVulnerabilities", &graphql.Field{
		Type: graphql.NewNonNull(types.vulnConnection),
		Args: relayArgs(graphql.FieldConfigArgument{
			"ecosystem":       &graphql.ArgumentConfig{Type: types.ecosystemEnum},
			"package":         &graphql.ArgumentConfig{Type: graphql.String},
			"severities":      &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.severityEnum))},
			"classifications": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.classificationEnum))},
			"orderBy":         &graphql.ArgumentConfig{Type: types.vulnerabilityOrder},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			filter := advisoryFilterFromArgs(p.Args)
			pairs := s.store.ListGlobalVulnerabilities(filter)
			nodes := make([]map[string]interface{}, 0, len(pairs))
			for _, pair := range pairs {
				nodes = append(nodes, s.vulnerabilityToGQL(pair.Advisory, pair.Vulnerability))
			}
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// securityAdvisoryOrderInput is the SecurityAdvisoryOrder input object.
func (s *Resolver) securityAdvisoryOrderInput() *graphql.InputObject {
	return graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "SecurityAdvisoryOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC")),
			},
			"field": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("SecurityAdvisoryOrderField",
					"EPSS_PERCENTAGE", "EPSS_PERCENTILE", "PUBLISHED_AT", "UPDATED_AT")),
			},
		},
	})
}

// securityVulnerabilityOrderInput mints the SecurityVulnerabilityOrder input
// object.
func (s *Resolver) securityVulnerabilityOrderInput() *graphql.InputObject {
	return graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "SecurityVulnerabilityOrder",
		Fields: graphql.InputObjectConfigFieldMap{
			"direction": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("OrderDirection", "ASC", "DESC")),
			},
			"field": &graphql.InputObjectFieldConfig{
				Type: graphql.NewNonNull(s.graphQLEnum("SecurityVulnerabilityOrderField", "UPDATED_AT")),
			},
		},
	})
}

// ---------------------------------------------------------------------------
// Repository fields
// ---------------------------------------------------------------------------

// addRepositoryAdvisoryFields hangs hasVulnerabilityAlertsEnabled,
// vulnerabilityAlerts and dependencyGraphManifests off Repository.
func (s *Resolver) addRepositoryAdvisoryFields(repoType *graphql.Object, types *advisorySchema) {
	repoType.AddFieldConfig("hasVulnerabilityAlertsEnabled", &graphql.Field{
		Type: graphql.NewNonNull(graphql.Boolean),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromAdvisorySource(p)
			if err != nil {
				return nil, err
			}
			if repo == nil {
				return false, nil
			}
			return repo.VulnerabilityAlertsEnabled, nil
		},
	})

	repoType.AddFieldConfig("vulnerabilityAlerts", &graphql.Field{
		Type: types.alertConnection,
		Args: relayArgs(graphql.FieldConfigArgument{
			"states":           &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.alertStateEnum))},
			"dependencyScopes": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(s.graphQLEnum("RepositoryVulnerabilityAlertDependencyScope", "DEVELOPMENT", "RUNTIME")))},
			"classifications":  &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(types.classificationEnum))},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromAdvisorySource(p)
			if err != nil {
				return nil, err
			}
			// An alert names a vulnerable dependency this repository is
			// running. Answering an empty connection to a viewer without
			// security access would report that as "no alerts"; the field is
			// nullable so that the two can be told apart.
			//
			// The predicate is viewerMayActOnRepo rather than
			// viewerHasRepoPermission because the latter is satisfied by mere
			// readability, which every account has on a public repository —
			// so it published the vulnerable dependency versions of every
			// public repository on the instance. Push standing is what GitHub
			// requires, and it is what the REST alert routes now require too.
			if repo == nil || !s.viewerHasRepoSecurityAccess(p.Context, repo) {
				return nil, nil
			}
			nodes := s.vulnerabilityAlertNodes(repo, p.Args)
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})

	repoType.AddFieldConfig("dependencyGraphManifests", &graphql.Field{
		Type: types.manifestConnection,
		Args: relayArgs(graphql.FieldConfigArgument{
			"withDependencies":  &graphql.ArgumentConfig{Type: graphql.Boolean},
			"dependenciesFirst": &graphql.ArgumentConfig{Type: graphql.Int},
			"dependenciesAfter": &graphql.ArgumentConfig{Type: graphql.String},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			repo, err := s.repoFromAdvisorySource(p)
			if err != nil {
				return nil, err
			}
			// The dependency graph is as readable as the repository's code:
			// a viewer who can read the contents can read what they depend on.
			if repo == nil || !s.viewerCanReadRepo(p.Context, repo) {
				return nil, nil
			}
			nodes := s.dependencyManifestNodes(repo, p.Args)
			return paginateGQLMaps(nodes, p.Args), nil
		},
	})
}

// viewerHasRepoSecurityAccess is the resolver layer's copy of the question
// the REST security routes ask: may this viewer read the repository's
// security findings? Readability is not enough — a public repository is
// readable by everyone, and its alerts are not.
func (s *Resolver) viewerHasRepoSecurityAccess(ctx context.Context, repo *store.Repo) bool {
	return s.viewerMayActOnRepo(ctx, repo, store.ScopeSecurityEvents, store.PermRead, store.PermWrite)
}

// repoFromAdvisorySource resolves the *store.Repo behind a Repository source
// map. It returns a nil repo (not an error) when the source names a
// repository the store no longer holds, so a field can answer null rather
// than fail the whole query.
func (s *Resolver) repoFromAdvisorySource(p graphql.ResolveParams) (*store.Repo, error) {
	source, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("resolve source: unexpected type %T", p.Source)
	}
	id, ok := source["databaseId"].(int)
	if !ok {
		return nil, fmt.Errorf("repository source missing databaseId")
	}
	return s.store.GetRepoByID(id), nil
}

// ---------------------------------------------------------------------------
// Mutation
// ---------------------------------------------------------------------------

// addVulnerabilityAlertMutation registers
// dismissRepositoryVulnerabilityAlert.
func (s *Resolver) addVulnerabilityAlertMutation(mutationType *graphql.Object, types *advisorySchema) {
	dismissReasonEnum := s.graphQLEnum("DismissReason",
		"FIX_STARTED", "INACCURATE", "NOT_USED", "NO_BANDWIDTH", "TOLERABLE_RISK")

	inputType := graphql.NewInputObject(graphql.InputObjectConfig{
		Name: "DismissRepositoryVulnerabilityAlertInput",
		Fields: graphql.InputObjectConfigFieldMap{
			"repositoryVulnerabilityAlertId": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.ID)},
			"dismissReason":                  &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(dismissReasonEnum)},
		},
	})
	payloadType := graphql.NewObject(graphql.ObjectConfig{
		Name: "DismissRepositoryVulnerabilityAlertPayload",
		Fields: graphql.Fields{
			"repositoryVulnerabilityAlert": &graphql.Field{Type: types.alert},
		},
	})

	s.registerMutation(mutationType, "dismissRepositoryVulnerabilityAlert", &graphql.Field{
		Type: payloadType,
		Args: graphql.FieldConfigArgument{
			"input": &graphql.ArgumentConfig{Type: graphql.NewNonNull(inputType)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			input, _ := p.Args["input"].(map[string]interface{})
			nodeID, _ := input["repositoryVulnerabilityAlertId"].(string)
			rawReason, _ := input["dismissReason"].(string)
			reason, ok := store.DependabotDismissReasonFromGraphQL(rawReason)
			if !ok {
				return nil, fmt.Errorf("dismissReason %q is not a DismissReason", rawReason)
			}
			// The policy row already proved the viewer may act on the alert's
			// repository; re-reading the alert here is the lookup, not a
			// second authorization decision.
			alert := s.store.LookupDependabotAlertByNodeID(nodeID)
			if alert == nil {
				return nil, gqlMissingNode("RepositoryVulnerabilityAlert", nodeID)
			}
			viewer := s.ghUserFromContext(p.Context)
			if err := s.store.UpdateDependabotAlert(alert, "dismissed", reason, "", viewer); err != nil {
				return nil, err
			}
			repo := s.store.GetRepoByFullName(alert.RepoKey)
			s.emitDependabotAlertEvent(repo, alert, viewer, "dismissed")
			return map[string]interface{}{
				"repositoryVulnerabilityAlert": optionalObject(s.vulnerabilityAlertToGQL(repo, alert)),
			}, nil
		},
	})
}

// mutationTargetVulnerabilityAlert resolves the repository a
// RepositoryVulnerabilityAlert node id belongs to, for the mutation policy
// table.
//
// The alert carries no author, so there is no author exemption to grant: the
// person who has a vulnerable dependency is not thereby entitled to dismiss
// the alert about it.
func mutationTargetVulnerabilityAlert(key string) func(*Resolver, map[string]interface{}) mutationTarget {
	return func(s *Resolver, input map[string]interface{}) mutationTarget {
		nodeID, _ := input[key].(string)
		target := mutationTarget{missing: gqlMissingNode("RepositoryVulnerabilityAlert", nodeID)}
		if alert := s.store.LookupDependabotAlertByNodeID(nodeID); alert != nil {
			target.repo = s.store.GetRepoByFullName(alert.RepoKey)
		}
		return target
	}
}

// emitDependabotAlertEvent delivers a dependabot_alert webhook for an alert
// a GraphQL mutation moved, through the same payload builder the REST alert
// routes use.
func (s *Resolver) emitDependabotAlertEvent(repo *store.Repo, alert *store.DependabotAlert, sender *store.User, action string) {
	if repo == nil || alert == nil {
		return
	}
	var dismisser map[string]interface{}
	if alert.DismissedByLogin != "" {
		if user := s.store.LookupUserByLogin(alert.DismissedByLogin); user != nil {
			dismisser = s.senderPayload(user)
		}
	}
	var senderJSON map[string]interface{}
	if sender != nil {
		senderJSON = s.senderPayload(sender)
	}
	payload := store.DependabotAlertEventPayload(alert, s.repoPayload(repo), senderJSON, dismisser, action)
	s.emitWebhookEvent(repo.FullName, "dependabot_alert", action, payload)
}

// ---------------------------------------------------------------------------
// Renderers
// ---------------------------------------------------------------------------

// advisoryToGQL renders one advisory as its SecurityAdvisory source map. The
// private "_cwes" and "_vulnerabilities" keys carry the connection members;
// the schema exposes neither.
func (s *Resolver) advisoryToGQL(advisory *store.SecurityAdvisory) map[string]interface{} {
	if advisory == nil {
		return nil
	}
	identifiers := []map[string]interface{}{
		{"type": "GHSA", "value": advisory.GHSAID},
	}
	if advisory.CVEID != "" {
		identifiers = append(identifiers, map[string]interface{}{"type": "CVE", "value": advisory.CVEID})
	}

	cwes := make([]map[string]interface{}, 0, len(advisory.CWEs))
	for _, cwe := range advisory.CWEs {
		identifier := store.NormalizeCWEID(cwe)
		cwes = append(cwes, map[string]interface{}{
			"__typename": "CWE",
			"id":         store.CWENodeID(identifier),
			"cweId":      identifier,
			// This instance records a CWE's identifier and nothing else, so
			// the identifier is the whole of what it can name it. The fields
			// are non-null in GitHub's schema; reporting the identifier and
			// an empty description is what it actually knows.
			"name":        identifier,
			"description": "",
		})
	}

	// CVSS.score is non-null in GitHub's schema. An advisory authored with a
	// vector and no explicit score is scored from that vector rather than
	// reported as 0.0, which would read as "no risk" for a vector that says
	// the opposite.
	cvssScore, _ := store.AdvisoryCVSSScore(advisory)
	cvss := map[string]interface{}{
		"score":        cvssScore,
		"vectorString": nullOrEmptyString(advisory.CVSSVector),
	}
	permalink := externalURL("/advisories/" + advisory.GHSAID)

	rendered := map[string]interface{}{
		// SecurityAdvisory, CWE, RepositoryVulnerabilityAlert and
		// DependencyGraphManifest all implement Node, and the Node interface
		// dispatches on this discriminator. Without it Query.node resolves
		// the value to no concrete type and fails the field.
		"__typename":             "SecurityAdvisory",
		"id":                     advisory.NodeID,
		"databaseId":             advisory.ID,
		"ghsaId":                 advisory.GHSAID,
		"summary":                advisory.Summary,
		"description":            advisory.Description,
		"severity":               advisorySeverityEnum(advisory.Severity),
		"classification":         "GENERAL",
		"origin":                 "UNSPECIFIED",
		"publishedAt":            formatAdvisoryTime(advisory.PublishedAt),
		"updatedAt":              advisory.UpdatedAt.UTC().Format(time.RFC3339),
		"withdrawnAt":            formatAdvisoryTime(store.AdvisoryWithdrawnAt(advisory)),
		"permalink":              permalink,
		"notificationsPermalink": permalink + "/dependabot",
		"cvss":                   cvss,
		"cvssSeverities":         map[string]interface{}{"cvssV3": cvss, "cvssV4": nil},
		// EPSS is a third-party exploit-probability score this instance does
		// not compute and has no source for, so the nullable field is null.
		"epss":        nil,
		"identifiers": identifiers,
		// References are URLs an advisory cites. Neither the advisory
		// creation nor the update contract carries any, so an advisory
		// published here genuinely has none.
		"references": []map[string]interface{}{},
		"_cwes":      cwes,
	}

	vulnerabilities := make([]map[string]interface{}, 0, len(advisory.Vulnerabilities))
	for _, vulnerability := range advisory.Vulnerabilities {
		vulnerabilities = append(vulnerabilities, advisoryVulnerabilityToGQL(advisory, vulnerability))
	}
	rendered["_vulnerabilities"] = vulnerabilities
	return rendered
}

// vulnerabilityToGQL renders one advisory/vulnerability pair.
func (s *Resolver) vulnerabilityToGQL(advisory *store.SecurityAdvisory, vulnerability store.SecurityAdvisoryVulnerability) map[string]interface{} {
	return advisoryVulnerabilityToGQL(advisory, vulnerability)
}

// resolveVulnerabilityAdvisory renders the advisory a SecurityVulnerability
// belongs to, looked up by the GHSA id the vulnerability source carries.
func (s *Resolver) resolveVulnerabilityAdvisory(p graphql.ResolveParams) (interface{}, error) {
	source, ok := p.Source.(map[string]interface{})
	if !ok {
		return nil, fmt.Errorf("vulnerability source: unexpected type %T", p.Source)
	}
	ghsaID, _ := source["_advisoryGHSA"].(string)
	advisory := s.store.GetGlobalAdvisoryByGHSA(ghsaID)
	if advisory == nil {
		// SecurityVulnerability.advisory is non-null, so a vulnerability whose
		// advisory has vanished cannot be rendered at all; saying so is better
		// than completing the field with an empty object.
		return nil, fmt.Errorf("advisory %s is no longer published", ghsaID)
	}
	return s.advisoryToGQL(advisory), nil
}

// advisoryVulnerabilityToGQL renders a SecurityVulnerability. The parent
// advisory is referenced by GHSA id, not embedded: see the advisory field's
// resolver for why.
func advisoryVulnerabilityToGQL(advisory *store.SecurityAdvisory, vulnerability store.SecurityAdvisoryVulnerability) map[string]interface{} {
	firstPatched := interface{}(nil)
	if vulnerability.FirstPatchedVersion != "" {
		firstPatched = map[string]interface{}{"identifier": vulnerability.FirstPatchedVersion}
	}
	// SecurityAdvisoryPackage.ecosystem is non-null, so a vulnerability whose
	// ecosystem is outside the enum cannot be rendered as a package at all —
	// but `package` is non-null too, which means such a vulnerability simply
	// is not a SecurityVulnerability. The ecosystem is carried privately so
	// the caller can drop it rather than emit an invalid member.
	ecosystem := store.AdvisoryEcosystemGraphQL(vulnerability.PackageEcosystem)
	return map[string]interface{}{
		"_advisoryGHSA": advisory.GHSAID,
		"package": map[string]interface{}{
			"ecosystem": ecosystem,
			"name":      vulnerability.PackageName,
		},
		"severity":               advisorySeverityEnum(advisory.Severity),
		"updatedAt":              advisory.UpdatedAt.UTC().Format(time.RFC3339),
		"vulnerableVersionRange": vulnerability.VulnerableVersionRange,
		"firstPatchedVersion":    firstPatched,
		"_ecosystem":             ecosystem,
		"_packageName":           vulnerability.PackageName,
		"id":                     advisory.GHSAID + "\x1f" + vulnerability.PackageEcosystem + "\x1f" + vulnerability.PackageName,
	}
}

// vulnerabilityAlertNodes renders a repository's Dependabot alerts as
// RepositoryVulnerabilityAlert source maps, honoring the connection's
// states and dependencyScopes filters.
func (s *Resolver) vulnerabilityAlertNodes(repo *store.Repo, args map[string]interface{}) []map[string]interface{} {
	wantStates := enumArgSet(args, "states")
	wantScopes := enumArgSet(args, "dependencyScopes")

	alerts := s.store.ListDependabotAlerts(repo.FullName, "", "", "", "", "", "created", "desc")
	nodes := make([]map[string]interface{}, 0, len(alerts))
	for _, alert := range alerts {
		if len(wantStates) != 0 && !wantStates[store.DependabotAlertGraphQLState(alert.State)] {
			continue
		}
		node := s.vulnerabilityAlertToGQL(repo, alert)
		if len(wantScopes) != 0 {
			scope, _ := node["dependencyScope"].(string)
			if scope == "" || !wantScopes[scope] {
				continue
			}
		}
		nodes = append(nodes, node)
	}
	return nodes
}

// vulnerabilityAlertToGQL renders one Dependabot alert as a
// RepositoryVulnerabilityAlert source map.
func (s *Resolver) vulnerabilityAlertToGQL(repo *store.Repo, alert *store.DependabotAlert) map[string]interface{} {
	if alert == nil {
		return nil
	}
	var repository map[string]interface{}
	if repo != nil {
		repository = repoToGraphQL(s.store, s.store.SnapRepo(repo))
	}

	var dismisser map[string]interface{}
	if alert.DismissedByLogin != "" {
		if user := s.store.LookupUserByLogin(alert.DismissedByLogin); user != nil {
			dismisser = userToGraphQL(user)
		}
	}

	// The advisory an alert points at may have been withdrawn, or may never
	// have been published on this instance at all if the alert outlived it.
	// Both fields are nullable for exactly that case.
	var advisoryNode, vulnerabilityNode map[string]interface{}
	if advisory := s.store.GetGlobalAdvisoryByGHSA(alert.VulnerabilityID); advisory != nil {
		advisoryNode = s.advisoryToGQL(advisory)
		for _, vulnerability := range advisory.Vulnerabilities {
			if !strings.EqualFold(vulnerability.PackageName, alert.PackageName) {
				continue
			}
			if store.NormalizeAdvisoryEcosystem(vulnerability.PackageEcosystem) !=
				store.NormalizeAdvisoryEcosystem(alert.PackageEcosystem) {
				continue
			}
			vulnerabilityNode = advisoryVulnerabilityToGQL(advisory, vulnerability)
			break
		}
	}

	node := map[string]interface{}{
		"id":              alert.NodeID,
		"number":          alert.Number,
		"state":           store.DependabotAlertGraphQLState(alert.State),
		"createdAt":       alert.CreatedAt.UTC().Format(time.RFC3339),
		"dismissedAt":     formatAdvisoryTime(alert.DismissedAt),
		"autoDismissedAt": formatAdvisoryTime(alert.AutoDismissedAt),
		"fixedAt":         formatAdvisoryTime(alert.FixedAt),
		"dismissReason":   nullOrEmptyString(store.DependabotDismissReasonText(alert.DismissedReason)),
		"dismissComment":  nullOrEmptyString(alert.DismissedComment),
		// These four are written through nullOrMap rather than directly: a
		// nil Go map boxed into an interface{} is not a nil interface, and
		// graphql-go reads the non-nil box as an object present — then fails
		// the whole query on the first non-nullable field inside the absent
		// object (an alert nobody dismissed reported "cannot return null for
		// User.login").
		"dismisser":                  optionalObject(dismisser),
		"repository":                 optionalObject(repository),
		"securityAdvisory":           optionalObject(advisoryNode),
		"securityVulnerability":      optionalObject(vulnerabilityNode),
		"vulnerableManifestPath":     alert.ManifestPath,
		"vulnerableManifestFilename": store.DependabotAlertManifestFilename(alert.ManifestPath),
		// Dependabot on this instance raises alerts; it does not open update
		// pull requests, so there is no update to report rather than an
		// empty one.
		"dependabotUpdate":       nil,
		"vulnerableRequirements": nil,
		"dependencyScope":        nil,
		"dependencyRelationship": nil,
		"__typename":             "RepositoryVulnerabilityAlert",
	}

	// Scope and relationship are properties of the manifest entry, not of the
	// alert, so they are read from the repository's current dependency set.
	// An alert whose dependency has since been removed reports null for both
	// rather than the stale answer.
	if repo != nil {
		dependency, found := s.store.LookupResolvedDependency(
			repo.ID, "refs/heads/"+repo.DefaultBranch,
			alert.ManifestPath, alert.PackageEcosystem, alert.PackageName)
		if found {
			node["dependencyScope"] = dependencyScopeEnum(dependency.Scope)
			node["dependencyRelationship"] = dependencyRelationshipEnum(dependency.Relationship)
			if dependency.Version != "" {
				node["vulnerableRequirements"] = "= " + dependency.Version
			}
		}
	}
	return node
}

// dependencyManifestNodes renders a repository's current dependency
// manifests as DependencyGraphManifest source maps.
func (s *Resolver) dependencyManifestNodes(repo *store.Repo, args map[string]interface{}) []map[string]interface{} {
	repository := repoToGraphQL(s.store, s.store.SnapRepo(repo))
	manifests := s.store.ResolvedDependencyManifests(repo.ID, "refs/heads/"+repo.DefaultBranch, "")

	// withDependencies: false asks for the manifests without their members,
	// which is how a client counts manifests without paying for every
	// dependency in the repository.
	includeDependencies := true
	if want, ok := args["withDependencies"].(bool); ok {
		includeDependencies = want
	}

	nodes := make([]map[string]interface{}, 0, len(manifests))
	for _, manifest := range manifests {
		dependencies := make([]map[string]interface{}, 0, len(manifest.Dependencies))
		if includeDependencies {
			for _, dependency := range manifest.Dependencies {
				dependencies = append(dependencies, dependencyToGQL(dependency))
			}
		}
		nodes = append(nodes, map[string]interface{}{
			"__typename": "DependencyGraphManifest",
			"id":         store.DependencyGraphManifestNodeID(repo.ID, manifest.Name),
			"filename":   manifest.Name,
			"blobPath":   "/" + repo.FullName + "/blob/" + repo.DefaultBranch + "/" + manifest.Name,
			// A manifest reached this view by being accepted through the
			// dependency submission API, which parses it before storing it.
			"parseable":         true,
			"exceedsMaxSize":    false,
			"dependenciesCount": len(manifest.Dependencies),
			"repository":        repository,
			"_dependencies":     dependencies,
		})
	}
	return nodes
}

// dependencyToGQL renders one resolved dependency.
func dependencyToGQL(dependency store.ResolvedDependency) map[string]interface{} {
	requirements := ""
	if dependency.Version != "" {
		requirements = "= " + dependency.Version
	}
	return map[string]interface{}{
		"id":              dependency.PackageURL,
		"packageName":     dependency.Name,
		"packageLabel":    dependency.Name,
		"packageManager":  nullOrEmptyString(store.DependencyPackageManager(dependency.Ecosystem)),
		"packageUrl":      nullOrEmptyString(dependency.PackageURL),
		"relationship":    dependencyRelationshipString(dependency.Relationship),
		"requirements":    requirements,
		"hasDependencies": len(dependency.DependsOn) != 0,
		// The package's own repository would be the upstream project's, which
		// this instance does not host and cannot invent a link to.
		"repository": nil,
	}
}

// ---------------------------------------------------------------------------
// Argument and value helpers
// ---------------------------------------------------------------------------

// relayArgs returns the four Relay connection arguments merged with any
// field-specific ones, so no connection can accidentally omit one.
func relayArgs(extra graphql.FieldConfigArgument) graphql.FieldConfigArgument {
	args := graphql.FieldConfigArgument{
		"first":  &graphql.ArgumentConfig{Type: graphql.Int},
		"after":  &graphql.ArgumentConfig{Type: graphql.String},
		"last":   &graphql.ArgumentConfig{Type: graphql.Int},
		"before": &graphql.ArgumentConfig{Type: graphql.String},
	}
	for name, config := range extra {
		args[name] = config
	}
	return args
}

// advisoryConnectionType builds the Relay connection and edge objects for one
// advisory-family node type.
func advisoryConnectionType(name string, nodeType *graphql.Object, pageInfo *graphql.Object) *graphql.Object {
	edgeType := graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Edge",
		Fields: graphql.Fields{
			"node":   &graphql.Field{Type: nodeType},
			"cursor": &graphql.Field{Type: graphql.NewNonNull(graphql.String)},
		},
	})
	return graphql.NewObject(graphql.ObjectConfig{
		Name: name + "Connection",
		Fields: graphql.Fields{
			"nodes":      &graphql.Field{Type: graphql.NewList(nodeType)},
			"edges":      &graphql.Field{Type: graphql.NewList(edgeType)},
			"totalCount": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
			"pageInfo":   &graphql.Field{Type: graphql.NewNonNull(pageInfo)},
		},
	})
}

// advisorySourceNodes reads a private node slice off a source map.
func advisorySourceNodes(source interface{}, key string) []map[string]interface{} {
	parent, ok := source.(map[string]interface{})
	if !ok {
		return nil
	}
	nodes, _ := parent[key].([]map[string]interface{})
	return nodes
}

// advisoryFilterFromArgs translates the root fields' arguments into the
// store's filter.
func advisoryFilterFromArgs(args map[string]interface{}) store.GlobalAdvisoryFilter {
	filter := store.GlobalAdvisoryFilter{}
	if identifier, ok := args["identifier"].(map[string]interface{}); ok {
		kind, _ := identifier["type"].(string)
		value, _ := identifier["value"].(string)
		switch strings.ToUpper(kind) {
		case "CVE":
			filter.CVEID = value
			// A client naming a specific advisory is asking about that
			// advisory, so a withdrawal is an answer rather than a reason to
			// report it missing.
			filter.IncludeWithdrawn = true
		case "GHSA":
			filter.GHSAID = value
			filter.IncludeWithdrawn = true
		}
	}
	if ecosystem, ok := args["ecosystem"].(string); ok && ecosystem != "" {
		filter.Ecosystem = store.AdvisoryEcosystemFromGraphQL(ecosystem)
	}
	if packageName, ok := args["package"].(string); ok && packageName != "" {
		filter.Package = packageName
	}
	for severity := range enumArgSet(args, "severities") {
		filter.Severities = append(filter.Severities, severity)
	}
	if since, ok := parseAdvisoryTimeArg(args, "publishedSince"); ok {
		filter.PublishedSince = &since
	}
	if since, ok := parseAdvisoryTimeArg(args, "updatedSince"); ok {
		filter.UpdatedSince = &since
	}
	return filter
}

// sortAdvisoriesForArgs applies the connection's orderBy, defaulting to
// GitHub's documented {field: UPDATED_AT, direction: DESC}.
func sortAdvisoriesForArgs(advisories []*store.SecurityAdvisory, args map[string]interface{}) {
	field, direction := "UPDATED_AT", "DESC"
	if order, ok := args["orderBy"].(map[string]interface{}); ok {
		if value, ok := order["field"].(string); ok && value != "" {
			field = value
		}
		if value, ok := order["direction"].(string); ok && value != "" {
			direction = value
		}
	}
	ascending := strings.EqualFold(direction, "ASC")
	if field == "PUBLISHED_AT" {
		store.SortAdvisoriesByPublicationOrder(advisories, ascending)
		return
	}
	// EPSS_PERCENTAGE and EPSS_PERCENTILE order by a score this instance does
	// not have, and every advisory would tie. Ordering by update time keeps
	// the connection's cursors stable instead of leaving the order to a map
	// walk, which is what a client paging through it actually depends on.
	store.SortAdvisoriesByUpdate(advisories, ascending)
}

// filterVulnerabilityNodes applies the ecosystem/package/severity narrowing an
// advisory's vulnerabilities connection accepts.
func filterVulnerabilityNodes(nodes []map[string]interface{}, args map[string]interface{}) []map[string]interface{} {
	wantEcosystem, _ := args["ecosystem"].(string)
	wantPackage, _ := args["package"].(string)
	wantSeverities := enumArgSet(args, "severities")

	filtered := make([]map[string]interface{}, 0, len(nodes))
	for _, node := range nodes {
		ecosystem, _ := node["_ecosystem"].(string)
		packageName, _ := node["_packageName"].(string)
		severity, _ := node["severity"].(string)
		if wantEcosystem != "" && !strings.EqualFold(ecosystem, wantEcosystem) {
			continue
		}
		if wantPackage != "" && !strings.EqualFold(packageName, wantPackage) {
			continue
		}
		if len(wantSeverities) != 0 && !wantSeverities[severity] {
			continue
		}
		filtered = append(filtered, node)
	}
	return filtered
}

// enumArgSet reads a list-of-enum argument into a set. graphql-go delivers
// the members as []interface{} of strings.
func enumArgSet(args map[string]interface{}, key string) map[string]bool {
	raw, ok := args[key].([]interface{})
	if !ok || len(raw) == 0 {
		return nil
	}
	set := make(map[string]bool, len(raw))
	for _, member := range raw {
		if value, ok := member.(string); ok && value != "" {
			set[value] = true
		}
	}
	return set
}

// parseAdvisoryTimeArg reads a DateTime argument, which arrives as the string
// the custom scalar carries.
func parseAdvisoryTimeArg(args map[string]interface{}, key string) (time.Time, bool) {
	value, ok := args[key].(string)
	if !ok || value == "" {
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}
	return parsed.UTC(), true
}

// advisorySeverityEnum renders a stored severity as its
// SecurityAdvisorySeverity member. GitHub's enum has no INFO or NONE, so a
// severity outside the five reports UNKNOWN rather than an invalid member.
func advisorySeverityEnum(severity string) string {
	switch strings.ToUpper(strings.TrimSpace(severity)) {
	case "CRITICAL":
		return "CRITICAL"
	case "HIGH":
		return "HIGH"
	case "MODERATE", "MEDIUM":
		return "MODERATE"
	case "LOW":
		return "LOW"
	default:
		return "UNKNOWN"
	}
}

// dependencyScopeEnum renders a submitted dependency scope as its
// RepositoryVulnerabilityAlertDependencyScope member, or nil when the
// submission did not state one — the enum has no member for "unstated".
func dependencyScopeEnum(scope string) interface{} {
	switch strings.ToLower(strings.TrimSpace(scope)) {
	case "development":
		return "DEVELOPMENT"
	case "runtime":
		return "RUNTIME"
	default:
		return nil
	}
}

// dependencyRelationshipEnum renders a submitted relationship as its
// RepositoryVulnerabilityAlertDependencyRelationship member. The submission
// API's vocabulary is direct/indirect; the alert enum's is
// direct/transitive/inconclusive/unknown, and an unstated relationship is
// UNKNOWN rather than a guess at DIRECT.
func dependencyRelationshipEnum(relationship string) interface{} {
	switch strings.ToLower(strings.TrimSpace(relationship)) {
	case "direct":
		return "DIRECT"
	case "indirect", "transitive":
		return "TRANSITIVE"
	default:
		return "UNKNOWN"
	}
}

// dependencyRelationshipString renders a relationship for
// DependencyGraphDependency.relationship, which GitHub types as a lowercase
// String! rather than as an enum.
func dependencyRelationshipString(relationship string) string {
	switch strings.ToLower(strings.TrimSpace(relationship)) {
	case "direct":
		return "direct"
	case "indirect", "transitive":
		return "transitive"
	default:
		return "unknown"
	}
}

// formatAdvisoryTime renders an optional timestamp as RFC 3339 or nil.
func formatAdvisoryTime(at *time.Time) interface{} {
	if at == nil {
		return nil
	}
	return at.UTC().Format(time.RFC3339)
}

// nullOrEmptyString renders an empty string as a null, which a nullable
// GraphQL String must be rather than "".
func nullOrEmptyString(value string) interface{} {
	if value == "" {
		return nil
	}
	return value
}

// advisoryNodeByID resolves the node ids this family mints, for Query.node.
// Repository-scoped nodes are answered only to a viewer entitled to them;
// an advisory is public.
func (s *Resolver) advisoryNodeByID(ctx context.Context, nodeID string) interface{} {
	if advisory := s.store.GetGlobalAdvisoryByGHSA(ghsaIDFromAdvisoryNodeID(s, nodeID)); advisory != nil {
		return s.advisoryToGQL(advisory)
	}
	if alert := s.store.LookupDependabotAlertByNodeID(nodeID); alert != nil {
		repo := s.store.GetRepoByFullName(alert.RepoKey)
		if repo == nil || !s.viewerHasRepoSecurityAccess(ctx, repo) {
			return nil
		}
		return s.vulnerabilityAlertToGQL(repo, alert)
	}
	if repoID, filename, ok := store.ParseDependencyGraphManifestNodeID(nodeID); ok {
		repo := s.store.GetRepoByID(repoID)
		if repo == nil || !s.viewerCanReadRepo(ctx, repo) {
			return nil
		}
		for _, node := range s.dependencyManifestNodes(repo, nil) {
			if name, _ := node["filename"].(string); name == filename {
				return node
			}
		}
	}
	return nil
}

// ghsaIDFromAdvisoryNodeID maps a SecurityAdvisory node id back to its GHSA
// id by scanning the published advisories, which is the only index the store
// keeps them under.
func ghsaIDFromAdvisoryNodeID(s *Resolver, nodeID string) string {
	if !strings.HasPrefix(nodeID, "GSA_") {
		return ""
	}
	for _, advisory := range s.store.ListGlobalAdvisoriesFiltered(store.GlobalAdvisoryFilter{IncludeWithdrawn: true}) {
		if advisory.NodeID == nodeID {
			return advisory.GHSAID
		}
	}
	return ""
}
