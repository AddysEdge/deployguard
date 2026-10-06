//go:build !windows

package main

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestBinarySIGINT sends a real SIGINT to the compiled CLI during a run.
// (Windows cannot deliver SIGINT to a child process programmatically; the
// in-process cancellation test covers the same code path there.)
func TestBinarySIGINT(t *testing.T) {
	var hits atomic.Int64
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	}))
	defer slow.Close()
	slow2 := httptest.NewServer(slow.Config.Handler) // a distinct origin with the same behavior
	defer slow2.Close()

	reportPath := filepath.Join(t.TempDir(), "sigint.json")
	cmd := exec.Command(binPath, "compare", "--config", example("deployguard.yaml"), "--baseline", slow.URL, "--candidate", slow2.URL, "--report", reportPath)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	begin := time.Now()
	if err := cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 130 {
		t.Fatalf("want exit 130 after SIGINT, got %v", err)
	}
	if time.Since(begin) > 3*time.Second {
		t.Fatalf("shutdown after SIGINT took %s", time.Since(begin))
	}
	rep := readReport(t, reportPath)
	if rep["run"].(map[string]any)["canceled"] != true {
		t.Fatal("report should record the cancellation")
	}
}
