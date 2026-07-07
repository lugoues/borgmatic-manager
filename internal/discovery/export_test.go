package discovery

// StubFSProbes replaces the host filesystem probes for tests and returns a
// restore function. Nil arguments leave the corresponding probe unchanged.
func StubFSProbes(mount, read func(string) bool) func() {
	return StubAllFSProbes(mount, read, func(string) bool { return true })
}

// StubAllFSProbes also stubs the directory-emptiness probe.
func StubAllFSProbes(mount, read, empty func(string) bool) func() {
	origMount, origRead, origEmpty := isMountPoint, canReadDir, dirIsEmpty
	if mount != nil {
		isMountPoint = mount
	}
	if read != nil {
		canReadDir = read
	}
	if empty != nil {
		dirIsEmpty = empty
	}
	return func() {
		isMountPoint, canReadDir, dirIsEmpty = origMount, origRead, origEmpty
	}
}

// MountPointsFromReader exposes mountPoints for tests.
var MountPointsFromReader = mountPoints
