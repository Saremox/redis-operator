package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runMain runs main with args and returns the exit code that main gives.
// The code is -1 when main does not call exit.
func runMain(t *testing.T, args ...string) int {
	t.Helper()
	prevExit := exit
	t.Cleanup(func() {
		exit = prevExit
		rootCmd.SetArgs(nil)
	})

	code := -1
	exit = func(c int) { code = c }
	rootCmd.SetArgs(args)
	main()
	return code
}

func TestRootCmdRegistersSubcommands(t *testing.T) {
	var names []string
	for _, cmd := range rootCmd.Commands() {
		names = append(names, cmd.Name())
	}

	assert.Contains(t, names, "cleanup")
	assert.Contains(t, names, "run")
}

func TestMainRunsSubcommand(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "temp-1.rdb")
	require.NoError(t, os.WriteFile(stale, []byte("stale"), 0o600))

	code := runMain(t, "cleanup", "--data-dir", dir)

	assert.Equal(t, -1, code, "main must not exit on success")
	assert.NoFileExists(t, stale)
}

func TestMainExitsWithOneOnError(t *testing.T) {
	code := runMain(t, "no-such-command")

	assert.Equal(t, 1, code)
}
