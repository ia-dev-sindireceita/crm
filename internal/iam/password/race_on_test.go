//go:build race

package password

// raceEnabled is true when the test binary was built with -race.
// TestBenchmarkHashProductionParams_Band uses it to skip the
// wall-clock argon2 cost-band assertion: under -race, memory-access
// instrumentation slows argon2 ~3-5× on the GitHub-hosted CI runner,
// pushing the median past the ADR-0070 §7 ceiling. The band is a
// production-shape regression check, not a -race-shape check.
const raceEnabled = true
