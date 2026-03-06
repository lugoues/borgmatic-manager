package runtime

import (
	"context"
	"fmt"

	"github.com/stretchr/testify/mock"
)

// MockRuntime is a testify mock implementation of ContainerRuntime.
// It is used by discovery tests (Plan 03) and config gen tests (Plan 04).
type MockRuntime struct {
	mock.Mock
}

func (m *MockRuntime) ListVolumes(ctx context.Context) ([]VolumeInfo, error) {
	args := m.Called(ctx)
	return args.Get(0).([]VolumeInfo), args.Error(1)
}

func (m *MockRuntime) ListContainers(ctx context.Context) ([]ContainerInfo, error) {
	args := m.Called(ctx)
	return args.Get(0).([]ContainerInfo), args.Error(1)
}

func (m *MockRuntime) EventStream(ctx context.Context) (<-chan Event, <-chan error) {
	args := m.Called(ctx)
	return args.Get(0).(<-chan Event), args.Get(1).(<-chan error)
}

func (m *MockRuntime) CreateContainer(ctx context.Context, config ContainerConfig) (string, error) {
	return "", fmt.Errorf("CreateContainer: not implemented")
}

func (m *MockRuntime) StartContainer(ctx context.Context, id string) error {
	return fmt.Errorf("StartContainer: not implemented")
}

func (m *MockRuntime) WaitContainer(ctx context.Context, id string) (int64, error) {
	return 0, fmt.Errorf("WaitContainer: not implemented")
}

func (m *MockRuntime) RemoveContainer(ctx context.Context, id string) error {
	return fmt.Errorf("RemoveContainer: not implemented")
}
