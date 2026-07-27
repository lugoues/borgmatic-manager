package config

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
