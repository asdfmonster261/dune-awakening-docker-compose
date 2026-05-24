package main

import (
	"context"
	"log"
	"time"
)

// CrashRecovery detects on-demand game-server containers that have died
// unexpectedly (SIGSEGV, OOM, panic, etc.) and rolls the matching
// ServerSetScale back to replicas=0 + empty partitions. The next time a
// player requests travel to that map, the director's SetInstanceCount
// PATCH will set replicas=0→1 again, which triggers a fresh docker start
// on the (still-exited) container.
//
// This complements two existing pieces:
//   - recovery.go handles the "orchestrator restarted, container still
//     running" case (replicas was 0 in world.yaml, container is alive).
//   - cleanup.go handles the "container gone, farm_state row left
//     behind" case (database hygiene).
//
// The gap this fixes: container died while the orchestrator was up
// (so recovery.go doesn't see it) and the scale stays stuck at
// replicas=1. Without rollback, the director's next scale PATCH is
// a no-op (1→1 = nothing) and nobody ever restarts the container.
type CrashRecovery struct {
	store    *Store
	docker   *DockerClient
	interval time.Duration
	// lastSeen tracks the last tick we saw each game-server service in the
	// running set. Used as a per-service grace window: we only flag a crash
	// after we've actually observed the container running at least once.
	// Without this, a just-scaled-up container that hasn't reached "running"
	// state yet would be falsely flagged.
	lastSeen map[string]time.Time
}

func NewCrashRecovery(store *Store, docker *DockerClient, interval time.Duration) *CrashRecovery {
	return &CrashRecovery{
		store:    store,
		docker:   docker,
		interval: interval,
		lastSeen: make(map[string]time.Time),
	}
}

// Run blocks until ctx is canceled, ticking every c.interval.
func (c *CrashRecovery) Run(ctx context.Context) {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := c.tick(ctx); err != nil {
				log.Printf("crash recovery: %v", err)
			}
		}
	}
}

func (c *CrashRecovery) tick(ctx context.Context) error {
	running, err := c.docker.ListRunningGameServers(ctx, "")
	if err != nil {
		return err
	}
	now := time.Now()
	runningSet := make(map[string]struct{}, len(running))
	for _, gs := range running {
		runningSet[gs.Service] = struct{}{}
		c.lastSeen[gs.Service] = now
	}

	scales := c.store.ListScales()
	for _, sc := range scales {
		if sc.Metadata.Annotations["igw.funcom.com/on-demand"] != "true" {
			continue
		}
		if getInt(sc.Spec, "replicas", 0) < 1 {
			continue
		}
		mapName := sc.Metadata.Annotations["igw.funcom.com/map-name"]
		if mapName == "" {
			continue
		}
		service := "game-server-" + safeMapName(mapName)
		if _, ok := runningSet[service]; ok {
			continue
		}
		// Container is missing. Grace check: only flag a crash if we've
		// previously seen the container running. Otherwise it's either
		// still starting up (just-PATCHed) or docker start hasn't been
		// called yet — both fine to skip this tick.
		if _, seen := c.lastSeen[service]; !seen {
			continue
		}

		updated := sc
		updated.Spec = cloneMap(sc.Spec)
		updated.Status = cloneMap(sc.Status)
		updated.Spec["replicas"] = 0
		updated.Spec["partitions"] = []int{}
		updated.Status["replicas"] = 0
		updated.Status["readyReplicas"] = 0
		c.store.SetScale(sc.Metadata.Name, &updated)
		delete(c.lastSeen, service)
		log.Printf("crash recovery: %s container missing, scaled %s 1→0 (director will re-spawn on next allocation)",
			service, sc.Metadata.Name)
	}
	return nil
}
