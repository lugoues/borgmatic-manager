package runtime

import (
	"context"
	"io"

	"github.com/stretchr/testify/mock"
)

// MockRuntime is a testify mock implementation of ContainerRuntime.
// It is used by discovery tests, config gen tests, and runner tests.
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
	args := m.Called(ctx, config)
	return args.String(0), args.Error(1)
}

func (m *MockRuntime) StartContainer(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockRuntime) WaitContainer(ctx context.Context, id string) (int64, error) {
	args := m.Called(ctx, id)
	return args.Get(0).(int64), args.Error(1)
}

func (m *MockRuntime) RemoveContainer(ctx context.Context, id string) error {
	args := m.Called(ctx, id)
	return args.Error(0)
}

func (m *MockRuntime) ContainerNetworkConnect(ctx context.Context, networkID, containerID string) error {
	args := m.Called(ctx, networkID, containerID)
	return args.Error(0)
}

func (m *MockRuntime) ContainerLogs(ctx context.Context, id string) (io.ReadCloser, error) {
	args := m.Called(ctx, id)
	return args.Get(0).(io.ReadCloser), args.Error(1)
}
