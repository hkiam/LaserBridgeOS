// Command laserbridged bridges a TCP port to the GRBL controller's serial
// port. It is the appliance's own replacement for ser2net; for now it does
// the same job, transparently, and knows where the configuration lives.
package main

import (
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
)

const defaultSocket = "/run/laserbridge/laserbridged.sock"

func main() {
	logger := log.New(os.Stdout, "laserbridged: ", log.LstdFlags)
	if err := run(logger); err != nil {
		logger.Printf("%v", err)
		os.Exit(1)
	}
}

func run(logger *log.Logger) error {
	dataDir := getenv("LASERBRIDGE_DATA", "/data")
	configPath := getenv("LASERBRIDGE_CONFIG", filepath.Join(dataDir, "config.yaml"))
	socketPath := getenv("LASERBRIDGE_BRIDGE_SOCKET", defaultSocket)

	store := config.NewStore(configPath)
	// The appliance configuration is the only source of truth; the daemon
	// has no settings of its own and nothing to persist.
	cfg, err := store.Load()
	if err != nil {
		return err
	}

	bridge := proxy.New(proxy.Config{
		Device:   cfg.GRBL.Device,
		Baudrate: cfg.GRBL.Baudrate,
		Port:     cfg.GRBL.Port,
		// max_connections stays in the configuration for ser2net, which can
		// serve several. This bridge serves one on purpose: two applications
		// steering one laser is the failure it exists to prevent.
		KickOldClient: cfg.GRBL.KickOldUser,
	}, logger)

	done := make(chan struct{})
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-signals
		logger.Printf("shutting down")
		close(done)
	}()

	socket := proxy.NewStatusSocket(socketPath, bridge)
	go func() {
		if err := socket.Serve(done); err != nil {
			logger.Printf("status socket: %v", err)
		}
	}()

	logger.Printf("bridging %s at %d baud to TCP port %d",
		cfg.GRBL.Device, cfg.GRBL.Baudrate, cfg.GRBL.Port)
	return bridge.Run(done, nil)
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
