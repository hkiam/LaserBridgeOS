package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/laserbridgeos/laserbridgeos/backend/internal/api"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/config"
	"github.com/laserbridgeos/laserbridgeos/backend/internal/proxy"
	lbruntime "github.com/laserbridgeos/laserbridgeos/backend/internal/runtime"
	lbupdate "github.com/laserbridgeos/laserbridgeos/backend/internal/update"
)

var version = "development"

func main() {
	if err := run(os.Args[1:]); err != nil {
		log.Printf("laserbridge: %v", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	dataDir := getenv("LASERBRIDGE_DATA", "/data")
	configPath := getenv("LASERBRIDGE_CONFIG", filepath.Join(dataDir, "config.yaml"))
	runtimeDir := getenv("LASERBRIDGE_RUNTIME", "/run/laserbridge")
	store := config.NewStore(configPath)
	runner := lbruntime.ExecRunner{}
	manager := &lbruntime.Manager{Store: store, Dir: runtimeDir, DataDir: dataDir, Run: runner}
	if len(args) == 0 {
		return errors.New("usage: laserbridge <serve|init|apply|run-ustreamer|ssh-enabled|boot-confirm|grbl-backend|grbl-status|grbl-journal>")
	}
	switch args[0] {
	case "init":
		return manager.InitData()
	case "apply":
		return manager.Apply()
	case "run-ustreamer":
		return manager.RunUstreamer()
	case "boot-confirm":
		return newUpdater(manager).ConfirmBoot()
	case "grbl-status":
		// Reads the bridge's own account of itself over its Unix socket.
		// Nothing else may touch the serial port, so this is how the rest of
		// the appliance finds out what the bridge is doing.
		status, err := proxy.ReadStatus(getenv("LASERBRIDGE_BRIDGE_SOCKET", "/run/laserbridge/laserbridged.sock"))
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(status, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	case "grbl-journal":
		// What went wrong lately, newest last. Reads the daemon's record over
		// the same socket the web interface uses.
		events, err := proxy.ReadJournal(getenv("LASERBRIDGE_BRIDGE_SOCKET", "/run/laserbridge/laserbridged.sock"), 0)
		if err != nil {
			return err
		}
		encoded, err := json.MarshalIndent(events, "", "  ")
		if err != nil {
			return err
		}
		fmt.Println(string(encoded))
		return nil
	case "grbl-backend":
		// Both GRBL backends sit in the default runlevel; each asks this
		// whether it is the one that should run. Exactly one may own the
		// serial port, and the configuration decides which.
		if len(args) < 2 {
			return errors.New("usage: laserbridge grbl-backend <ser2net|laserbridged>")
		}
		cfg, err := store.Load()
		if err != nil {
			return err
		}
		if cfg.GRBL.Backend != args[1] {
			return fmt.Errorf("GRBL backend is %q, not %q", cfg.GRBL.Backend, args[1])
		}
		return nil
	case "ssh-enabled":
		cfg, err := store.Load()
		if err != nil {
			return err
		}
		if !cfg.SSH.Enabled {
			return errors.New("SSH is disabled in appliance configuration")
		}
		return nil
	case "serve":
		listen := ":80"
		webRoot := "/usr/share/laserbridge/web"
		for i := 1; i < len(args); i++ {
			switch args[i] {
			case "--listen":
				i++
				if i >= len(args) {
					return errors.New("--listen needs a value")
				}
				listen = args[i]
			case "--web-root":
				i++
				if i >= len(args) {
					return errors.New("--web-root needs a value")
				}
				webRoot = args[i]
			default:
				return fmt.Errorf("unknown argument %q", args[i])
			}
		}
		quarantined, err := store.Ensure()
		if err != nil {
			return err
		}
		if quarantined != "" {
			log.Printf("laserbridge: unreadable configuration moved to %s; defaults restored", quarantined)
		}
		// Without root the appliance cannot drive services, so the API is
		// served read-only rather than reporting failures for every command.
		if os.Geteuid() != 0 {
			manager.Run = nil
			return serve(listen, webRoot, store, manager, nil)
		}
		return serve(listen, webRoot, store, manager, runner)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func newUpdater(manager *lbruntime.Manager) *lbupdate.Manager {
	return &lbupdate.Manager{
		DataDir:     manager.DataDir,
		RuntimeDir:  manager.Dir,
		VersionPath: getenv("LASERBRIDGE_VERSION_FILE", "/etc/laserbridge/version"),
	}
}

func serve(listen, webRoot string, store *config.Store, manager *lbruntime.Manager, runner lbruntime.Runner) error {
	logger := log.New(os.Stdout, "laserbridge-web: ", log.LstdFlags)
	updater := newUpdater(manager)
	versionPath := updater.VersionPath
	bridgeSocket := getenv("LASERBRIDGE_BRIDGE_SOCKET", "/run/laserbridge/laserbridged.sock")

	// The GRBL bridge is the process whose hanging matters most and the one
	// thing it cannot notice about itself. This is a different process, so it
	// can. Only with the privileges to do something about it.
	var watch *lbruntime.BridgeWatch
	if runner != nil {
		watch = &lbruntime.BridgeWatch{
			Store: store, Runner: runner, SocketPath: bridgeSocket, Logger: logger,
			// Shorter than the default: the status page asks this on every
			// poll, and a page that takes two seconds to load because the
			// bridge is wedged is the page you need at that moment.
			Timeout: time.Second,
		}
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		go watch.Watch(ctx)
	}

	server := &http.Server{
		Addr:              listen,
		Handler:           (&api.Server{Store: store, Runtime: manager, Runner: runner, Updater: updater, WebRoot: filepath.Clean(webRoot), VersionPath: versionPath, BridgeSocket: bridgeSocket, BridgeWatch: watch, Logger: logger}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Minute,
		WriteTimeout:      10 * time.Minute,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
	}
	errorsCh := make(chan error, 1)
	go func() {
		logger.Printf("version %s listening on %s", version, listen)
		errorsCh <- server.ListenAndServe()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	select {
	case <-signals:
		// A system update can be several hundred megabytes in flight; give it
		// a chance to finish writing a slot instead of cutting it in half.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return server.Shutdown(ctx)
	case err := <-errorsCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
