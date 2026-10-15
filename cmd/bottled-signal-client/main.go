package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"

	"github.com/cloud-in-a-bottle/bottled-bridge-core/bridge"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/config"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/events"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/files"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/store"
	"github.com/cloud-in-a-bottle/bottled-bridge-core/web"

	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/account"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/app"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/connector"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/exporter"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/importer"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/legacy"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/signalcli"
	"github.com/cloud-in-a-bottle/bottled-signal-client/internal/static"
)

const listenAddr = ":8080"

func main() {
	// Info, not the zerolog default of everything: debug and trace logs include message contents.
	log := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout, TimeFormat: time.DateTime, NoColor: true}).
		Level(zerolog.InfoLevel).With().Timestamp().Logger()
	zlog.Logger = log
	if err := run(log); err != nil {
		log.Fatal().Err(err).Msg("Exiting")
	}
}

func run(log zerolog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	signalBin := os.Getenv("SIGNAL_CLI_BIN")
	if signalBin == "" {
		return errors.New("required environment variable SIGNAL_CLI_BIN is not set")
	}
	// signal-cli's keys and state: losing them means registering again.
	signalDir := filepath.Join(cfg.DataDir, "signal-cli")
	// Files being sent, which signal-cli reads from disk.
	sendDir := filepath.Join(cfg.TempDir, "send")
	for _, dir := range []string{signalDir, sendDir} {
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}

	// The database of the app before it used the shared core has its own tables; set them aside so the store can
	// start, to be imported once the account is back.
	pending, err := legacy.SetAside(cfg.DBPath)
	if err != nil {
		return fmt.Errorf("setting the previous app's database aside: %w", err)
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	fs, err := files.New(cfg.AttachmentsDir())
	if err != nil {
		return err
	}
	broker := events.NewBroker()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	daemonDone := make(chan struct{})
	go func() {
		defer close(daemonDone)
		signalcli.RunDaemon(ctx, signalBin, signalDir, log.With().Str("component", "signal-cli").Logger())
	}()

	rpc := signalcli.NewClient(signalcli.BaseURL)
	conn := connector.New(rpc, signalBin, signalDir, sendDir)
	svc, err := bridge.New(app.BridgeOptions(conn), cfg.BridgeDBPath, st, fs, broker, log)
	if err != nil {
		return err
	}
	handler, err := web.New(web.Options{
		Config: cfg, Service: svc, Store: st, Files: fs, Broker: broker, Branding: app.Branding, Static: static.FS,
		Log: log, Messaging: &app.Messaging,
	})
	if err != nil {
		return err
	}
	account.Routes(handler, svc, rpc)
	imports, err := importer.NewManager(cfg.ImportsDir(), cfg.AttachmentsDir(), svc, st, fs, broker, log.With().Str("component", "import").Logger())
	if err != nil {
		return err
	}
	imports.Routes(handler)
	exports, err := exporter.NewManager(filepath.Join(cfg.TempDir, "exports"), signalDir, svc, st, fs, broker, log.With().Str("component", "export").Logger())
	if err != nil {
		return err
	}
	exports.Routes(handler)

	srv := &http.Server{
		Addr:    listenAddr,
		Handler: handler,
		// Ends open event streams on shutdown.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}
	serveErr := make(chan error, 1)
	go func() {
		serveErr <- srv.ListenAndServe()
	}()
	log.Info().Str("addr", listenAddr).Msg("Serving")
	if err = svc.Start(ctx); err != nil {
		return err
	}
	if pending {
		go legacy.ImportWhenLoggedIn(ctx, cfg.DBPath, svc, rpc, st, fs, broker, log.With().Str("component", "legacy").Logger())
	}

	select {
	case <-ctx.Done():
	case err = <-serveErr:
		return err
	}
	log.Info().Msg("Shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(shutdownCtx)
	svc.Stop()
	<-daemonDone
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
