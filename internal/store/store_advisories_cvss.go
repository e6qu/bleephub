package store

import (
	"math"
	"strings"
)

// CVSS base-score computation.
//
// An advisory is authored with a vector string, never a number: the score is a
// total function of the vector per the CVSS spec, so it must be computed here.
// v3.0 and v3.1 share this arithmetic (v3.1 changed only rounding, in
// roundUpCVSS). v4.0 is NOT computed: its score comes from a published lookup
// table, so CVSSBaseScore reports false and the caller leaves it unset.

// cvssMetricWeights is the CVSS v3.x base-metric weight table. Privileges
// Required is absent because its weights depend on Scope, resolved in
// CVSSBaseScore.
var cvssMetricWeights = map[string]map[string]float64{
	"AV": {"N": 0.85, "A": 0.62, "L": 0.55, "P": 0.20},
	"AC": {"L": 0.77, "H": 0.44},
	"UI": {"N": 0.85, "R": 0.62},
	"C":  {"H": 0.56, "L": 0.22, "N": 0.00},
	"I":  {"H": 0.56, "L": 0.22, "N": 0.00},
	"A":  {"H": 0.56, "L": 0.22, "N": 0.00},
}

// cvssPrivilegesRequired holds the two Privileges Required weightings, keyed by
// whether Scope changed (a changed scope weights low privileges higher).
var cvssPrivilegesRequired = map[bool]map[string]float64{
	false: {"N": 0.85, "L": 0.62, "H": 0.27}, // Scope: Unchanged
	true:  {"N": 0.85, "L": 0.68, "H": 0.50}, // Scope: Changed
}

// CVSSBaseScore computes the base score of a v3.0/v3.1 vector, reporting false
// when the string is not a complete v3 base vector. All eight metrics must be
// present with legal values: CVSS defines no default for a missing metric.
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

	impactSubScore := 1 - ((1 - values["C"]) * (1 - values["I"]) * (1 - values["A"]))
	var impact float64
	if scopeChanged {
		impact = 7.52*(impactSubScore-0.029) - 3.25*math.Pow(impactSubScore-0.02, 15)
	} else {
		impact = 6.42 * impactSubScore
	}
	if impact <= 0 {
		// No CIA impact scores zero, however exploitable.
		return 0, true
	}

	exploitability := 8.22 * values["AV"] * values["AC"] * values["PR"] * values["UI"]
	score := impact + exploitability
	if scopeChanged {
		score *= 1.08
	}
	return roundUpCVSS(math.Min(score, 10)), true
}

// parseCVSSVector splits a vector into metric/value pairs, requiring the
// CVSS:3.x prefix so a v2 or v4 vector isn't scored with v3 arithmetic.
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
	// Scope selects the PR table and impact formulas, so an absent or illegal
	// S makes the vector unscorable.
	if scope := metrics["S"]; scope != "U" && scope != "C" {
		return nil, false
	}
	return metrics, true
}

// roundUpCVSS implements the CVSS v3.1 "Roundup" function: round up to one
// decimal. It scales by 100000 (not the naive math.Ceil(score*10)/10) because
// float error makes a value like 4.02 land a hair high and round up wrongly.
func roundUpCVSS(score float64) float64 {
	scaled := int64(math.Round(score * 100000))
	if scaled%10000 == 0 {
		return float64(scaled) / 100000
	}
	return (math.Floor(float64(scaled)/10000) + 1) / 10
}

// AdvisoryCVSSScore returns the author-supplied score if any, else the one the
// vector implies, reporting false when the advisory has neither.
func AdvisoryCVSSScore(a *SecurityAdvisory) (float64, bool) {
	if a == nil {
		return 0, false
	}
	if a.CVSSScore != 0 {
		return a.CVSSScore, true
	}
	return CVSSBaseScore(a.CVSSVector)
}
