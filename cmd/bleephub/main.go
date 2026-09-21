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

	"github.com/e6qu/bleephub/internal/gitbackend"
	"github.com/e6qu/bleephub/internal/server"
	"github.com/rs/zerolog"
	zlog "github.com/rs/zerolog/log"
)

var (
	version     = "development"
	commit      = "none"
	publishedAt = "not-yet-published"
)

// obsFlushGrace bounds the telemetry flush at shutdown, in case the exporter's
// endpoint has gone away and a flush would block forever.
const obsFlushGrace = 5 * time.Second

// main only chooses the exit status; cleanup lives in run, since os.Exit skips
// deferred calls in the same function.
func main() {
	if err := run(); err != nil {
		bootLogger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr})
		bootLogger.Error().Err(err).Msg("bleephub exited")
		os.Exit(1)
	}
}

// adoptCommand is the subcommand an operator runs once against an object store
// written before repositories had manifests. See gitbackend.Adopt.
const adoptCommand = "adopt"

func run() error {
	if len(os.Args) > 1 && os.Args[1] == adoptCommand {
		return runAdopt(os.Args[2:])
	}
	return serve()
}

// runAdopt is `bleephub adopt`: it reads the same BLEEPHUB_* storage settings the
// server does, so that it adopts the store the server will then run on.
func runAdopt(arguments []string) error {
	flags := flag.NewFlagSet(adoptCommand, flag.ContinueOnError)
	repository := flags.String("repository", "", "adopt this repository (owner/repo) only; every repository under the git prefix otherwise")
	removeOldLayout := flags.Bool("remove-old-layout", false, "instead of adopting, delete the reference objects and supersession markers of repositories that have a manifest")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return gitbackend.Adopt(ctx, gitbackend.AdoptRequest{Repository: *repository, RemoveOldLayout: *removeOldLayout}, os.Stdout)
}

func serve() error {
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

	// Default to structured JSON; opt into ANSI console via BLEEPHUB_LOG_FORMAT=console.
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
	// Route the package-level boot/fatal loggers through the same configured logger.
	zlog.Logger = logger

	logger.Info().Str("version", version).Str("commit", commit).Msg("starting")

	// Handle SIGTERM so a container runtime can drain in-flight requests before
	// its grace period elapses and it SIGKILLs PID 1.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	monitoring, err := bleephub.MonitoringTokenFromEnvironment()
	if err != nil {
		return err
	}
	srv := bleephub.NewServer(*addr, logger, bleephub.WithBuildInfo(bleephub.BuildInfo{
		Version: version, Commit: commit, PublishedAt: publishedAt,
	}), monitoring)
	if err := srv.ListenAndServe(ctx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	logger.Info().Msg("stopped")
	return nil
}
