package discovery_test

import (
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/lugoues/borgmatic-manager/internal/discovery"
	"github.com/lugoues/borgmatic-manager/internal/models"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestIsEnabled(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   bool
	}{
		{
			name:   "returns true when enable label is true",
			labels: map[string]string{"borgmatic-manager.enable": "true"},
			want:   true,
		},
		{
			name:   "returns false when enable label is absent",
			labels: map[string]string{},
			want:   false,
		},
		{
			name:   "returns false when enable label is false",
			labels: map[string]string{"borgmatic-manager.enable": "false"},
			want:   false,
		},
		{
			name:   "returns false when enable label is yes",
			labels: map[string]string{"borgmatic-manager.enable": "yes"},
			want:   false,
		},
		{
			name:   "returns false when enable label is empty",
			labels: map[string]string{"borgmatic-manager.enable": ""},
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
			got := discovery.IsEnabled(tt.labels)
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
		name    string
		labels  map[string]string
		want    []models.DatabaseConfig
		wantErr bool
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
			name: "missing required field fails (a skipped entry would shrink the backup set)",
			labels: map[string]string{
				"borgmatic-manager.db.0.name":     "noType",
				"borgmatic-manager.db.0.username": "user",
				"borgmatic-manager.db.1.type":     "postgresql",
				"borgmatic-manager.db.1.name":     "gooddb",
				"borgmatic-manager.db.1.username": "gooduser",
			},
			wantErr: true,
		},
		{
			name: "unknown db type fails",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "redis",
				"borgmatic-manager.db.0.name":     "cache",
				"borgmatic-manager.db.0.username": "user",
			},
			wantErr: true,
		},
		{
			name: "non-integer index fails",
			labels: map[string]string{
				"borgmatic-manager.db.abc.type":     "postgresql",
				"borgmatic-manager.db.abc.name":     "db",
				"borgmatic-manager.db.abc.username": "user",
			},
			wantErr: true,
		},
		{
			name: "invalid port value fails",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.name":     "db",
				"borgmatic-manager.db.0.username": "user",
				"borgmatic-manager.db.0.port":     "notanumber",
			},
			wantErr: true,
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
			name: "sqlite missing volume fails",
			labels: map[string]string{
				"borgmatic-manager.db.0.type": "sqlite",
				"borgmatic-manager.db.0.name": "app",
				"borgmatic-manager.db.0.path": "app.sqlite3",
			},
			wantErr: true,
		},
		{
			name: "sqlite missing path fails",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":   "sqlite",
				"borgmatic-manager.db.0.name":   "app",
				"borgmatic-manager.db.0.volume": "app-data",
			},
			wantErr: true,
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
			name: "unknown db field name fails (typos must not be silent)",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":        "mariadb",
				"borgmatic-manager.db.0.name":        "db",
				"borgmatic-manager.db.0.username":    "user",
				"borgmatic-manager.db.0.futureField": "value",
			},
			wantErr: true,
		},
		{
			name: "missing username fails",
			labels: map[string]string{
				"borgmatic-manager.db.0.type": "postgresql",
				"borgmatic-manager.db.0.name": "db",
			},
			wantErr: true,
		},
		{
			name: "unknown type is rejected before other required-field checks",
			labels: map[string]string{
				"borgmatic-manager.db.0.type": "redis",
				"borgmatic-manager.db.0.name": "cache",
			},
			wantErr: true,
		},
		{
			name: "missing name fails",
			labels: map[string]string{
				"borgmatic-manager.db.0.type":     "postgresql",
				"borgmatic-manager.db.0.username": "user",
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := discovery.ParseDatabaseLabels(tt.labels, discardLogger())
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}
