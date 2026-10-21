package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// newSFTPClient opens an SFTP subsystem on the given SSH client. The caller
// is responsible for closing the returned client.
func newSFTPClient(client *ssh.Client) (*sftp.Client, error) {
	return sftp.NewClient(client)
}

// expandRemotePath resolves a leading "~" or "~/" against the SFTP working
// directory (the login user's home). The interactive shell expands tilde, but
// SFTP paths are passed to the server verbatim, so without this an agent
// asking to upload to "~/app" would create a literal "~" directory instead.
func expandRemotePath(client *sftp.Client, remotePath string) (string, error) {
	if remotePath != "~" && !strings.HasPrefix(remotePath, "~/") {
		return remotePath, nil
	}
	home, err := client.Getwd()
	if err != nil {
		return "", fmt.Errorf("failed to resolve remote home directory: %w", err)
	}
	if remotePath == "~" {
		return home, nil
	}
	return path.Join(home, remotePath[2:]), nil
}

// expandLocalPath resolves a leading "~/" (or "~\" on Windows) against the
// local user's home directory. Windows shells do not expand tilde, so a path
// copied from a Unix-style instruction would otherwise fail to stat.
func expandLocalPath(localPath string) string {
	if localPath != "~" && !strings.HasPrefix(localPath, "~/") && !strings.HasPrefix(localPath, `~\`) {
		return localPath
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return localPath
	}
	if localPath == "~" {
		return home
	}
	return filepath.Join(home, localPath[2:])
}

// hasTrailingSeparator reports whether p ends with a path separator, which is
// the conventional way of saying "this path is a directory".
func hasTrailingSeparator(p string) bool {
	return len(p) > 0 && (p[len(p)-1] == '/' || p[len(p)-1] == '\\')
}

// remoteIsDir stats p over SFTP and reports whether it exists and whether it
// is a directory. A missing path is not an error: callers treat it as "the
// destination does not exist yet".
func remoteIsDir(client *sftp.Client, p string) (exists, isDir bool, err error) {
	info, err := client.Stat(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("failed to stat remote path %s: %w", p, err)
	}
	return true, info.IsDir(), nil
}

// destIsContainer reports whether a single-file transfer should be placed
// inside the destination directory rather than written to the destination
// path itself. That is the case when the destination already is a directory or
// was written with a trailing separator -- the same rule scp/cp use.
func destIsContainer(destIsDir, trailingSep bool) bool {
	return destIsDir || trailingSep
}

// Upload copies a local path (file or directory) to a remote destination on
// the given session's host. Directories are copied recursively. A single
// file is placed inside remotePath when that already is a directory or was
// written with a trailing slash, matching scp/cp semantics.
func (m *SSHManager) Upload(sessionID, localPath, remotePath string) (string, error) {
	session, err := m.requireActive(sessionID)
	if err != nil {
		return "", err
	}

	client, err := newSFTPClient(session.client)
	if err != nil {
		return "", fmt.Errorf("failed to start sftp: %w", err)
	}
	defer client.Close()

	localPath = expandLocalPath(localPath)
	// Only '/' is a separator remotely; a trailing backslash is a valid
	// filename character on a Unix host and must not imply a directory.
	remoteTrailing := strings.HasSuffix(remotePath, "/")
	remotePath, err = expandRemotePath(client, remotePath)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(localPath)
	if err != nil {
		return "", fmt.Errorf("failed to stat local path: %w", err)
	}

	if info.IsDir() {
		count, err := uploadDir(client, localPath, remotePath)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Uploaded %d files from %s to %s", count, localPath, remotePath), nil
	}

	_, destIsDir, err := remoteIsDir(client, remotePath)
	if err != nil {
		return "", err
	}
	intoDir := destIsContainer(destIsDir, remoteTrailing)
	target := remotePath
	if intoDir {
		if err := client.MkdirAll(remotePath); err != nil {
			return "", fmt.Errorf("failed to create remote directory: %w", err)
		}
		target = path.Join(remotePath, filepath.Base(localPath))
	}

	if err := uploadFile(client, localPath, target, info.Mode()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Uploaded %s to %s", localPath, target), nil
}

// Download copies a remote path (file or directory) to a local destination
// from the given session's host. Directories are copied recursively. A single
// file is placed inside localPath when that already is a directory or was
// written with a trailing separator, matching scp/cp semantics.
func (m *SSHManager) Download(sessionID, remotePath, localPath string) (string, error) {
	session, err := m.requireActive(sessionID)
	if err != nil {
		return "", err
	}

	client, err := newSFTPClient(session.client)
	if err != nil {
		return "", fmt.Errorf("failed to start sftp: %w", err)
	}
	defer client.Close()

	remotePath, err = expandRemotePath(client, remotePath)
	if err != nil {
		return "", err
	}
	localTrailing := hasTrailingSeparator(localPath)
	localPath = expandLocalPath(localPath)

	info, err := client.Stat(remotePath)
	if err != nil {
		return "", fmt.Errorf("failed to stat remote path: %w", err)
	}

	if info.IsDir() {
		count, err := downloadDir(client, remotePath, localPath)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Downloaded %d files from %s to %s", count, remotePath, localPath), nil
	}

	_, destIsDir, err := localIsDir(localPath)
	if err != nil {
		return "", err
	}
	intoDir := destIsContainer(destIsDir, localTrailing)
	target := localPath
	if intoDir {
		if err := os.MkdirAll(localPath, 0o755); err != nil {
			return "", fmt.Errorf("failed to create local directory: %w", err)
		}
		target = filepath.Join(localPath, path.Base(remotePath))
	}

	if err := downloadFile(client, remotePath, target, info.Mode()); err != nil {
		return "", err
	}
	return fmt.Sprintf("Downloaded %s to %s", remotePath, target), nil
}

// localIsDir stats localPath and reports whether it exists and is a directory.
// A missing path is not an error: the destination will be created.
func localIsDir(localPath string) (exists, isDir bool, err error) {
	info, err := os.Stat(localPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, false, nil
		}
		return false, false, fmt.Errorf("failed to stat local path %s: %w", localPath, err)
	}
	return true, info.IsDir(), nil
}

func uploadFile(client *sftp.Client, localPath, remotePath string, mode os.FileMode) error {
	src, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("failed to open local file: %w", err)
	}
	defer src.Close()

	// path.Dir, not filepath.Dir: remote paths use forward slashes even when
	// this server runs on Windows.
	if dir := path.Dir(remotePath); dir != "." {
		if err := client.MkdirAll(dir); err != nil {
			return fmt.Errorf("failed to create remote directory: %w", err)
		}
	}

	// Write-only is more widely supported than Create's read+write handle
	// (some SFTP servers refuse to open a file for both at once).
	dst, err := client.OpenFile(remotePath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
	if err != nil {
		return fmt.Errorf("failed to create remote file: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("failed to copy file contents: %w", err)
	}

	// Close flushes pending SFTP writes; ignoring its error would report a
	// truncated upload as successful.
	if err := dst.Close(); err != nil {
		return fmt.Errorf("failed to finalize remote file: %w", err)
	}

	if mode.Perm() != 0 {
		if err := client.Chmod(remotePath, mode); err != nil {
			return fmt.Errorf("failed to set remote file mode: %w", err)
		}
	}

	return nil
}

func uploadDir(client *sftp.Client, localRoot, remoteRoot string) (int, error) {
	if err := client.MkdirAll(remoteRoot); err != nil {
		return 0, fmt.Errorf("failed to create remote directory: %w", err)
	}

	// Resolve a symlinked source root: filepath.Walk uses Lstat and would
	// treat the link itself as a file instead of descending into it.
	resolved, err := filepath.EvalSymlinks(localRoot)
	if err != nil {
		return 0, &TransferError{Op: "upload", Path: localRoot, Err: fmt.Errorf("failed to resolve local path: %w", err)}
	}
	visited := map[string]bool{resolved: true}
	return uploadTree(client, resolved, remoteRoot, visited)
}

// uploadTree copies one local directory and recurses into subdirectories.
// Symlinks are followed (like scp -r); visited holds resolved paths so a
// symlink cycle cannot make the walk loop forever.
func uploadTree(client *sftp.Client, localDir, remoteDir string, visited map[string]bool) (int, error) {
	entries, err := os.ReadDir(localDir)
	if err != nil {
		return 0, &TransferError{Op: "upload", Path: localDir, Err: err}
	}

	count := 0
	for _, entry := range entries {
		localPath := filepath.Join(localDir, entry.Name())
		remotePath := path.Join(remoteDir, entry.Name())

		// Stat follows symlinks, so a link to a directory is copied as a
		// directory and a link to a file as that file's contents.
		info, err := os.Stat(localPath)
		if err != nil {
			return count, &TransferError{Op: "upload", Transferred: count, Path: localPath, Err: err}
		}

		if info.IsDir() {
			real, err := filepath.EvalSymlinks(localPath)
			if err != nil {
				return count, &TransferError{Op: "upload", Transferred: count, Path: localPath, Err: fmt.Errorf("failed to resolve local path: %w", err)}
			}
			if visited[real] {
				continue
			}
			visited[real] = true

			if err := client.MkdirAll(remotePath); err != nil {
				return count, &TransferError{Op: "upload", Transferred: count, Path: localPath, Err: fmt.Errorf("failed to create remote directory: %w", err)}
			}
			n, err := uploadTree(client, localPath, remotePath, visited)
			count += n
			if err != nil {
				return count, err
			}
			continue
		}

		if err := uploadFile(client, localPath, remotePath, info.Mode()); err != nil {
			return count, &TransferError{Op: "upload", Transferred: count, Path: localPath, Err: err}
		}
		count++
	}

	return count, nil
}

func downloadFile(client *sftp.Client, remotePath, localPath string, mode os.FileMode) error {
	src, err := client.Open(remotePath)
	if err != nil {
		return fmt.Errorf("failed to open remote file: %w", err)
	}
	defer src.Close()

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("failed to create local directory: %w", err)
	}

	dst, err := os.Create(localPath)
	if err != nil {
		return fmt.Errorf("failed to create local file: %w", err)
	}

	if _, err := io.Copy(dst, src); err != nil {
		_ = dst.Close()
		return fmt.Errorf("failed to copy file contents: %w", err)
	}

	// Close reports buffered-write/fsync failures that would otherwise make a
	// truncated download look successful.
	if err := dst.Close(); err != nil {
		return fmt.Errorf("failed to finalize local file: %w", err)
	}

	if mode.Perm() != 0 {
		if err := os.Chmod(localPath, mode); err != nil {
			return fmt.Errorf("failed to set local file mode: %w", err)
		}
	}

	return nil
}

func downloadDir(client *sftp.Client, remoteRoot, localRoot string) (int, error) {
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return 0, fmt.Errorf("failed to create local directory: %w", err)
	}

	// Download already decided remoteRoot is a directory via Stat, which
	// follows symlinks. Resolve the root so an Lstat-based traversal does not
	// mistake a symlinked directory for a file.
	resolved, err := client.RealPath(remoteRoot)
	if err != nil {
		resolved = remoteRoot
	}
	visited := map[string]bool{resolved: true}
	return downloadTree(client, resolved, localRoot, visited)
}

// downloadTree copies one remote directory and recurses into subdirectories.
// Symlinks are followed and visited holds resolved paths, so links that point
// back up the tree cannot cause infinite recursion.
func downloadTree(client *sftp.Client, remoteDir, localDir string, visited map[string]bool) (int, error) {
	entries, err := client.ReadDir(remoteDir)
	if err != nil {
		return 0, &TransferError{Op: "download", Path: remoteDir, Err: err}
	}

	count := 0
	for _, entry := range entries {
		remotePath := path.Join(remoteDir, entry.Name())
		localPath := filepath.Join(localDir, entry.Name())

		info := entry
		if info.Mode()&os.ModeSymlink != 0 {
			// Follow the link so the local tree mirrors what the shell sees
			// on the remote host.
			info, err = client.Stat(remotePath)
			if err != nil {
				return count, &TransferError{Op: "download", Transferred: count, Path: remotePath, Err: err}
			}
		}

		if info.IsDir() {
			real, err := client.RealPath(remotePath)
			if err != nil {
				real = remotePath
			}
			if visited[real] {
				continue
			}
			visited[real] = true

			mode := info.Mode().Perm()
			if mode == 0 {
				mode = 0o755
			}
			// Keep the owner writable while populating the tree; the remote
			// mode (which may be read-only) is applied once it is complete.
			if err := os.MkdirAll(localPath, mode|0o700); err != nil {
				return count, &TransferError{Op: "download", Transferred: count, Path: remotePath, Err: fmt.Errorf("failed to create local directory: %w", err)}
			}
			n, err := downloadTree(client, remotePath, localPath, visited)
			count += n
			if err != nil {
				return count, err
			}
			_ = os.Chmod(localPath, mode)
			continue
		}

		if err := downloadFile(client, remotePath, localPath, info.Mode()); err != nil {
			return count, &TransferError{Op: "download", Transferred: count, Path: remotePath, Err: err}
		}
		count++
	}

	return count, nil
}

// ReadFile reads a remote file over SFTP and returns its content as a string,
// without going through the interactive shell or the emulator. offset is the
// starting byte; length caps how many bytes are returned (0 means "up to
// maxRemoteReadBytes"). The result is truncated to maxRemoteReadBytes
// regardless, and the truncated flag reports whether more data remains beyond
// what was returned.
func (m *SSHManager) ReadFile(sessionID, remotePath string, offset, length int64) (content string, truncated bool, err error) {
	session, err := m.requireActive(sessionID)
	if err != nil {
		return "", false, err
	}

	client, err := newSFTPClient(session.client)
	if err != nil {
		return "", false, fmt.Errorf("failed to start sftp: %w", err)
	}
	defer client.Close()

	f, err := client.Open(remotePath)
	if err != nil {
		return "", false, fmt.Errorf("failed to open remote file: %w", err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return "", false, fmt.Errorf("failed to seek remote file: %w", err)
		}
	}

	limit := int64(maxRemoteReadBytes)
	if length > 0 && length < limit {
		limit = length
	}

	// Read one extra byte to detect whether more data remains.
	buf := make([]byte, limit+1)
	n, err := io.ReadFull(f, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return "", false, fmt.Errorf("failed to read remote file: %w", err)
	}

	if int64(n) > limit {
		return string(buf[:limit]), true, nil
	}
	// Only a complete read (from the start, not truncated) satisfies the
	// read-before-edit guard, so a partial slice can't unlock a blind edit.
	if offset == 0 {
		session.recordRead(remotePath)
	}
	return string(buf[:n]), false, nil
}

// WriteFile writes content to a remote file over SFTP, creating parent
// directories as needed and truncating any existing file. It does not touch
// the interactive shell or emulator.
func (m *SSHManager) WriteFile(sessionID, remotePath, content string) (string, error) {
	session, err := m.requireActive(sessionID)
	if err != nil {
		return "", err
	}

	client, err := newSFTPClient(session.client)
	if err != nil {
		return "", fmt.Errorf("failed to start sftp: %w", err)
	}
	defer client.Close()

	if dir := filepath.ToSlash(filepath.Dir(remotePath)); dir != "." && dir != "" {
		if err := client.MkdirAll(dir); err != nil {
			return "", fmt.Errorf("failed to create remote directory: %w", err)
		}
	}

	f, err := client.Create(remotePath)
	if err != nil {
		return "", fmt.Errorf("failed to create remote file: %w", err)
	}
	defer f.Close()

	n, err := f.Write([]byte(content))
	if err != nil {
		return "", fmt.Errorf("failed to write remote file: %w", err)
	}

	return fmt.Sprintf("Wrote %d bytes to %s", n, remotePath), nil
}
