package rdbclean_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/saremox/redis-operator/cmd/instance/rdbclean"
)

func write(t *testing.T, dir, name string, size int) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), make([]byte, size), 0o644))
}

func TestCleanRemovesOnlyTheStaleRDBFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "dump.rdb", 5)
	write(t, dir, "temp-12.rdb", 3)
	write(t, dir, "temp-13.rdb", 4)
	write(t, dir, "appendonly.aof", 9)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "old.rdb"), 0o755))

	var events []rdbclean.Kind
	cleaned, size, err := rdbclean.Clean(dir, "dump.rdb", false, os.ReadDir, func(e rdbclean.Event) { events = append(events, e.Kind) })

	require.NoError(t, err)
	assert.Equal(t, 2, cleaned)
	assert.EqualValues(t, 7, size)
	assert.Equal(t, []rdbclean.Kind{rdbclean.Removed, rdbclean.Removed}, events)
	for _, kept := range []string{"dump.rdb", "appendonly.aof"} {
		assert.FileExists(t, filepath.Join(dir, kept))
	}
	assert.DirExists(t, filepath.Join(dir, "old.rdb"))
	assert.NoFileExists(t, filepath.Join(dir, "temp-12.rdb"))
}

func TestCleanInADryRunKeepsTheFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "temp-1.rdb", 3)

	var got rdbclean.Event
	cleaned, _, err := rdbclean.Clean(dir, "dump.rdb", true, os.ReadDir, func(e rdbclean.Event) { got = e })

	require.NoError(t, err)
	assert.Equal(t, 1, cleaned)
	assert.Equal(t, rdbclean.WouldRemove, got.Kind)
	assert.FileExists(t, filepath.Join(dir, "temp-1.rdb"))
}

func TestCleanReportsAMissingDirectoryAndReturnsNoError(t *testing.T) {
	var got rdbclean.Event
	cleaned, _, err := rdbclean.Clean(filepath.Join(t.TempDir(), "none"), "dump.rdb", false, os.ReadDir, func(e rdbclean.Event) { got = e })

	require.NoError(t, err)
	assert.Zero(t, cleaned)
	assert.Equal(t, rdbclean.Missing, got.Kind)
}

func TestCleanReturnsTheErrors(t *testing.T) {
	file := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(file, nil, 0o644))
	readErr := errors.New("read failed")
	failing := func(string) ([]os.DirEntry, error) { return nil, readErr }

	_, _, err := rdbclean.Clean(file, "dump.rdb", false, os.ReadDir, func(rdbclean.Event) {})
	require.ErrorContains(t, err, "is not a directory")

	_, _, err = rdbclean.Clean(t.TempDir(), "dump.rdb", false, failing, func(rdbclean.Event) {})
	require.ErrorIs(t, err, readErr)
}
