// Package flattener contains the reconciliation loop that turns a CNAME source
// into a flat set of A/AAAA records on a target subdomain. One Worker runs per
// configured flatten entry, each in its own goroutine.
package flattener

import (
	"context"
	"time"

	"flatns/internal/infra/config"
	"flatns/internal/infra/logger"
	"flatns/internal/provider"
	"flatns/internal/resolver"

	"go.uber.org/zap"
)

// Worker reconciles a single flatten entry on a fixed interval.
type Worker struct {
	cfg      config.FlattenConfig
	provider provider.Provider
	resolver *resolver.Resolver
	log      *zap.Logger
}

// NewWorker constructs a Worker. The provider and resolver are created by the
// caller (which owns provider de-duplication across entries). The worker logs
// through the global logger with the entry's identity attached.
func NewWorker(cfg config.FlattenConfig, p provider.Provider, r *resolver.Resolver) *Worker {
	return &Worker{
		cfg:      cfg,
		provider: p,
		resolver: r,
		log: logger.L.With(
			zap.String("flatten", cfg.Name),
			zap.String("instance", cfg.Instance),
			zap.String("domain", cfg.Domain),
			zap.String("sub_domain", cfg.SubDomain),
		),
	}
}

// Run executes the reconciliation loop until ctx is cancelled. It performs an
// immediate first pass and then repeats every configured interval.
func (w *Worker) Run(ctx context.Context) {
	w.log.Info("worker started",
		zap.String("source", w.cfg.Source),
		zap.Duration("interval", w.cfg.Interval),
		zap.Bool("ipv6", w.cfg.IPv6),
	)

	// Run once immediately so the records are correct without waiting a full
	// interval after start-up.
	w.reconcileOnce(ctx)

	ticker := time.NewTicker(w.cfg.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			w.log.Info("worker stopped")
			return
		case <-ticker.C:
			w.reconcileOnce(ctx)
		}
	}
}

// reconcileOnce performs a single resolve-diff-apply cycle, logging but not
// propagating errors so that a transient failure never kills the loop.
func (w *Worker) reconcileOnce(ctx context.Context) {
	resolved, err := w.resolver.Resolve(ctx, w.cfg.Source, w.cfg.IPv6)
	if err != nil {
		w.log.Warn("resolve failed, skipping cycle", zap.Error(err))
		return
	}

	desired := w.buildDesired(resolved)

	if err := w.reconcileType(ctx, provider.RecordTypeA, desired[provider.RecordTypeA]); err != nil {
		w.log.Error("reconcile A records failed", zap.Error(err))
	}
	if w.cfg.IPv6 {
		if err := w.reconcileType(ctx, provider.RecordTypeAAAA, desired[provider.RecordTypeAAAA]); err != nil {
			w.log.Error("reconcile AAAA records failed", zap.Error(err))
		}
	}
}

// buildDesired groups resolved addresses into the desired value sets per type.
func (w *Worker) buildDesired(res resolver.Result) map[provider.RecordType][]string {
	desired := map[provider.RecordType][]string{
		provider.RecordTypeA: res.IPv4,
	}
	if w.cfg.IPv6 {
		desired[provider.RecordTypeAAAA] = res.IPv6
	}
	return desired
}

// reconcileType brings the provider's managed records of a single type into
// agreement with the desired value set. It only ever touches records carrying
// this entry's managed marker, leaving user records untouched.
func (w *Worker) reconcileType(ctx context.Context, rt provider.RecordType, desiredValues []string) error {
	existing, err := w.provider.ListRecords(ctx, w.cfg.Domain, w.cfg.SubDomain, []provider.RecordType{rt})
	if err != nil {
		return err
	}

	marker := provider.BuildManagedRemark(provider.ManagedRemark{
		Instance: w.cfg.Instance,
		Source:   w.cfg.Source,
	})

	// Partition existing records into ones we manage (matching our instance and
	// source marker) and everything else, which we must never modify.
	managed := make(map[string]provider.Record) // value -> record
	for _, rec := range existing {
		m, ok := provider.ParseManagedRemark(rec.Remark)
		if !ok || m.Instance != w.cfg.Instance || m.Source != w.cfg.Source {
			continue
		}
		managed[rec.Value] = rec
	}

	desiredSet := make(map[string]struct{}, len(desiredValues))
	for _, v := range desiredValues {
		desiredSet[v] = struct{}{}
	}

	// Create records for desired values that are not yet present.
	for _, value := range desiredValues {
		if _, ok := managed[value]; ok {
			continue
		}
		rec := provider.Record{
			Domain:    w.cfg.Domain,
			SubDomain: w.cfg.SubDomain,
			Type:      rt,
			Value:     value,
			TTL:       w.cfg.TTL,
			Remark:    marker,
		}
		if _, err := w.provider.CreateRecord(ctx, rec); err != nil {
			w.log.Error("create record failed", zap.String("type", string(rt)), zap.String("value", value), zap.Error(err))
			continue
		}
		w.log.Info("created record", zap.String("type", string(rt)), zap.String("value", value))
	}

	// Delete managed records whose value is no longer desired.
	for value, rec := range managed {
		if _, ok := desiredSet[value]; ok {
			// Value still desired; ensure TTL is in sync.
			if rec.TTL != w.cfg.TTL {
				rec.TTL = w.cfg.TTL
				rec.Remark = marker
				if err := w.provider.UpdateRecord(ctx, rec); err != nil {
					w.log.Error("update record TTL failed", zap.String("type", string(rt)), zap.String("value", value), zap.Error(err))
					continue
				}
				w.log.Info("updated record ttl", zap.String("type", string(rt)), zap.String("value", value), zap.Uint64("ttl", w.cfg.TTL))
			}
			continue
		}
		if err := w.provider.DeleteRecord(ctx, w.cfg.Domain, rec.ID); err != nil {
			w.log.Error("delete record failed", zap.String("type", string(rt)), zap.String("value", value), zap.Error(err))
			continue
		}
		w.log.Info("deleted stale record", zap.String("type", string(rt)), zap.String("value", value))
	}

	return nil
}
