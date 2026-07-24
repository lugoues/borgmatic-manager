package config

// SetLookPath replaces the exec.LookPath seam for tests.
func (g *Generator) SetLookPath(fn func(string) (string, error)) {
	g.lookPath = fn
}

// SetBoundaryProbe replaces the filesystem-boundary seam for tests.
func (g *Generator) SetBoundaryProbe(fn func(string) (bool, error)) {
	g.boundaryProbe = fn
}
