package main

import (
	"context"
	"log"
)

// reconcileOnDemandOnStartup syncs ServerSetScale state with docker reality
// when the orchestrator (re)starts. The store loses its scale state on
// restart — loadWorld pulls from world.yaml, where on-demand maps have
// replicas=0 and partitions=[]. If a player was on an on-demand map when
// the orchestrator crashed, the container survived (compose has
// restart="no" for on-demand services and doesn't touch them), and the
// freshly-loaded scale would report 0 while the container keeps running.
// Without recovery, the director's next "player left, scale 1→0" PATCH
// ends up as a 0→0 no-op and the container is never stopped — silent leak.
//
// Recovery rule: for each scale whose initial replicas is 0 (i.e. on-demand)
// AND whose matching compose service has a currently-running container,
// bump store.replicas to 1 AND populate store.partitions from the
// BattleGroup CR's worldPartitions[] section for that map. Both fields are
// needed:
//   - replicas=1 so the director's "1→0" scale-down later actually does
//     something (otherwise it's a no-op).
//   - partitions=[<id>] so the director's "brown in IGWO" check (Battle-
//     group.cs:185-235) sees the live server's partition_id in the spec
//     and doesn't filter the server out of the ClassicalInstancing pool.
//     Without partitions, the running container would be invisible to the
//     director and unreachable for travel — the exact symptom that Bug 2
//     produces post-restart.
//
// We deliberately do NOT try to start containers that "should" be running
// but aren't — we don't have authoritative pre-restart desired state, and
// the director's reconciliation will PATCH us back to 1 when a player needs
// the map. We also keep on-demand maps with no running container at the
// fresh `partitions=[]` state — that's required for the director's
// SetInstanceCount to find a partition to add when scaling 0→1 (otherwise
// Patch_SetInstanceCount bails with "Found no partitions not running").
func reconcileOnDemandOnStartup(ctx context.Context, store *Store, docker *DockerClient) {
	if docker == nil {
		return
	}
	running, err := docker.ListRunningGameServers(ctx, "")
	if err != nil {
		log.Printf("on-demand recovery: list running: %v", err)
		return
	}
	runningSet := make(map[string]struct{}, len(running))
	for _, gs := range running {
		runningSet[gs.Service] = struct{}{}
	}

	// Build the map-name → []partition-id lookup from BattleGroup CR.
	// Used to repopulate scale.spec.partitions for running on-demand
	// containers (see top-of-file comment).
	partitionsByMap := extractWorldPartitionIDs(store.GetBattleGroup())

	scales := store.ListScales()
	var recovered int
	for _, sc := range scales {
		// Always-on maps have replicas>0 in world.yaml; skip — compose owns
		// their lifecycle, recovery doesn't apply.
		if getInt(sc.Spec, "replicas", 0) > 0 {
			continue
		}
		mapName := sc.Metadata.Annotations["igw.funcom.com/map-name"]
		if mapName == "" {
			continue
		}
		service := "game-server-" + safeMapName(mapName)
		if _, ok := runningSet[service]; !ok {
			continue
		}
		// Container is running but the freshly-loaded scale says replicas=0.
		// Take fresh copies of the inner maps before mutating — ListScales
		// only does a shallow struct copy and the Spec/Status maps are
		// shared with the store.
		updated := sc
		updated.Spec = cloneMap(sc.Spec)
		updated.Status = cloneMap(sc.Status)
		updated.Spec["replicas"] = 1
		updated.Status["replicas"] = 1
		updated.Status["readyReplicas"] = 1
		// Restore partitions from the BG CR so the director sees this server
		// as not-brown. Without this, post-restart on-demand maps stay
		// invisible to the ClassicalInstancing pool.
		if parts := partitionsByMap[mapName]; len(parts) > 0 {
			updated.Spec["partitions"] = parts
		}
		store.SetScale(sc.Metadata.Name, &updated)
		log.Printf("on-demand recovery: scale %s synced from running container %s (replicas 0→1, partitions=%v)",
			sc.Metadata.Name, service, updated.Spec["partitions"])
		recovered++
	}
	log.Printf("on-demand recovery: scanned %d running game-server(s), recovered %d on-demand scale(s)",
		len(running), recovered)
}

func cloneMap(m map[string]interface{}) map[string]interface{} {
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
