package runtime

import (
	"context"

	"github.com/stretchr/testify/mock"
)

// MockRuntime is a testify mock implementation of ContainerRuntime.
// It is used by discovery and event listener tests.
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

func (m *MockRuntime) RemoveContainersByLabel(ctx context.Context, key, value string) ([]string, error) {
	args := m.Called(ctx, key, value)
	var names []string
	if v := args.Get(0); v != nil {
		names = v.([]string)
	}
	return names, args.Error(1)
}
