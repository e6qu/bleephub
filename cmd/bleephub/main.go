package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/e6qu/bleephub/internal/server"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

var (
	version     = "development"
	commit      = "none"
	publishedAt = "not-yet-published"
)

// obsFlushGrace bounds the telemetry flush at shutdown. The exporter's endpoint
// may be the thing that has gone away, and a flush that blocks on it forever is
// indistinguishable from a hang.
const obsFlushGrace = 5 * time.Second

// main does nothing but choose the exit status. Every deferred cleanup lives in
// run, because a deferred call and os.Exit in the same function means the
// cleanup never runs — which is how telemetry came to be dropped on every exit
// path, the ordinary one included.
func main() {
	if err := run(); err != nil {
		bootLogger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr})
		bootLogger.Error().Err(err).Msg("bleephub exited")
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", ":5555", "listen address")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	flag.Parse()

	level, err := zerolog.ParseLevel(*logLevel)
	if err != nil {
		return fmt.Errorf("invalid --log-level %q (want debug, info, warn or error): %w", *logLevel, err)
	}

	obs, err := bleephub.InitObservability("bleephub")
	if err != nil {
		return err
	}
	defer func() {
		flush, cancel := context.WithTimeout(context.Background(), obsFlushGrace)
		defer cancel()
		_ = obs.Shutdown(flush)
	}()

	// Default to structured JSON, which log pipelines can parse. ANSI console
	// output is human-friendly for local development but noise in production, so
	// it is opt-in via BLEEPHUB_LOG_FORMAT=console.
	var base io.Writer = os.Stderr
	if strings.EqualFold(os.Getenv("BLEEPHUB_LOG_FORMAT"), "console") {
		base = zerolog.ConsoleWriter{Out: os.Stderr}
	}
	var output zerolog.LevelWriter
	if obs.LogWriter != nil {
		output = zerolog.MultiLevelWriter(base, obs.LogWriter)
	} else {
		output = zerolog.MultiLevelWriter(base)
	}
	logger := zerolog.New(output).
		With().Timestamp().Str("service", "bleephub").Logger().
		Level(level)
	// Route the few package-level boot/fatal loggers (persistence quorum wait,
	// AdminToken requirement) through the same configured logger.
	zlog.Logger = logger

	logger.Info().Str("version", version).Str("commit", commit).Msg("starting")

	// SIGTERM is what a container runtime sends first. Without a handler the
	// process runs as PID 1, ignores it, and is SIGKILLed after the runtime's
	// grace period with every in-flight request cut mid-response.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	srv := bleephub.NewServer(*addr, logger, bleephub.WithBuildInfo(bleephub.BuildInfo{
		Version: version, Commit: commit, PublishedAt: publishedAt,
	}))
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info().Msg("stopped")
	return nil
}
