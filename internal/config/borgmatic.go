package config

// BorgmaticConfig represents the top-level borgmatic configuration structure.
// Fields use yaml tags matching borgmatic's schema and omitempty to produce
// clean YAML output with only populated fields.
type BorgmaticConfig struct {
	SourceDirectories []string       `yaml:"source_directories,omitempty"`
	Repositories      []Repository   `yaml:"repositories,omitempty"`
	WorkingDirectory  string         `yaml:"working_directory,omitempty"`
	ArchiveNameFormat string         `yaml:"archive_name_format,omitempty"`
	KeepDaily         int            `yaml:"keep_daily,omitempty"`
	KeepWeekly        int            `yaml:"keep_weekly,omitempty"`
	KeepMonthly       int            `yaml:"keep_monthly,omitempty"`
	KeepYearly        int            `yaml:"keep_yearly,omitempty"`
	Checks            []Check        `yaml:"checks,omitempty"`
	PostgresqlDBs     []PostgresqlDB `yaml:"postgresql_databases,omitempty"`
	MysqlDBs          []MysqlDB      `yaml:"mysql_databases,omitempty"`
	MariadbDBs        []MariadbDB    `yaml:"mariadb_databases,omitempty"`
	SqliteDBs         []SqliteDB     `yaml:"sqlite_databases,omitempty"`
}

// Repository represents a borgmatic backup repository.
type Repository struct {
	Path  string `yaml:"path"`
	Label string `yaml:"label,omitempty"`
}

// Check represents a borgmatic consistency check configuration.
type Check struct {
	Name      string `yaml:"name"`
	Frequency string `yaml:"frequency,omitempty"`
}

// PostgresqlDB represents a PostgreSQL database for borgmatic's database hooks.
type PostgresqlDB struct {
	Name     string `yaml:"name"`
	Hostname string `yaml:"hostname,omitempty"`
	Port     *int   `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Options  string `yaml:"options,omitempty"`
}

// MysqlDB represents a MySQL database for borgmatic's database hooks.
type MysqlDB struct {
	Name     string `yaml:"name"`
	Hostname string `yaml:"hostname,omitempty"`
	Port     *int   `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Options  string `yaml:"options,omitempty"`
}

// MariadbDB represents a MariaDB database for borgmatic's database hooks.
type MariadbDB struct {
	Name     string `yaml:"name"`
	Hostname string `yaml:"hostname,omitempty"`
	Port     *int   `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Options  string `yaml:"options,omitempty"`
}

// SqliteDB represents a SQLite database for borgmatic's database hooks.
type SqliteDB struct {
	Name     string `yaml:"name"`
	Path     string `yaml:"path,omitempty"`
	Hostname string `yaml:"hostname,omitempty"`
	Port     *int   `yaml:"port,omitempty"`
	Username string `yaml:"username,omitempty"`
	Password string `yaml:"password,omitempty"`
	Options  string `yaml:"options,omitempty"`
}
