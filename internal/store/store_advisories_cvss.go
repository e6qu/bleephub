package store

import (
	"math"
	"strings"
)

// CVSS base-score computation.
//
// An advisory is authored with a CVSS vector string, never with a number:
// repository-advisory-create carries cvss_vector_string and no score member,
// because the score is not an independent fact — it is a total function of
// the vector, defined by the CVSS specification. GitHub computes it and
// serves both.
//
// Without this, every advisory authored through the documented request came
// back scored 0.0 while carrying a vector that says "critical", and both the
// REST cvss.score and the GraphQL CVSS.score reported that 0 as though it
// were the advisory's severity.
//
// CVSS v3.0 and v3.1 share this arithmetic (v3.1 changed only the rounding
// rule, which roundUpCVSS implements). A v4.0 vector is NOT computed here:
// its score comes from a large published lookup table rather than a formula,
// and guessing at it with the v3 arithmetic would produce a confidently wrong
// number. CVSSBaseScore reports false for one, and the caller leaves the
// score unset rather than inventing it.

// cvssMetricWeights is the CVSS v3.x base-metric weight table. Privileges
// Required is absent because its weights depend on Scope, which is resolved
// in CVSSBaseScore.
var cvssMetricWeights = map[string]map[string]float64{
	"AV": {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.20},
	"AC": {"L": 0.77, "H": 0.44},
	"UI": {"N": 0.85, "R": 0.62},
	"C":  {"H": 0.56, "L": 0.22, "N": 0.00},
	"I":  {"H": 0.56, "L": 0.22, "N": 0.00},
	"A":  {"H": 0.56, "L": 0.22, "N": 0.00},
}

// cvssPrivilegesRequired holds the two Privileges Required weightings: an
// unchanged scope, and a changed one, where holding low privileges is worth
// more to an attacker because the impact crosses a security boundary.
var cvssPrivilegesRequired = map[bool]map[string]float64{
	false: {"N": 0.85, "L": 0.62, "H": 0.27}, // Scope: Unchanged
	true:  {"N": 0.85, "L": 0.68, "H": 0.50}, // Scope: Changed
}

// CVSSBaseScore computes the base score of a CVSS v3.0/v3.1 vector string,
// reporting false when the string is not a complete v3 base vector.
//
// Every one of the eight base metrics must be present with a legal value:
// CVSS defines no default for a missing metric, so a partial vector has no
// score rather than a score computed from assumptions.
func CVSSBaseScore(vector string) (float64, bool) {
	metrics, ok := parseCVSSVector(vector)
	if !ok {
		return 0, false
	}
	scopeChanged := metrics["S"] == "C"

	weight := func(metric string) (float64, bool) {
		if metric == "PR" {
			value, present := cvssPrivilegesRequired[scopeChanged][metrics["PR"]]
			return value, present
		}
		value, present := cvssMetricWeights[metric][metrics[metric]]
		return value, present
	}

	values := map[string]float64{}
	for _, metric := range []string{"AV", "AC", "PR", "UI", "C", "I", "A"} {
		value, present := weight(metric)
		if !present {
			return 0, false
		}
		values[metric] = value
	}

	// Impact sub-score, from the three impact metrics.
	impactSubScore := 1 - ((1 - values["C"]) * (1 - values["I"]) * (1 - values["A"]))
	var impact float64
	if scopeChanged {
		impact = 7.52*(impactSubScore-0.029) - 3.25*math.Pow(impactSubScore-0.02, 15)
	} else {
		impact = 6.42 * impactSubScore
	}
	if impact <= 0 {
		// A vulnerability with no impact on confidentiality, integrity or
		// availability scores zero however exploitable it is.
		return 0, true
	}

	exploitability := 8.22 * values["AV"] * values["AC"] * values["PR"] * values["UI"]
	score := impact + exploitability
	if scopeChanged {
		score *= 1.08
	}
	return roundUpCVSS(math.Min(score, 10)), true
}

// parseCVSSVector splits a vector string into its metric/value pairs,
// requiring the CVSS:3.x prefix so a v2 or v4 vector is not silently scored
// with v3 arithmetic.
func parseCVSSVector(vector string) (map[string]string, bool) {
	vector = strings.TrimSpace(vector)
	if vector == "" {
		return nil, false
	}
	parts := strings.Split(vector, "/")
	version := strings.ToUpper(parts[0])
	if version != "CVSS:3.0" && version != "CVSS:3.1" {
		return nil, false
	}
	metrics := make(map[string]string, len(parts))
	for _, part := range parts[1:] {
		name, value, found := strings.Cut(part, ":")
		if !found {
			continue
		}
		metrics[strings.ToUpper(strings.TrimSpace(name))] = strings.ToUpper(strings.TrimSpace(value))
	}
	// Scope has no weight of its own but selects the PR table and the two
	// impact formulas, so an absent or illegal S makes the vector unscorable.
	if scope := metrics["S"]; scope != "U" && scope != "C" {
		return nil, false
	}
	return metrics, true
}

// roundUpCVSS implements the CVSS v3.1 "Roundup" function: round to one
// decimal place, always upward.
//
// The naive math.Ceil(score*10)/10 is not this function. CVSS v3.1 defines
// Roundup over the integer scaled by 100000 precisely because binary floating
// point makes an exactly-representable-looking value like 4.02 land a hair
// above 4.02, which the naive form rounds up to 4.1 instead of leaving at
// 4.1's predecessor.
func roundUpCVSS(score float64) float64 {
	scaled := int64(math.Round(score * 100000))
	if scaled%10000 == 0 {
		return float64(scaled) / 100000
	}
	return (math.Floor(float64(scaled)/10000) + 1) / 10
}

// AdvisoryCVSSScore is the score to serve for an advisory: the one the author
// supplied if any, otherwise the one its vector implies. It reports false
// when the advisory has neither, which is the nullable score's honest answer.
func AdvisoryCVSSScore(a *SecurityAdvisory) (float64, bool) {
	if a == nil {
		return 0, false
	}
	if a.CVSSScore != 0 {
		return a.CVSSScore, true
	}
	return CVSSBaseScore(a.CVSSVector)
}
