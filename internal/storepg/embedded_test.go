package storepg

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
)

// embeddedEnv lazily starts ONE embedded PostgreSQL for the whole test binary.
var (
	envOnce sync.Once
	envDSN  string
	envErr  error
	envStop func()
)

func startEmbedded(t *testing.T) string {
	t.Helper()
	envOnce.Do(func() {
		// Pick a free port rather than a fixed one: concurrent test runs (or
		// another agent building on this machine) must not fight over a port.
		port, err := freeTCPPort()
		if err != nil {
			envErr = fmt.Errorf("find free port: %w", err)
			return
		}
		dataDir := filepath.Join(`C:\`, "hindsight-go-pgtest", fmt.Sprintf("run-%d", os.Getpid()))
		pg := embeddedpostgres.NewDatabase(
			embeddedpostgres.DefaultConfig().
				Version(embeddedpostgres.V16).
				Port(port).
				Database("hindsight_go_test").
				Username("postgres").
				Password("postgres").
				DataPath(filepath.Join(dataDir, "data")).
				RuntimePath(filepath.Join(dataDir, "runtime")).
				Locale("C").
				Encoding("UTF8"),
		)
		if err := pg.Start(); err != nil {
			envErr = fmt.Errorf("start embedded postgres: %w", err)
			return
		}
		envDSN = fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/hindsight_go_test?sslmode=disable", port)
		envStop = func() { _ = pg.Stop() }
	})
	if envErr != nil {
		t.Fatalf("embedded postgres unavailable: %v", envErr)
	}
	t.Cleanup(func() {
		// Stop once when the last test finishes; sync.Once guarantees a single
		// server, and Stop is idempotent-safe here because it runs after all
		// tests in this binary have completed.
	})
	return envDSN
}

// freeTCPPort asks the OS for an unused localhost port.
func freeTCPPort() (uint32, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return uint32(l.Addr().(*net.TCPAddr).Port), nil
}

// TestMain stops the embedded server after the binary finishes.
func TestMain(m *testing.M) {
	code := m.Run()
	if envStop != nil {
		envStop()
	}
	os.Exit(code)
}
