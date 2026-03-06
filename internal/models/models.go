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

// VolumeInfo describes a Docker volume to be backed up.
type VolumeInfo struct {
	// Name is the Docker volume name.
	Name string
	// MountPath is the path where the volume is mounted inside the backup container.
	// This field is computed at a later stage (Phase 2/3) and may be empty initially.
	MountPath string
}

// DatabaseConfig describes a database backed up via borgmatic's database hooks.
type DatabaseConfig struct {
	// Type is the database engine: postgresql, mysql, mariadb, or sqlite.
	Type string
	Name string
	// Username is the database user for authentication.
	Username string
	// Password is the database password (optional).
	Password string
	// Hostname is the database server hostname (optional).
	Hostname string
	// Port is the database server port. A value of 0 means use the default port.
	Port int
	// Network is the Docker network used to reach the database (optional).
	Network string
	// Options contains additional database-specific options (optional).
	Options string
}
