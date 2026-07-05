package discovery

// StubFSProbes replaces the host filesystem probes for tests and returns a
// restore function. Nil arguments leave the corresponding probe unchanged.
func StubFSProbes(mount, read func(string) bool) func() {
	origMount, origRead := isMountPoint, canReadDir
	if mount != nil {
		isMountPoint = mount
	}
	if read != nil {
		canReadDir = read
	}
	return func() {
		isMountPoint, canReadDir = origMount, origRead
	}
}

// MountPointsFromReader exposes mountPoints for tests.
var MountPointsFromReader = mountPoints
