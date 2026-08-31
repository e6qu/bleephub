package bleephub

import (
	"context"
	"os"
	"time"

	"github.com/e6qu/bleephub/internal/store"
)

// startObjectReaper launches the opt-in periodic orphan-object sweep. It is OFF
// unless BLEEPHUB_OBJECT_REAPER is set:
//
//	BLEEPHUB_OBJECT_REAPER=report   list orphaned objects each pass (never deletes)
//	BLEEPHUB_OBJECT_REAPER=delete   also delete them
//	BLEEPHUB_OBJECT_REAPER_INTERVAL Go duration between passes (default 6h)
//	BLEEPHUB_OBJECT_REAPER_GRACE    minimum object age before it may be swept (default 24h)
//
// Report mode is the safe default posture: an operator can watch the counts and
// sample keys before ever enabling deletion.
func (s *Server) startObjectReaper(ctx context.Context) {
	mode := os.Getenv("BLEEPHUB_OBJECT_REAPER")
	if mode != "report" && mode != "delete" {
		return
	}
	interval := envDurationOr("BLEEPHUB_OBJECT_REAPER_INTERVAL", 6*time.Hour)
	grace := envDurationOr("BLEEPHUB_OBJECT_REAPER_GRACE", 24*time.Hour)
	opts := store.ReapOptions{Delete: mode == "delete", GracePeriod: grace}

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		s.runObjectReaperOnce(ctx, opts)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.runObjectReaperOnce(ctx, opts)
			}
		}
	}()
}

func (s *Server) runObjectReaperOnce(ctx context.Context, opts store.ReapOptions) {
	report, err := s.store.ReapOrphanObjects(ctx, opts)
	if err != nil {
		s.logger.Error().Err(err).Msg("object reaper pass failed")
		return
	}
	if !report.ObjectBacked {
		return
	}
	s.logger.Info().
		Bool("delete", opts.Delete).
		Int("scanned", report.Scanned).
		Int("orphans", report.OrphanCount).
		Int64("orphan_bytes", report.OrphanBytes).
		Int("deleted", report.DeletedCount).
		Int("delete_errors", report.DeleteErrors).
		Strs("sample", report.SampleKeys).
		Msg("object reaper pass")
}

func envDurationOr(name string, def time.Duration) time.Duration {
	if v := os.Getenv(name); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return def
}
