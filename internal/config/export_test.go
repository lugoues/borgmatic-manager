package config

// SetLookPath replaces the exec.LookPath seam for tests.
func (g *Generator) SetLookPath(fn func(string) (string, error)) {
	g.lookPath = fn
}

// SetBoundaryProbe replaces the filesystem-boundary seam for tests.
func (g *Generator) SetBoundaryProbe(fn func(string) (bool, error)) {
	g.boundaryProbe = fn
}

// PlaceholderRunID exposes the stand-in run id used by the non-writing passes
// (Plan, RenderGroup) so tests can assert on it by value rather than by literal.
const PlaceholderRunID = placeholderRunID
