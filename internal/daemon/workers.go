package daemon

import (
	"context"
	"sync"
	"time"
)

// startWorkers launches the daemon's durable background loops for formation/auto-extraction,
// vector embedding backfill, feedback verification, meta-learning diagnosis, and maintenance compaction.
func (s *Service) startWorkers(ctx context.Context, wg *sync.WaitGroup) {
	wg.Go(func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := s.store.FormNext(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("form_memory failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})

	wg.Go(func() {
		ticker := time.NewTicker(500 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := s.store.EmbedNext(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("embed_memories failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})

	wg.Go(func() {
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			if err := s.store.VerifyNext(ctx); err != nil && ctx.Err() == nil {
				s.logger.Error("verify_feedback failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	})

	wg.Go(func() {
		diagnoseTick := time.NewTicker(2 * time.Second)
		defer diagnoseTick.Stop()
		compactTick := time.NewTicker(time.Hour)
		defer compactTick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-diagnoseTick.C:
				if err := s.store.Diagnose(ctx); err != nil && ctx.Err() == nil {
					s.logger.Error("diagnose failed", "error", err)
				}
			case <-compactTick.C:
				if err := s.store.CompactMaintenance(ctx); err != nil && ctx.Err() == nil {
					s.logger.Error("compact_maintenance failed", "error", err)
				}
			}
		}
	})
}
