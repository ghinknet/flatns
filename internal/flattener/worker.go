// Package flattener contains the reconciliation loop that turns a CNAME source
// into a flat set of A/AAAA records on a target subdomain. One Worker runs per
// configured flatten entry, each in its own goroutine.
package flattener

import (
	"cmp"
	"context"
	"slices"
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

// buildDesired groups resolved addresses into the desired value sets per type,
// then applies the entry's record-count limits (see capType and capTotal).
func (w *Worker) buildDesired(res resolver.Result) map[provider.RecordType][]string {
	v4 := w.capType(provider.RecordTypeA, res.IPv4)
	var v6 []string
	if w.cfg.IPv6 {
		v6 = w.capType(provider.RecordTypeAAAA, res.IPv6)
	}
	v4, v6 = w.capTotal(v4, v6)

	desired := map[provider.RecordType][]string{
		provider.RecordTypeA: v4,
	}
	if w.cfg.IPv6 {
		desired[provider.RecordTypeAAAA] = v6
	}
	return desired
}

// capType enforces the per-type MaxRecords limit. The resolver returns
// addresses sorted and de-duplicated, so keeping the first MaxRecords is
// deterministic: the same resolved set produces the same kept set across
// cycles, so the cap never causes create/delete churn. A limit of 0 means
// unlimited. Dropped addresses are logged at warn level so an operator can see
// that the provider's record quota is truncating the record set.
func (w *Worker) capType(rt provider.RecordType, values []string) []string {
	limit := w.cfg.MaxRecords
	if limit <= 0 || len(values) <= limit {
		return values
	}
	w.log.Warn("resolved addresses exceed max_records, dropping surplus",
		zap.String("type", string(rt)),
		zap.Int("resolved", len(values)),
		zap.Int("max_records", limit),
		zap.Strings("dropped", values[limit:]),
	)
	return values[:limit]
}

// capTotal enforces MaxRecordsTotal across both types together, for providers
// whose quota counts A and AAAA records against one budget. The budget is
// split evenly between the two types, either type's unused share flows to the
// other, and IPv4 receives the odd slot since v4 reachability is universal.
func (w *Worker) capTotal(v4, v6 []string) ([]string, []string) {
	limit := w.cfg.MaxRecordsTotal
	if limit <= 0 || len(v4)+len(v6) <= limit {
		return v4, v6
	}
	keep6 := min(len(v6), limit-min(len(v4), (limit+1)/2))
	keep4 := min(len(v4), limit-keep6)
	w.log.Warn("resolved addresses exceed max_records_total, dropping surplus",
		zap.Int("resolved", len(v4)+len(v6)),
		zap.Int("max_records_total", limit),
		zap.Strings("dropped_v4", v4[keep4:]),
		zap.Strings("dropped_v6", v6[keep6:]),
	)
	return v4[:keep4], v6[:keep6]
}

// reconcileType brings the provider's managed records of a single type into
// agreement with the desired value set. It only ever touches records carrying
// this entry's managed marker, leaving user records untouched.
//
// Value changes are applied by repointing stale records in place via
// UpdateRecord rather than create-then-delete. Providers cap how many records
// may coexist on one subdomain (e.g. DNSPod's LimitExceeded.SubdomainRollLimit
// on plans whose quota equals max_records), so creating the new set while the
// old one still exists can be rejected outright — and deleting the old set
// first would leave the name unresolvable until the creates land. In-place
// updates keep the record count constant, so neither failure mode can occur.
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

	// Desired values not present yet, in the resolver's deterministic order.
	var missing []string
	for _, value := range desiredValues {
		if _, ok := managed[value]; !ok {
			missing = append(missing, value)
		}
	}

	// Managed records whose value is no longer desired. Sorted by value so the
	// stale/missing pairing is stable across cycles (managed is a map), which
	// lets a failed update retry the same pair instead of reshuffling.
	var stale []provider.Record
	for value, rec := range managed {
		if _, ok := desiredSet[value]; !ok {
			stale = append(stale, rec)
		}
	}
	slices.SortFunc(stale, func(a, b provider.Record) int {
		return cmp.Compare(a.Value, b.Value)
	})

	// Repoint stale records at missing values in place. On failure the stale
	// record is left serving (and the value left uncreated) for the next cycle,
	// so a rejected update never shrinks the live record set.
	for len(missing) > 0 && len(stale) > 0 {
		rec, value := stale[0], missing[0]
		stale, missing = stale[1:], missing[1:]
		oldValue := rec.Value
		rec.Value = value
		rec.TTL = w.cfg.TTL
		rec.Remark = marker
		if err := w.provider.UpdateRecord(ctx, rec); err != nil {
			w.log.Error("update record failed", zap.String("type", string(rt)), zap.String("old_value", oldValue), zap.String("value", value), zap.Error(err))
			continue
		}
		w.log.Info("updated record", zap.String("type", string(rt)), zap.String("old_value", oldValue), zap.String("value", value))
	}

	// Create records for desired values beyond what updates could absorb.
	for _, value := range missing {
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

	// Delete leftover stale records that no missing value claimed.
	for _, rec := range stale {
		if err := w.provider.DeleteRecord(ctx, w.cfg.Domain, rec.ID); err != nil {
			w.log.Error("delete record failed", zap.String("type", string(rt)), zap.String("value", rec.Value), zap.Error(err))
			continue
		}
		w.log.Info("deleted stale record", zap.String("type", string(rt)), zap.String("value", rec.Value))
	}

	// Records keeping their value only need the TTL held in sync.
	for value, rec := range managed {
		if _, ok := desiredSet[value]; !ok || rec.TTL == w.cfg.TTL {
			continue
		}
		rec.TTL = w.cfg.TTL
		rec.Remark = marker
		if err := w.provider.UpdateRecord(ctx, rec); err != nil {
			w.log.Error("update record TTL failed", zap.String("type", string(rt)), zap.String("value", value), zap.Error(err))
			continue
		}
		w.log.Info("updated record ttl", zap.String("type", string(rt)), zap.String("value", value), zap.Uint64("ttl", w.cfg.TTL))
	}

	return nil
}
