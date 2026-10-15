//go:build darwin || linux

package supervisor

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// writeExactPath walks directory descriptors without following user symlinks.
// A fresh inode and descriptor-relative rename also avoid mutating hard-link
// aliases or following a leaf replaced after the check.
func writeExactPath(path string, data []byte) error {
	// macOS exposes its system temporary directory through /var. Resolve that
	// trusted process setting, then refuse symlinks in every requested component.
	if temp := filepath.Clean(os.TempDir()); strings.HasPrefix(path, temp+string(os.PathSeparator)) {
		canonical, err := filepath.EvalSymlinks(temp)
		if err != nil {
			return err
		}
		path = canonical + strings.TrimPrefix(path, temp)
	}
	parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Close(fd) }()
	for _, part := range parts[:len(parts)-1] {
		next, openErr := unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr == unix.ENOENT {
			if err := unix.Mkdirat(fd, part, 0o755); err != nil && err != unix.EEXIST {
				return err
			}
			next, openErr = unix.Openat(fd, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		}
		if openErr != nil {
			return fmt.Errorf("write parent %q must be a real directory: %w", part, openErr)
		}
		_ = unix.Close(fd)
		fd = next
	}
	name := parts[len(parts)-1]
	var stat unix.Stat_t
	mode := uint32(0o644)
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("write target must be a regular file")
		}
		mode = uint32(stat.Mode) & 0o777
	} else if err != unix.ENOENT {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	temporary := ".amac-write-" + hex.EncodeToString(nonce[:])
	fileFD, err := unix.Openat(fd, temporary, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
	if err != nil {
		return err
	}
	defer func() { _ = unix.Unlinkat(fd, temporary, 0) }()
	file := os.NewFile(uintptr(fileFD), temporary)
	_, writeErr := file.Write(data)
	if writeErr == nil {
		writeErr = file.Sync()
	}
	closeErr := file.Close()
	if writeErr != nil {
		return writeErr
	}
	if closeErr != nil {
		return closeErr
	}
	return unix.Renameat(fd, temporary, fd, name)
}
