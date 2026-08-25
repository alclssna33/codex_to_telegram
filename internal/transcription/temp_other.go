//go:build !windows

package transcription

import "os"

func restrictPrivateTempDir(path string) error {
	return os.Chmod(path, 0o700)
}
