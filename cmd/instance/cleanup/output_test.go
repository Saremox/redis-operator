package cleanup

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stdout returns what f prints on standard output.
func stdout(t *testing.T, f func() error) string {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	prev := os.Stdout
	os.Stdout = w
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	ferr := f()
	os.Stdout = prev
	require.NoError(t, w.Close())
	require.NoError(t, ferr)
	return <-out
}

// TestRunCleanupOutput pins the lines that cleanup prints for each entry and
// for the summary.
func TestRunCleanupOutput(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "temp-1.rdb")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))
	staleInfo, err := os.Stat(stale)
	require.NoError(t, err)
	entries := func(string) ([]fs.DirEntry, error) {
		return []fs.DirEntry{
			fakeDirEntry{name: "dump.rdb", info: staleInfo},
			fakeDirEntry{name: "notes.txt", info: staleInfo},
			fakeDirEntry{name: "no-info.rdb", infoErr: errors.New("injected info error")},
			fakeDirEntry{name: "gone.rdb", info: staleInfo},
			fakeDirEntry{name: "temp-1.rdb", info: staleInfo},
		}, nil
	}
	gone := filepath.Join(dir, "gone.rdb")

	setCleanupFlags(t, dir, "dump.rdb", true)
	readDir = entries
	assert.Equal(t, "Warning: failed to get info for no-info.rdb: injected info error\n"+
		"Would delete: "+gone+" (5 bytes)\n"+
		"Would delete: "+stale+" (5 bytes)\n"+
		"Would delete 2 stale RDB file(s), freed 10 bytes\n", stdout(t, func() error { return runCleanup(nil, nil) }))

	dryRun = false
	assert.Equal(t, "Warning: failed to get info for no-info.rdb: injected info error\n"+
		"Warning: failed to remove "+gone+": remove "+gone+": no such file or directory\n"+
		"Deleted: "+stale+" (5 bytes)\n"+
		"Deleted 1 stale RDB file(s), freed 5 bytes\n", stdout(t, func() error { return runCleanup(nil, nil) }))

	readDir = os.ReadDir
	assert.Equal(t, "No stale RDB files found\n", stdout(t, func() error { return runCleanup(nil, nil) }))

	dataDir = filepath.Join(dir, "missing")
	assert.Equal(t, "Data directory "+dataDir+" does not exist, skipping cleanup\n", stdout(t, func() error { return runCleanup(nil, nil) }))
}
