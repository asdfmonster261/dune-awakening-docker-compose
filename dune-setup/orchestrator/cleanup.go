package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

// FarmStateCleanup periodically purges farm_state rows for game-server
// containers that are no longer running. The game-server binary writes its
// own row on startup and does NOT reliably mark alive=false on stop, so
// rows accumulate across restarts and the farm leader ends up seeing
// phantom peers.
//
// Cleanup criterion: a row's igw_addr column carries the container's docker
// network IP (e.g. "172.16.23.11/0"). On each tick we list the IPs of all
// currently-running game-server containers in the compose network and DELETE
// any row whose IP isn't in that set.
type FarmStateCleanup struct {
	db       *sql.DB
	docker   *DockerClient
	network  string
	interval time.Duration
}

// NewFarmStateCleanup connects to postgres and returns a ready-to-run
// cleaner. Returns nil + nil if the DSN env vars aren't set (cleanup
// disabled).
func NewFarmStateCleanup(docker *DockerClient, network string, interval time.Duration) (*FarmStateCleanup, error) {
	dsn := buildPostgresDSN()
	if dsn == "" {
		return nil, nil
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres open: %w", err)
	}
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(30 * time.Minute)
	return &FarmStateCleanup{
		db:       db,
		docker:   docker,
		network:  network,
		interval: interval,
	}, nil
}

// Run blocks until ctx is canceled, ticking every c.interval.
func (c *FarmStateCleanup) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.tick(ctx); err != nil {
				log.Printf("farm_state cleanup: %v", err)
			}
		}
	}
}

func (c *FarmStateCleanup) tick(ctx context.Context) error {
	running, err := c.docker.ListRunningGameServers(ctx, c.network)
	if err != nil {
		return fmt.Errorf("list game-servers: %w", err)
	}
	runningIPs := make(map[string]struct{}, len(running))
	for _, gs := range running {
		if gs.IP != "" {
			runningIPs[gs.IP] = struct{}{}
		}
	}
	// Defensive: if we couldn't find ANY running game-server, something is
	// wrong with docker enumeration — don't nuke the table.
	if len(runningIPs) == 0 {
		return nil
	}

	rows, err := c.db.QueryContext(ctx, "SELECT server_id, host(igw_addr) FROM farm_state")
	if err != nil {
		return fmt.Errorf("select farm_state: %w", err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var serverID, ip string
		if err := rows.Scan(&serverID, &ip); err != nil {
			return fmt.Errorf("scan: %w", err)
		}
		if _, ok := runningIPs[ip]; !ok {
			stale = append(stale, serverID)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("rows iteration: %w", err)
	}
	if len(stale) == 0 {
		return nil
	}

	res, err := c.db.ExecContext(ctx, "DELETE FROM farm_state WHERE server_id = ANY($1)", pgArray(stale))
	if err != nil {
		return fmt.Errorf("delete stale: %w", err)
	}
	n, _ := res.RowsAffected()
	log.Printf("farm_state cleanup: removed %d stale row(s) (running IPs: %d)", n, len(runningIPs))
	return nil
}

// buildPostgresDSN composes a DSN from env vars. Returns "" if the password
// isn't set (signals cleanup-disabled).
func buildPostgresDSN() string {
	pass := getenv("POSTGRES_DUNE_PASS", "")
	if pass == "" {
		return ""
	}
	host := getenv("POSTGRES_HOST", "postgres")
	port := getenv("POSTGRES_PORT", "5432")
	user := getenv("POSTGRES_USER", "dune")
	db := getenv("POSTGRES_DB", "dune")
	return fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable connect_timeout=10",
		host, port, user, pass, db)
}

// pgArray formats a Go string slice as a Postgres text[] literal so we can
// pass it to `WHERE server_id = ANY($1)` without depending on lib/pq's
// Array() helper (which would otherwise require importing the symbol into
// every call site).
func pgArray(xs []string) string {
	if len(xs) == 0 {
		return "{}"
	}
	escaped := make([]string, len(xs))
	for i, x := range xs {
		escaped[i] = `"` + strings.ReplaceAll(strings.ReplaceAll(x, `\`, `\\`), `"`, `\"`) + `"`
	}
	return "{" + strings.Join(escaped, ",") + "}"
}
