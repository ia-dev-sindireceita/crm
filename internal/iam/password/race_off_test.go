//go:build !race

package password

// See race_on_test.go for the full rationale. When -race is OFF
// (production-shape build), the cost-band assertion runs.
const raceEnabled = false
