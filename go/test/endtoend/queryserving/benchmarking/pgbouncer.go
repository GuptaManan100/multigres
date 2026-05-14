// Copyright 2026 Supabase, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package benchmarking

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/multigres/multigres/go/test/utils"
	"github.com/multigres/multigres/go/tools/executil"
)

// PgBouncerInstance manages a pgbouncer process for benchmark comparison.
type PgBouncerInstance struct {
	process   *executil.Cmd
	cancel    context.CancelFunc
	configDir string
	port      int
}

// PgBouncerOption configures a PgBouncerInstance at construction time.
type PgBouncerOption func(*pgBouncerConfig)

type pgBouncerConfig struct {
	// resetQueryAlways adds `server_reset_query_always = 1` to pgbouncer.ini.
	// Off by default (matches pgbouncer's own default), which means in
	// transaction-pooling mode the reset query (DISCARD ALL) is NOT run
	// when a backend is returned to the pool. That's correct for
	// benchmarks (extra overhead, no behavioral benefit), but it's a trap
	// for session-state demos: with low client contention, the same client
	// often gets the same backend back after COMMIT and observes its temp
	// table as if it survived. Enabling this forces DISCARD ALL on every
	// release so the temp-table teardown is observable from a single
	// client — the same behavior production hits randomly under churn.
	resetQueryAlways bool
}

// WithServerResetQueryAlways enables `server_reset_query_always = 1` in the
// generated pgbouncer.ini. Required for session-state demos to behave
// deterministically; see the comment on pgBouncerConfig.resetQueryAlways.
func WithServerResetQueryAlways() PgBouncerOption {
	return func(c *pgBouncerConfig) {
		c.resetQueryAlways = true
	}
}

// NewPgBouncerInstance starts a pgbouncer instance pointing at the given PostgreSQL backend.
// Returns nil, nil if pgbouncer is not installed (caller should skip pgbouncer benchmarks).
func NewPgBouncerInstance(t *testing.T, backendHost string, backendPort int, user, password string, opts ...PgBouncerOption) (*PgBouncerInstance, error) {
	t.Helper()

	if !pgbouncerAvailable() {
		return nil, nil //nolint:nilnil // nil,nil signals "not available, skip gracefully"
	}

	cfg := &pgBouncerConfig{}
	for _, opt := range opts {
		opt(cfg)
	}

	port := utils.GetFreePort(t)
	configDir := t.TempDir()

	resetQueryAlwaysLine := ""
	if cfg.resetQueryAlways {
		resetQueryAlwaysLine = "server_reset_query_always = 1\n"
	}

	// Write pgbouncer.ini
	// Use scram-sha-256 auth to match PostgreSQL's default auth method.
	iniContent := fmt.Sprintf(`[databases]
postgres = host=%s port=%d dbname=postgres

[pgbouncer]
listen_addr = 127.0.0.1
listen_port = %d
auth_type = scram-sha-256
auth_file = %s/userlist.txt
pool_mode = transaction
%smax_client_conn = 200
default_pool_size = 20
log_connections = 0
log_disconnections = 0
admin_users = %s
pidfile = %s/pgbouncer.pid
logfile = %s/pgbouncer.log
unix_socket_dir = %s
`, backendHost, backendPort, port,
		configDir, resetQueryAlwaysLine, user, configDir, configDir, configDir)

	iniPath := filepath.Join(configDir, "pgbouncer.ini")
	if err := os.WriteFile(iniPath, []byte(iniContent), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write pgbouncer.ini: %w", err)
	}

	// Write userlist.txt with plaintext password.
	// pgbouncer handles the SCRAM-SHA-256 exchange itself when given plaintext.
	userlistContent := fmt.Sprintf(`"%s" "%s"`, user, password)
	userlistPath := filepath.Join(configDir, "userlist.txt")
	if err := os.WriteFile(userlistPath, []byte(userlistContent), 0o644); err != nil {
		return nil, fmt.Errorf("failed to write userlist.txt: %w", err)
	}

	// Start pgbouncer
	ctx, cancel := context.WithCancel(context.Background())
	process := executil.Command(ctx, "pgbouncer", iniPath)

	logFile := filepath.Join(configDir, "pgbouncer-stdout.log")
	f, err := os.Create(logFile)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create pgbouncer log file: %w", err)
	}
	process.SetStdout(f).SetStderr(f)

	if err := process.Start(); err != nil {
		cancel()
		f.Close()
		return nil, fmt.Errorf("failed to start pgbouncer: %w", err)
	}

	inst := &PgBouncerInstance{
		process:   process,
		cancel:    cancel,
		configDir: configDir,
		port:      port,
	}

	// Wait for pgbouncer to accept connections
	if err := waitForPort(t, "127.0.0.1", port, 10*time.Second); err != nil {
		inst.Stop(t)
		return nil, fmt.Errorf("pgbouncer did not become ready: %w", err)
	}

	t.Logf("pgbouncer started on port %d (config: %s)", port, iniPath)
	return inst, nil
}

// Port returns the listen port of this pgbouncer instance.
func (p *PgBouncerInstance) Port() int {
	return p.port
}

// Stop terminates the pgbouncer process.
func (p *PgBouncerInstance) Stop(t *testing.T) {
	t.Helper()
	if p == nil || p.process == nil {
		return
	}
	p.cancel()
	_ = p.process.Wait()
	t.Logf("pgbouncer stopped")
}

// pgbouncerAvailable returns true if the pgbouncer binary is on PATH.
func pgbouncerAvailable() bool {
	_, err := exec.LookPath("pgbouncer")
	return err == nil
}

// waitForPort polls a TCP port until it accepts connections or the timeout expires.
func waitForPort(t *testing.T, host string, port int, timeout time.Duration) error {
	t.Helper()

	addr := net.JoinHostPort(host, strconv.Itoa(port))
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("port %s not ready after %v", addr, timeout)
}
