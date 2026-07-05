package discovery_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/stretchr/testify/assert"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIsBackupEnabled(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "returns true when backup label is true",
			labels: map[string]string{"borgmatic-manager.backup": "true"},
			want:   true,
		},
		{
			name:   "returns false when backup label is absent",
			labels: map[string]string{},
			want:   false,
		},
		{
			name:   "returns false when backup label is false",
			labels: map[string]string{"borgmatic-manager.backup": "false"},
			want:   false,
		},
		{
			name:   "returns false when backup label is yes",
			labels: map[string]string{"borgmatic-manager.backup": "yes"},
			want:   false,
		},
		{
			name:   "returns false when backup label is empty",
			labels: map[string]string{"borgmatic-manager.backup": ""},
			want:   false,
		},
		{
			name:   "returns false with nil labels",
			labels: nil,
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discovery.IsBackupEnabled(tt.labels)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestGetGroup(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   string
	}{
		{
			name:   "returns group name when label is present",
			labels: map[string]string{"borgmatic-manager.group": "myapp"},
			want:   "myapp",
		},
		{
			name:   "returns empty string when label is absent",
			labels: map[string]string{},
			want:   "",
		},
		{
			name:   "returns empty string with nil labels",
			labels: nil,
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discovery.GetGroup(tt.labels)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseDatabaseLabels(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   []models.DatabaseConfig
	}{
		{
			name:   "empty labels returns nil",
			labels: map[string]string{},
			want:   nil,
		},
		{
			name:   "nil labels returns nil",
			labels: nil,
			want:   nil,
		},
		{
			name: "single DB with all fields",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
				"borgmatic-manager.db.0.password": "secret",
				"borgmatic-manager.db.0.hostname": "db.local",
				"borgmatic-manager.db.0.port":     "5432",
				"borgmatic-manager.db.0.options":  "--no-owner",
			},
			want: []models.DatabaseConfig{
				{
					Type:     "postgresql",
					Name:     "appdb",
					Username: "admin",
					Password: "secret",
					Hostname: "db.local",
					Port:     5432,
					Options:  "--no-owner",
				},
			},
		},
		{
			name: "deprecated network label is ignored",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "appdb",
				"borgmatic-manager.db.0.username": "admin",
				"borgmatic-manager.db.0.network":  "backend",
			},
			want: []models.DatabaseConfig{
				{Type: "postgresql", Name: "appdb", Username: "admin"},
			},
		},
		{
			name: "single DB with only required fields",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "mysql",
				"borgmatic-manager.db.0.name":     "mydb",
				"borgmatic-manager.db.0.username": "root",
			},
			want: []models.DatabaseConfig{
				{
					Type:     "mysql",
					Name:     "mydb",
					Username: "root",
				},
			},
		},
		{
			name: "multiple DBs sorted by index",
			labels: map[string]string{
				"borgmatic-manager.db.1.type":     "mysql",
				"borgmatic-manager.db.1.name":     "seconddb",
				"borgmatic-manager.db.1.username": "user2",
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "firstdb",
				"borgmatic-manager.db.0.username": "user1",
			},
			want: []models.DatabaseConfig{
				{Type: "postgresql", Name: "firstdb", Username: "user1"},
				{Type: "mysql", Name: "seconddb", Username: "user2"},
			},
		},
		{
			name: "index gaps are accepted",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "db0",
				"borgmatic-manager.db.0.username": "u0",
				"borgmatic-manager.db.2.type":     "mysql",
				"borgmatic-manager.db.2.name":     "db2",
				"borgmatic-manager.db.2.username": "u2",
			},
			want: []models.DatabaseConfig{
				{Type: "postgresql", Name: "db0", Username: "u0"},
				{Type: "mysql", Name: "db2", Username: "u2"},
			},
		},
		{
			name: "missing required field skips entry",
			labels: map[string]string{
				"borgmatic-manager.db.0.name":     "noType",
				"borgmatic-manager.db.0.username": "user",
				"borgmatic-manager.db.1.type":     "postgresql",
				"borgmatic-manager.db.1.name":     "gooddb",
				"borgmatic-manager.db.1.username": "gooduser",
			},
			want: []models.DatabaseConfig{
				{Type: "postgresql", Name: "gooddb", Username: "gooduser"},
			},
		},
		{
			name: "unknown db type skips entry",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "redis",
				"borgmatic-manager.db.0.name":     "cache",
				"borgmatic-manager.db.0.username": "user",
			},
			want: nil,
		},
		{
			name: "non-integer index skips label",
			labels: map[string]string{
				"borgmatic-manager.db.abc.type":     "postgresql",
				"borgmatic-manager.db.abc.name":     "db",
				"borgmatic-manager.db.abc.username": "user",
			},
			want: nil,
		},
		{
			name: "invalid port value keeps port at 0",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "db",
				"borgmatic-manager.db.0.username": "user",
				"borgmatic-manager.db.0.port":     "notanumber",
			},
			want: []models.DatabaseConfig{
				{Type: "postgresql", Name: "db", Username: "user", Port: 0},
			},
		},
		{
			name: "non-borgmatic labels are ignored",
			labels: map[string]string{
				"com.example.label":               "value",
				"borgmatic-manager.db.0.type":     "mysql",
				"borgmatic-manager.db.0.name":     "mydb",
				"borgmatic-manager.db.0.username": "user",
			},
			want: []models.DatabaseConfig{
				{Type: "mysql", Name: "mydb", Username: "user"},
			},
		},
		{
			name: "sqlite with volume and path",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":   "sqlite",
				"borgmatic-manager.db.0.name":   "app",
				"borgmatic-manager.db.0.volume": "app-data",
				"borgmatic-manager.db.0.path":   "db/app.sqlite3",
			},
			want: []models.DatabaseConfig{
				{Type: "sqlite", Name: "app", Volume: "app-data", Path: "db/app.sqlite3"},
			},
		},
		{
			name: "sqlite missing volume skips entry",
			labels: map[string]string{
				"borgmatic-manager.db.0.type": "sqlite",
				"borgmatic-manager.db.0.name": "app",
				"borgmatic-manager.db.0.path": "app.sqlite3",
			},
			want: nil,
		},
		{
			name: "sqlite missing path skips entry",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":   "sqlite",
				"borgmatic-manager.db.0.name":   "app",
				"borgmatic-manager.db.0.volume": "app-data",
			},
			want: nil,
		},
		{
			name: "sqlite does not require username and clears credentials",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "sqlite",
				"borgmatic-manager.db.0.name":     "app",
				"borgmatic-manager.db.0.volume":   "app-data",
				"borgmatic-manager.db.0.path":     "app.sqlite3",
				"borgmatic-manager.db.0.username": "ignored",
				"borgmatic-manager.db.0.hostname": "ignored.local",
			},
			want: []models.DatabaseConfig{
				{Type: "sqlite", Name: "app", Volume: "app-data", Path: "app.sqlite3"},
			},
		},
		{
			name: "unknown borgmatic db fields are ignored silently",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":        "mariadb",
				"borgmatic-manager.db.0.name":        "db",
				"borgmatic-manager.db.0.username":    "user",
				"borgmatic-manager.db.0.futureField": "value",
			},
			want: []models.DatabaseConfig{
				{Type: "mariadb", Name: "db", Username: "user"},
			},
		},
		{
			name: "missing username skips entry",
			labels: map[string]string{
				"borgmatic-manager.db.0.type": "postgresql",
				"borgmatic-manager.db.0.name": "db",
			},
			want: nil,
		},
		{
			name: "unknown type is rejected before other required-field checks",
			labels: map[string]string{
				"borgmatic-manager.db.0.type": "redis",
				"borgmatic-manager.db.0.name": "cache",
			},
			want: nil,
		},
		{
			name: "missing name skips entry",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.username": "user",
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := discovery.ParseDatabaseLabels(tt.labels, discardLogger())
			assert.Equal(t, tt.want, got)
		})
	}
}
