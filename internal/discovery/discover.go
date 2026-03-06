package discovery

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
)

// Discover queries the container runtime for labeled volumes and containers,
// then populates a BackupState using the label parser. Volumes are filtered
// by borgmatic-manager.backup=true and grouped by borgmatic-manager.group.
// Containers are filtered by borgmatic-manager.group and parsed for database
// configuration labels.
func Discover(ctx context.Context, rt runtime.ContainerRuntime, logger *slog.Logger) (*models.BackupState, error) {
	state := models.NewBackupState()

	// Discover volumes.
	volumes, err := rt.ListVolumes(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing volumes: %w", err)
	}

	for _, v := range volumes {
		group := GetGroup(v.Labels)
		if group == "" {
			logger.Warn("volume has backup=true but no group label", "volume", v.Name)
			continue
		}
		state.AddVolume(group, models.VolumeInfo{
			Name:      v.Name,
			MountPath: "/mnt/sources/" + v.Name,
		})
	}

	// Discover containers with database labels.
	containers, err := rt.ListContainers(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing containers: %w", err)
	}

	for _, c := range containers {
		group := GetGroup(c.Labels)
		if group == "" {
			continue
		}
		dbs := ParseDatabaseLabels(c.Labels, logger)
		if len(dbs) > 0 {
			state.AddDatabases(group, dbs)
		}
	}

	totalVols := countVolumes(state)
	totalDBs := countDatabases(state)

	if len(state.Groups) == 0 {
		logger.Warn("no backup groups discovered; check volume/container labels")
	}

	logger.Info("discovery complete",
		"groups", len(state.Groups),
		"volumes", totalVols,
		"databases", totalDBs,
	)

	return state, nil
}

func countVolumes(state *models.BackupState) int {
	total := 0
	for _, g := range state.Groups {
		total += len(g.Volumes)
	}
	return total
}

func countDatabases(state *models.BackupState) int {
	total := 0
	for _, g := range state.Groups {
		total += len(g.Databases)
	}
	return total
}
