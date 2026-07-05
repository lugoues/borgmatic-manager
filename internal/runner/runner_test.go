package runner

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/docker/docker/api/types/mount"
	"github.com/lugoues/borgmatic-manager/internal/config"
	"github.com/lugoues/borgmatic-manager/internal/models"
	"github.com/lugoues/borgmatic-manager/internal/runtime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
)

// stdcopyFrame creates a Docker stdcopy-formatted frame.
// streamType: 1 = stdout, 2 = stderr
func stdcopyFrame(streamType byte, data string) []byte {
	header := make([]byte, 8)
	header[0] = streamType
	binary.BigEndian.PutUint32(header[4:], uint32(len(data)))
	return append(header, []byte(data)...)
}

func newTestConfig() *config.ManagerConfig {
	return &config.ManagerConfig{}
}

func newTestGroup() *models.VolumeGroup {
	return &models.VolumeGroup{
		Volumes: []models.VolumeInfo{
			{Name: "app-data", HostPath: "/mnt/source/app-data"},
		},
		Databases: []models.DatabaseConfig{
			{Type: "postgresql", Name: "appdb"},
		},
	}
}

func setupMockForFullLifecycle(m *runtime.MockRuntime, logData []byte) {
	m.On("CreateContainer", mock.Anything, mock.AnythingOfType("runtime.ContainerConfig")).Return("ctr-123", nil)
	m.On("ContainerNetworkConnect", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("StartContainer", mock.Anything, "ctr-123").Return(nil)
	m.On("ContainerLogs", mock.Anything, "ctr-123").Return(io.NopCloser(bytes.NewReader(logData)), nil)
	m.On("WaitContainer", mock.Anything, "ctr-123").Return(int64(0), nil)
	m.On("RemoveContainer", mock.Anything, "ctr-123").Return(nil)
}

func TestRunGroup_Mounts(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	group := newTestGroup()
	cfg := newTestConfig()

	logData := stdcopyFrame(1, "done\n")

	m.On("CreateContainer", mock.Anything, mock.MatchedBy(func(c runtime.ContainerConfig) bool {
		// Verify mounts
		hasConfigBind := false
		hasSourceVolume := false
		hasCacheVolume := false
		hasSSHBind := false

		for _, mnt := range c.Mounts {
			switch {
			case mnt.Type == mount.TypeBind && mnt.Target == "/etc/borgmatic/config.yaml" && mnt.ReadOnly:
				hasConfigBind = true
			case mnt.Type == mount.TypeVolume && mnt.Source == "app-data" && mnt.Target == "/mnt/source/app-data" && mnt.ReadOnly:
				hasSourceVolume = true
			case mnt.Type == mount.TypeVolume && mnt.Source == "borgmatic-cache" && mnt.Target == "/root/.cache/borg" && !mnt.ReadOnly:
				hasCacheVolume = true
			case mnt.Type == mount.TypeBind && mnt.Target == "/root/.ssh" && mnt.ReadOnly:
				hasSSHBind = true
			}
		}

		return hasConfigBind && hasSourceVolume && hasCacheVolume && hasSSHBind
	})).Return("ctr-123", nil)
	m.On("ContainerNetworkConnect", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("StartContainer", mock.Anything, "ctr-123").Return(nil)
	m.On("ContainerLogs", mock.Anything, "ctr-123").Return(io.NopCloser(bytes.NewReader(logData)), nil)
	m.On("WaitContainer", mock.Anything, "ctr-123").Return(int64(0), nil)
	m.On("RemoveContainer", mock.Anything, "ctr-123").Return(nil)

	err := r.RunGroup(context.Background(), "mygroup", group, cfg)
	assert.NoError(t, err)
	m.AssertExpectations(t)
}

func TestRunGroup_Cleanup(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	group := newTestGroup()
	cfg := newTestConfig()

	logData := stdcopyFrame(1, "done\n")
	setupMockForFullLifecycle(m, logData)

	err := r.RunGroup(context.Background(), "mygroup", group, cfg)
	assert.NoError(t, err)
	m.AssertCalled(t, "RemoveContainer", mock.Anything, "ctr-123")
	m.AssertExpectations(t)
}

func TestRunGroup_StartFailure(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	group := newTestGroup()
	cfg := newTestConfig()

	m.On("CreateContainer", mock.Anything, mock.AnythingOfType("runtime.ContainerConfig")).Return("ctr-123", nil)
	m.On("ContainerNetworkConnect", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("StartContainer", mock.Anything, "ctr-123").Return(errors.New("start failed"))
	m.On("RemoveContainer", mock.Anything, "ctr-123").Return(nil)

	err := r.RunGroup(context.Background(), "mygroup", group, cfg)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "start failed")
	m.AssertCalled(t, "RemoveContainer", mock.Anything, "ctr-123")
	m.AssertNotCalled(t, "WaitContainer", mock.Anything, mock.Anything)
	m.AssertExpectations(t)
}

func TestRunGroup_LogStreaming(t *testing.T) {
	m := &runtime.MockRuntime{}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	group := newTestGroup()
	cfg := newTestConfig()

	// Build stdcopy-formatted log data with stdout and stderr lines
	var logData bytes.Buffer
	logData.Write(stdcopyFrame(1, "stdout line 1\n"))
	logData.Write(stdcopyFrame(2, "stderr line 1\n"))
	logData.Write(stdcopyFrame(1, "stdout line 2\n"))

	m.On("CreateContainer", mock.Anything, mock.AnythingOfType("runtime.ContainerConfig")).Return("ctr-123", nil)
	m.On("ContainerNetworkConnect", mock.Anything, mock.Anything, mock.Anything).Return(nil).Maybe()
	m.On("StartContainer", mock.Anything, "ctr-123").Return(nil)
	m.On("ContainerLogs", mock.Anything, "ctr-123").Return(io.NopCloser(&logData), nil)
	m.On("WaitContainer", mock.Anything, "ctr-123").Return(int64(0), nil)
	m.On("RemoveContainer", mock.Anything, "ctr-123").Return(nil)

	err := r.RunGroup(context.Background(), "mygroup", group, cfg)
	assert.NoError(t, err)

	logOutput := logBuf.String()
	assert.Contains(t, logOutput, "stdout line 1")
	assert.Contains(t, logOutput, "stderr line 1")
	assert.Contains(t, logOutput, "stdout line 2")
	assert.Contains(t, logOutput, `"stream":"stdout"`)
	assert.Contains(t, logOutput, `"stream":"stderr"`)
	assert.Contains(t, logOutput, `"group":"mygroup"`)
}

func TestTryRunGroup_MutexSkip(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	group := newTestGroup()
	cfg := newTestConfig()

	// Pre-lock the mutex for "mygroup"
	mu := r.getMutex("mygroup")
	mu.Lock()

	// TryRunGroup should return false without running
	ran, err := r.TryRunGroup(context.Background(), "mygroup", group, cfg, config.GroupRunMeta{})
	assert.NoError(t, err)
	assert.False(t, ran)

	// No runtime methods should have been called
	m.AssertNotCalled(t, "CreateContainer", mock.Anything, mock.Anything)

	mu.Unlock()
}

func TestTryRunGroup_Success(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	group := newTestGroup()
	cfg := newTestConfig()

	logData := stdcopyFrame(1, "done\n")
	setupMockForFullLifecycle(m, logData)

	ran, err := r.TryRunGroup(context.Background(), "mygroup", group, cfg, config.GroupRunMeta{})
	assert.NoError(t, err)
	assert.True(t, ran)
	m.AssertExpectations(t)
}

func TestSlogWriter(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	w := &slogWriter{
		logger: logger,
		group:  "testgroup",
		stream: "stdout",
	}

	// Write partial line, then complete it
	w.Write([]byte("hello "))
	w.Write([]byte("world\n"))

	output := buf.String()
	assert.Contains(t, output, "hello world")
	assert.Contains(t, output, `"group":"testgroup"`)
	assert.Contains(t, output, `"stream":"stdout"`)
}

func TestSlogWriter_MultipleLines(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&buf, nil))

	w := &slogWriter{
		logger: logger,
		group:  "grp",
		stream: "stderr",
	}

	w.Write([]byte("line1\nline2\nline3\n"))

	output := buf.String()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	assert.Equal(t, 3, len(lines), "expected 3 log lines")
	assert.Contains(t, output, "line1")
	assert.Contains(t, output, "line2")
	assert.Contains(t, output, "line3")
}

// Verify getMutex is exported for testing (via lowercase but same package)
func TestGetMutex_ReturnsConsistentMutex(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	mu1 := r.getMutex("group-a")
	mu2 := r.getMutex("group-a")
	mu3 := r.getMutex("group-b")

	assert.Same(t, mu1, mu2, "same group should return same mutex")
	assert.NotSame(t, mu1, mu3, "different groups should return different mutexes")
}

// Concurrency safety test for getMutex
func TestGetMutex_ConcurrentAccess(t *testing.T) {
	m := &runtime.MockRuntime{}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	r := NewRunner(m, logger, "/etc/borgmatic/generated")

	var wg sync.WaitGroup
	mutexes := make([]*sync.Mutex, 100)

	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			mutexes[idx] = r.getMutex("same-group")
		}(i)
	}

	wg.Wait()

	// All should be the same mutex
	for i := 1; i < 100; i++ {
		assert.Same(t, mutexes[0], mutexes[i])
	}
}
