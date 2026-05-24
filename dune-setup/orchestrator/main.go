// HTTPS server that impersonates a Kubernetes API server for the
// igw.funcom.com/v1 BattleGroup CR and its subresources. Implements:
//   - GET  /apis/igw.funcom.com/v1/namespaces/{ns}/battlegroups/{name}
//   - GET  /apis/igw.funcom.com/v1/namespaces/{ns}/serversetscales[/{name}]
//   - PATCH /apis/igw.funcom.com/v1/namespaces/{ns}/serverstats/{name}
//   - PATCH /apis/igw.funcom.com/v1/namespaces/{ns}/battlegroupdirectorstats/{name}
//   - PATCH /apis/igw.funcom.com/v1/namespaces/{ns}/battlegroups/{name}
//   - PATCH /apis/igw.funcom.com/v1/namespaces/{ns}/serversetscales/{name}
//
// Patches are JSON Merge Patch (RFC 7396). State is in memory; restart reloads
// from /world.yaml.
package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := getenv("LISTEN_ADDR", ":6443")
	certPath := getenv("TLS_CERT", "/certs/cert.pem")
	keyPath := getenv("TLS_KEY", "/certs/key.pem")
	worldPath := getenv("WORLD_PATH", "/world.yaml")

	bg, scales, err := loadWorld(worldPath)
	if err != nil {
		log.Fatalf("load world: %v", err)
	}
	if err := validateJSONable(bg); err != nil {
		log.Fatalf("world.yaml has non-JSON-serializable content: %v", err)
	}

	store := NewStore()
	store.SetBattleGroup(bg)
	for name, sc := range scales {
		store.SetScale(name, sc)
	}
	annotated := 0
	for _, sc := range scales {
		if sc.Metadata.Annotations["igw.funcom.com/map-name"] != "" {
			annotated++
		}
	}
	log.Printf("Loaded BattleGroup %q (ns=%s) with %d ServerSetScales (%d carry igw.funcom.com/map-name)",
		bg.Metadata.Name, bg.Metadata.Namespace, len(scales), annotated)

	// Docker client for on-demand map containers (Phase 4). Disabled if the
	// socket isn't mounted — in that case PATCH on replicas updates state but
	// doesn't start/stop containers.
	var docker *DockerClient
	if _, err := os.Stat("/var/run/docker.sock"); err == nil {
		project := getenv("COMPOSE_PROJECT_NAME", "dune-server")
		var err error
		docker, err = NewDockerClient(project)
		if err != nil {
			log.Printf("docker client init failed; on-demand maps disabled: %v", err)
			docker = nil
		} else {
			log.Printf("Docker socket present; on-demand map start/stop enabled (project=%s)", project)
		}
	} else {
		log.Printf("Docker socket not mounted; on-demand map start/stop disabled")
	}

	hostIP := getenv("HOST_IP", "")
	if hostIP != "" {
		log.Printf("Node API will report HOST_IP=%s", hostIP)
	}
	server := &Server{store: store, docker: docker, hostIP: hostIP}

	// Recover on-demand scale state from docker reality (see recovery.go).
	// Must run before HTTP starts so the director's first poll sees the
	// reconciled view, not the stale world.yaml defaults. Bounded so we
	// can't hang startup if docker is unresponsive.
	if docker != nil {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.Background(), 15*time.Second)
		reconcileOnDemandOnStartup(recoveryCtx, store, docker)
		recoveryCancel()
	}

	// Stale farm_state cleanup. Only runs if we have both the docker socket
	// (to enumerate running containers) and a postgres password (to DELETE
	// rows). Otherwise silently disabled.
	cleanupCtx, cleanupCancel := context.WithCancel(context.Background())
	defer cleanupCancel()
	if docker != nil {
		network := getenv("DOCKER_NETWORK", getenv("COMPOSE_PROJECT_NAME", "dune-server")+"_default")
		cleanup, err := NewFarmStateCleanup(docker, network, 60*time.Second)
		if err != nil {
			log.Printf("farm_state cleanup init failed: %v", err)
		} else if cleanup == nil {
			log.Printf("farm_state cleanup disabled (POSTGRES_DUNE_PASS not set)")
		} else {
			log.Printf("farm_state cleanup enabled (network=%s, interval=60s)", network)
			go cleanup.Run(cleanupCtx)
		}
	}

	// On-demand crash recovery (see crash_recovery.go). Detects exited
	// containers for scales the director currently believes are at
	// replicas=1, and rolls them back to 0 so the next travel request
	// triggers a fresh scale-up.
	if docker != nil {
		cr := NewCrashRecovery(store, docker, 60*time.Second)
		log.Printf("on-demand crash recovery enabled (interval=60s)")
		go cr.Run(cleanupCtx)
	}

	mux := http.NewServeMux()
	mux.Handle("/", server)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
	}

	// Run the server in a goroutine so we can intercept SIGTERM and drain
	// in-flight requests instead of dropping them when `docker compose down`
	// kills us. The 5s drain budget stays safely under docker's default 10s
	// stop_grace_period — beyond that the daemon SIGKILLs us anyway.
	errCh := make(chan error, 1)
	go func() {
		log.Printf("dune-orchestrator: listening on %s (cert=%s)", addr, certPath)
		if err := srv.ListenAndServeTLS(certPath, keyPath); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	select {
	case err := <-errCh:
		log.Fatalf("server crashed: %v", err)
	case sig := <-sigCh:
		log.Printf("received %s; draining in-flight requests (5s)", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			log.Printf("graceful shutdown error: %v", err)
		}
		log.Printf("shutdown complete")
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
