package models

// BackupState is the discovered backup configuration, keyed by group name.
type BackupState struct {
	Groups map[string]*VolumeGroup
}

// NewBackupState creates a new BackupState with an initialized (non-nil) Groups map.
func NewBackupState() *BackupState {
	return &BackupState{
		Groups: make(map[string]*VolumeGroup),
	}
}

func (bs *BackupState) getOrCreateGroup(name string) *VolumeGroup {
	g, ok := bs.Groups[name]
	if !ok {
		g = &VolumeGroup{}
		bs.Groups[name] = g
	}
	return g
}

// AddVolume appends a VolumeInfo to the named group, creating the group if needed.
func (bs *BackupState) AddVolume(group string, vol VolumeInfo) {
	g := bs.getOrCreateGroup(group)
	g.Volumes = append(g.Volumes, vol)
}

// AddDatabases appends database configs to the named group, creating it if needed.
func (bs *BackupState) AddDatabases(group string, dbs []DatabaseConfig) {
	g := bs.getOrCreateGroup(group)
	g.Databases = append(g.Databases, dbs...)
}

// VolumeGroup aggregates one backup group's volumes and databases.
type VolumeGroup struct {
	Volumes   []VolumeInfo
	Databases []DatabaseConfig
}

// VolumeInfo describes a container volume to be backed up.
type VolumeInfo struct {
	Name string
	// HostPath is the runtime-reported mountpoint on the host.
	HostPath string
}

// DatabaseConfig describes a database backed up via borgmatic's database hooks.
type DatabaseConfig struct {
	// Type is the database engine: postgresql, mysql, mariadb, or sqlite.
	Type string
	Name string
	// Username is the database user for authentication (required except sqlite).
	Username string
	// Password is the database password (optional).
	Password string
	// Hostname, when set, overrides container-based connection resolution.
	Hostname string
	// Port; 0 means the default (container-internal in container mode).
	Port int
	// Container is the labeled source container; set by discovery, not labels.
	Container string
	// Volume is the named volume holding a SQLite database file (sqlite only).
	Volume string
	// Path inside Volume; discovery resolves it to an absolute host path.
	Path string
	// Options contains additional database-specific options (optional).
	Options string
}
