package bootstrap

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

// destMode is the mode of the copy. The Redis container runs the copy as its
// entrypoint, so it needs the execute bits.
const destMode = 0o755

// executable is a variable so that tests can inject a source fault.
var executable = os.Executable

// NewCmd creates the bootstrap command.
func NewCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "bootstrap <destination>",
		Short: "Copy this binary to a destination path",
		Long: `Copy the running redis-instance binary to a destination path.

This lets an init container put the binary on a volume that another container
shares, so that container can run it without the binary being in its own image.

The binary copies itself, so the image that holds it needs no shell and no cp
and can stay distroless.`,
		Args: cobra.ExactArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			return bootstrap(args[0])
		},
	}
}

// bootstrap copies the running binary to dest. It writes a temporary file in
// the directory of dest and renames it, so that dest never holds a partial
// copy that the Redis container could try to run.
func bootstrap(dest string) error {
	src, err := executable()
	if err != nil {
		return fmt.Errorf("failed to find the running binary: %w", err)
	}

	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open %s: %w", src, err)
	}
	defer func() { _ = in.Close() }()

	tmp, err := os.CreateTemp(filepath.Dir(dest), filepath.Base(dest)+".tmp")
	if err != nil {
		return fmt.Errorf("failed to create a temporary file beside %s: %w", dest, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	// Keep the first of the three failures and close the file in every case,
	// so that a failed copy leaks no descriptor.
	written, err := io.Copy(tmp, in)
	if err == nil {
		err = tmp.Chmod(destMode)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("failed to write %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, dest); err != nil {
		return fmt.Errorf("failed to rename %s to %s: %w", tmpName, dest, err)
	}

	fmt.Printf("Copied %s to %s (%d bytes)\n", src, dest, written)
	return nil
}
