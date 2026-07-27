package config

import "time"

// SetLookPath replaces the exec.LookPath seam for tests.
func (g *Generator) SetLookPath(fn func(string) (string, error)) {
	g.lookPath = fn
}

// PlaceholderRunID exposes the stand-in run id used by the non-writing passes
// (Plan, RenderGroup) so tests can assert on it by value rather than by literal.
const PlaceholderRunID = placeholderRunID

// ArchiveMatchPattern exposes the archive-name pattern derivation, which the
// runner uses to tell this group's archives apart from another group's in a
// shared repository.
func ArchiveMatchPattern(format string) string { return archiveMatchPattern(format) }

// GlobMatchesForTest exposes the archive-name matcher so its edge cases can be
// tested directly rather than only through generation.
func GlobMatchesForTest(pattern, name string) bool { return globMatches(pattern, name) }

// ArchiveSampleNameForTest renders one sample at a fixed instant, so the
// rendering of individual placeholders can be asserted directly.
func ArchiveSampleNameForTest(format string) string {
	return archiveSampleNameAt(format, time.Date(2026, 7, 27, 16, 4, 26, 0, time.Local))
}

// ArchiveSampleNamesForTest exposes the full spread the collision check uses.
func ArchiveSampleNamesForTest(format string) []string { return archiveSampleNames(format) }

// StrftimeLayoutForTest exposes the directive conversion.
func StrftimeLayoutForTest(spec string) (string, bool) { return strftimeLayout(spec) }

// PatternsCollideForTest exposes the collision decision so the asymmetric and
// degenerate cases can be stated directly rather than reached through a config.
func PatternsCollideForTest(patternA string, samplesA []string, patternB string, samplesB []string) bool {
	return patternsCollide(patternA, samplesA, patternB, samplesB)
}

// SetSampleHostname pins the hostname the sample renderer uses, so a collision
// that depends on the hostname can be tested on any machine.
func SetSampleHostname(name string) func() {
	prev := sampleHostname
	sampleHostname = func() string { return name }
	return func() { sampleHostname = prev }
}
