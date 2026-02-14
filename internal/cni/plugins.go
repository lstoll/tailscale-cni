package cni

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Plugins built in the container and copied to the host so the CRI can find them.
var pluginNames = []string{"host-local", "bridge", "portmap", "loopback"}

// CopyPlugins copies the bridge, host-local, and portmap CNI plugin binaries
// from sourceDir to destDir. destDir is created if it does not exist.
// Typically sourceDir is the path inside the container (e.g. /opt/cni/bin)
// and destDir is the host plugin directory mounted into the container.
func CopyPlugins(sourceDir, destDir string) error {
	if err := os.MkdirAll(destDir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", destDir, err)
	}
	for _, name := range pluginNames {
		src := filepath.Join(sourceDir, name)
		dst := filepath.Join(destDir, name)
		if err := copyFile(src, dst, 0755); err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return nil
}

// copyFile copies src to dst by streaming and sets mode. It overwrites dst if it exists.
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Chmod(mode)
}
