package config

import "fmt"

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

// PatternMatchesFormatForTest reports whether a glob can match any name the
// format generates, which is the whole collision question for one direction.
func PatternMatchesFormatForTest(pattern, format string) bool {
	return patternMatchesFormat(pattern, archiveFormatSegments(format))
}

// FormatAlternativesForTest exposes the decomposition, so a directive's domain
// can be asserted directly rather than inferred from a match.
func FormatAlternativesForTest(format string) [][]string {
	var out [][]string
	for _, seg := range archiveFormatSegments(format) {
		switch {
		case seg.any:
			out = append(out, nil)
		case seg.digits > 0:
			out = append(out, []string{fmt.Sprintf("<%d digits>", seg.digits)})
		default:
			out = append(out, seg.alts)
		}
	}
	return out
}

// PatternsCollideForTest exposes the collision decision so the asymmetric and
// degenerate cases can be stated directly rather than reached through a config.
func PatternsCollideForTest(patternA, formatA, patternB, formatB string) bool {
	return patternsCollide(patternA, archiveFormatSegments(formatA), patternB, archiveFormatSegments(formatB))
}

// SetSampleHostname pins the hostname the decomposition uses, so a collision
// that depends on the hostname can be tested on any machine.
func SetSampleHostname(name string) func() {
	prev := sampleHostname
	sampleHostname = func() string { return name }
	return func() { sampleHostname = prev }
}

// ExtractRepoRefsForTest exposes the repository extraction so the difference
// between an empty list and an unreadable one can be stated directly.
func ExtractRepoRefsForTest(final map[string]interface{}) ([]RepoRef, bool) {
	return extractRepoRefs(final)
}
