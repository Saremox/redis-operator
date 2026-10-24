package bootstrap

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// run executes the command with dest as its only argument.
func run(t *testing.T, dest string) error {
	t.Helper()
	cmd := NewCmd()
	cmd.SetArgs([]string{dest})
	return cmd.Execute()
}

// tempFiles returns the temporary files that a run left in dir.
func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "redis-instance.tmp*"))
	require.NoError(t, err)
	return matches
}

// selfBytes returns the content of the running binary.
func selfBytes(t *testing.T) []byte {
	t.Helper()
	self, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(self)
	require.NoError(t, err)
	return content
}

func TestBootstrapCopiesTheRunningBinary(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "redis-instance")

	require.NoError(t, run(t, dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	// Compare the bytes. A file of the same length is not yet a copy.
	assert.True(t, bytes.Equal(selfBytes(t), got), "the copy differs from the source")
}

func TestBootstrapMakesTheCopyExecutable(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "redis-instance")

	require.NoError(t, run(t, dest))

	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.NotZero(t, info.Mode().Perm()&0o111, "the copy needs the execute bits")
}

// The init container runs again on every pod restart, so the destination is
// already there on each run but the first.
func TestBootstrapOverwritesAnExistingDestination(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")
	require.NoError(t, os.WriteFile(dest, []byte("an older copy"), 0o755))

	require.NoError(t, run(t, dest))

	got, err := os.ReadFile(dest)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(selfBytes(t), got), "the older copy must be replaced")
	assert.Empty(t, tempFiles(t, dir))
}

func TestBootstrapLeavesNoFileWhenTheSourceIsUnknown(t *testing.T) {
	dir := t.TempDir()

	executable = func() (string, error) { return "", errors.New("no executable") }
	t.Cleanup(func() { executable = os.Executable })

	require.Error(t, run(t, filepath.Join(dir, "redis-instance")))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
}

func TestBootstrapFailsWhenTheDestinationDirectoryIsMissing(t *testing.T) {
	dest := filepath.Join(t.TempDir(), "missing", "redis-instance")

	assert.Error(t, run(t, dest))
}

func TestBootstrapNeedsExactlyOneArgument(t *testing.T) {
	cmd := NewCmd()
	cmd.SetArgs(nil)

	assert.Error(t, cmd.Execute())
}

func TestBootstrapFailsWhenTheSourceIsGone(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	// A path that resolves but does not exist: os.Executable succeeds and the
	// open fails.
	executable = func() (string, error) { return filepath.Join(dir, "gone"), nil }
	t.Cleanup(func() { executable = os.Executable })

	require.Error(t, run(t, dest))
	assert.NoFileExists(t, dest)
}

func TestBootstrapFailsWhenTheSourceCannotBeRead(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	// Opening a directory succeeds and reading it does not, so this reaches
	// the copy itself rather than the open above it.
	source := filepath.Join(dir, "a-directory")
	require.NoError(t, os.Mkdir(source, 0o755))
	executable = func() (string, error) { return source, nil }
	t.Cleanup(func() { executable = os.Executable })

	require.Error(t, run(t, dest))

	assert.NoFileExists(t, dest)
	// A failed copy must take its temporary file with it.
	assert.Empty(t, tempFiles(t, dir), "a failed copy left a temporary file")
}

func TestBootstrapFailsWhenTheDestinationIsADirectory(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")
	require.NoError(t, os.Mkdir(dest, 0o755))

	require.Error(t, run(t, dest))
	assert.Empty(t, tempFiles(t, dir), "a failed rename left a temporary file")
}

func TestBootstrapLeavesNoTemporaryFileBehind(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "redis-instance")

	require.NoError(t, run(t, dest))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the directory must hold only the copy")
}
