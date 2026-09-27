//go:build unix

package upload

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A pipe put where a file was is refused at once: opening it does not wait
// for a writer that never comes.
func TestHashDoesNotWaitOnAPipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "invoice.pdf")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("no FIFO here: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := Hash(path)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || err.Error() != path+" is not a regular file" {
			t.Errorf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Hash waited on the pipe")
	}
}
