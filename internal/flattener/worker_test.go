package flattener

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"testing"

	"flatns/internal/infra/config"
	"flatns/internal/provider"
	"flatns/internal/resolver"

	"go.uber.org/zap"
)

// newTestWorker builds a Worker with the given limits and a no-op logger, so
// buildDesired can be exercised without a live provider or global logger.
func newTestWorker(ipv6 bool, maxRecords, maxTotal int) *Worker {
	ipv4 := true
	return &Worker{
		cfg: config.FlattenConfig{
			IPv4:            &ipv4,
			IPv6:            ipv6,
			MaxRecords:      maxRecords,
			MaxRecordsTotal: maxTotal,
		},
		log: zap.NewNop(),
	}
}

func TestBuildDesiredIPv4Disabled(t *testing.T) {
	ipv4 := false
	w := &Worker{cfg: config.FlattenConfig{IPv4: &ipv4, IPv6: true}, log: zap.NewNop()}
	got := w.buildDesired(resolver.Result{IPv4: []string{"1.1.1.1"}, IPv6: []string{"::1"}})
	if len(got[provider.RecordTypeA]) != 0 {
		t.Fatalf("A records should be disabled: %v", got)
	}
	if !reflect.DeepEqual(got[provider.RecordTypeAAAA], []string{"::1"}) {
		t.Fatalf("AAAA records = %v", got[provider.RecordTypeAAAA])
	}
}

func TestBuildDesiredLimits(t *testing.T) {
	v4 := []string{"1.1.1.1", "2.2.2.2", "3.3.3.3"}
	v6 := []string{"::1", "::2", "::3"}

	tests := []struct {
		name       string
		ipv6       bool
		maxRecords int
		maxTotal   int
		res        resolver.Result
		wantV4     []string
		wantV6     []string
	}{
		{
			name:   "no limit keeps all",
			ipv6:   true,
			res:    resolver.Result{IPv4: v4, IPv6: v6},
			wantV4: v4,
			wantV6: v6,
		},
		{
			name:       "per-type cap applies to each type",
			ipv6:       true,
			maxRecords: 2,
			res:        resolver.Result{IPv4: v4, IPv6: v6},
			wantV4:     []string{"1.1.1.1", "2.2.2.2"},
			wantV6:     []string{"::1", "::2"},
		},
		{
			name:       "per-type cap below count truncates ipv4 only entry",
			ipv6:       false,
			maxRecords: 2,
			res:        resolver.Result{IPv4: v4},
			wantV4:     []string{"1.1.1.1", "2.2.2.2"},
			wantV6:     nil,
		},
		{
			name:     "total cap splits evenly across types",
			ipv6:     true,
			maxTotal: 2,
			res:      resolver.Result{IPv4: v4, IPv6: v6},
			wantV4:   []string{"1.1.1.1"},
			wantV6:   []string{"::1"},
		},
		{
			name:     "total cap gives odd slot to ipv4",
			ipv6:     true,
			maxTotal: 3,
			res:      resolver.Result{IPv4: v4, IPv6: v6},
			wantV4:   []string{"1.1.1.1", "2.2.2.2"},
			wantV6:   []string{"::1"},
		},
		{
			name:     "total cap reallocates unused v6 budget to v4",
			ipv6:     true,
			maxTotal: 4,
			res:      resolver.Result{IPv4: v4, IPv6: []string{"::1"}},
			wantV4:   v4,
			wantV6:   []string{"::1"},
		},
		{
			name:       "both limits: per-type applies before total",
			ipv6:       true,
			maxRecords: 2,
			maxTotal:   3,
			res:        resolver.Result{IPv4: v4, IPv6: v6},
			wantV4:     []string{"1.1.1.1", "2.2.2.2"},
			wantV6:     []string{"::1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newTestWorker(tt.ipv6, tt.maxRecords, tt.maxTotal)
			got := w.buildDesired(tt.res)
			if !reflect.DeepEqual(got[provider.RecordTypeA], tt.wantV4) {
				t.Errorf("A records = %v, want %v", got[provider.RecordTypeA], tt.wantV4)
			}
			if !reflect.DeepEqual(got[provider.RecordTypeAAAA], tt.wantV6) {
				t.Errorf("AAAA records = %v, want %v", got[provider.RecordTypeAAAA], tt.wantV6)
			}
		})
	}
}

// fakeProvider is an in-memory Provider that counts mutations and tracks the
// peak number of concurrently existing records, so tests can assert that
// reconciliation never needs the old and new record sets to coexist (which is
// what trips per-subdomain record quotas like DNSPod's SubdomainRollLimit).
type fakeProvider struct {
	records map[string]provider.Record // id -> record
	nextID  int
	peak    int

	creates, updates, deletes int
	failUpdate                bool
}

func newFakeProvider(existing ...provider.Record) *fakeProvider {
	f := &fakeProvider{records: make(map[string]provider.Record)}
	for _, rec := range existing {
		f.nextID++
		rec.ID = strconv.Itoa(f.nextID)
		f.records[rec.ID] = rec
	}
	f.peak = len(f.records)
	return f
}

func (f *fakeProvider) Name() string { return "fake" }

func (f *fakeProvider) ListRecords(context.Context, string, string, []provider.RecordType) ([]provider.Record, error) {
	var out []provider.Record
	for _, rec := range f.records {
		out = append(out, rec)
	}
	return out, nil
}

func (f *fakeProvider) CreateRecord(_ context.Context, rec provider.Record) (provider.Record, error) {
	f.creates++
	f.nextID++
	rec.ID = strconv.Itoa(f.nextID)
	f.records[rec.ID] = rec
	f.peak = max(f.peak, len(f.records))
	return rec, nil
}

func (f *fakeProvider) UpdateRecord(_ context.Context, rec provider.Record) error {
	f.updates++
	if f.failUpdate {
		return errors.New("update refused")
	}
	if _, ok := f.records[rec.ID]; !ok {
		return errors.New("no such record")
	}
	f.records[rec.ID] = rec
	return nil
}

func (f *fakeProvider) DeleteRecord(_ context.Context, _ string, id string) error {
	f.deletes++
	delete(f.records, id)
	return nil
}

// values returns the sorted record values currently stored.
func (f *fakeProvider) values() []string {
	var out []string
	for _, rec := range f.records {
		out = append(out, rec.Value)
	}
	slices.Sort(out)
	return out
}

func TestReconcileType(t *testing.T) {
	const (
		domain = "example.com"
		source = "src.example.com"
		ttl    = 600
	)
	marker := provider.BuildManagedRemark(provider.ManagedRemark{Source: source})
	managedRec := func(value string, recTTL uint64) provider.Record {
		return provider.Record{
			Domain: domain, SubDomain: "@", Type: provider.RecordTypeA,
			Value: value, TTL: recTTL, Remark: marker,
		}
	}

	tests := []struct {
		name       string
		existing   []provider.Record
		desired    []string
		failUpdate bool

		wantValues  []string
		wantCreates int
		wantUpdates int
		wantDeletes int
		wantPeak    int
	}{
		{
			name:        "value change repoints records in place",
			existing:    []provider.Record{managedRec("1.1.1.1", ttl), managedRec("2.2.2.2", ttl)},
			desired:     []string{"3.3.3.3", "4.4.4.4"},
			wantValues:  []string{"3.3.3.3", "4.4.4.4"},
			wantUpdates: 2,
			wantPeak:    2,
		},
		{
			name:        "growth creates only the surplus after updates",
			existing:    []provider.Record{managedRec("1.1.1.1", ttl)},
			desired:     []string{"3.3.3.3", "4.4.4.4"},
			wantValues:  []string{"3.3.3.3", "4.4.4.4"},
			wantUpdates: 1,
			wantCreates: 1,
			wantPeak:    2,
		},
		{
			name:        "shrink deletes leftover stale records",
			existing:    []provider.Record{managedRec("1.1.1.1", ttl), managedRec("2.2.2.2", ttl)},
			desired:     []string{"3.3.3.3"},
			wantValues:  []string{"3.3.3.3"},
			wantUpdates: 1,
			wantDeletes: 1,
			wantPeak:    2,
		},
		{
			name:       "matching state is a no-op",
			existing:   []provider.Record{managedRec("1.1.1.1", ttl)},
			desired:    []string{"1.1.1.1"},
			wantValues: []string{"1.1.1.1"},
			wantPeak:   1,
		},
		{
			name: "foreign records are never touched",
			existing: []provider.Record{
				managedRec("1.1.1.1", ttl),
				{Domain: domain, SubDomain: "@", Type: provider.RecordTypeA, Value: "9.9.9.9", TTL: ttl},
				{Domain: domain, SubDomain: "@", Type: provider.RecordTypeA, Value: "8.8.8.8", TTL: ttl,
					Remark: provider.BuildManagedRemark(provider.ManagedRemark{Instance: "other", Source: source})},
			},
			desired:     []string{"2.2.2.2"},
			wantValues:  []string{"2.2.2.2", "8.8.8.8", "9.9.9.9"},
			wantUpdates: 1,
			wantPeak:    3,
		},
		{
			name:        "failed update keeps the stale record serving",
			existing:    []provider.Record{managedRec("1.1.1.1", ttl)},
			desired:     []string{"2.2.2.2"},
			failUpdate:  true,
			wantValues:  []string{"1.1.1.1"},
			wantUpdates: 1,
			wantPeak:    1,
		},
		{
			name:        "kept value syncs ttl in place",
			existing:    []provider.Record{managedRec("1.1.1.1", 300)},
			desired:     []string{"1.1.1.1"},
			wantValues:  []string{"1.1.1.1"},
			wantUpdates: 1,
			wantPeak:    1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeProvider(tt.existing...)
			fake.failUpdate = tt.failUpdate
			w := &Worker{
				cfg: config.FlattenConfig{
					Domain: domain, SubDomain: "@", Source: source, TTL: ttl,
				},
				provider: fake,
				log:      zap.NewNop(),
			}

			if err := w.reconcileType(context.Background(), provider.RecordTypeA, tt.desired); err != nil {
				t.Fatalf("reconcileType() error = %v", err)
			}

			if got := fake.values(); !reflect.DeepEqual(got, tt.wantValues) {
				t.Errorf("record values = %v, want %v", got, tt.wantValues)
			}
			if fake.creates != tt.wantCreates || fake.updates != tt.wantUpdates || fake.deletes != tt.wantDeletes {
				t.Errorf("creates/updates/deletes = %d/%d/%d, want %d/%d/%d",
					fake.creates, fake.updates, fake.deletes,
					tt.wantCreates, tt.wantUpdates, tt.wantDeletes)
			}
			if fake.peak > tt.wantPeak {
				t.Errorf("peak concurrent records = %d, want <= %d", fake.peak, tt.wantPeak)
			}
		})
	}
}

func TestReconcileTypeAdoptsRecordsAfterSourceChange(t *testing.T) {
	const (
		domain    = "example.com"
		oldSource = "old.cdn.example.com"
		newSource = "new.cdn.example.com"
		ttl       = 600
	)

	tests := []struct {
		name     string
		instance string
		oldValue string
		newValue string
	}{
		{name: "named instance repoints old source record", instance: "beijing", oldValue: "1.1.1.1", newValue: "2.2.2.2"},
		{name: "default instance refreshes remark when value is unchanged", oldValue: "1.1.1.1", newValue: "1.1.1.1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldRecord := provider.Record{
				Domain: domain, SubDomain: "@", Type: provider.RecordTypeA,
				Value: tt.oldValue, TTL: ttl,
				Remark: provider.BuildManagedRemark(provider.ManagedRemark{Instance: tt.instance, Source: oldSource}),
			}
			fake := newFakeProvider(oldRecord)
			w := &Worker{
				cfg: config.FlattenConfig{
					Instance: tt.instance, Domain: domain, SubDomain: "@",
					Source: newSource, TTL: ttl,
				},
				provider: fake,
				log:      zap.NewNop(),
			}

			if err := w.reconcileType(context.Background(), provider.RecordTypeA, []string{tt.newValue}); err != nil {
				t.Fatalf("reconcileType() error = %v", err)
			}
			if fake.creates != 0 || fake.updates != 1 || fake.deletes != 0 {
				t.Fatalf("creates/updates/deletes = %d/%d/%d, want 0/1/0", fake.creates, fake.updates, fake.deletes)
			}
			if len(fake.records) != 1 {
				t.Fatalf("record count = %d, want 1", len(fake.records))
			}
			wantRemark := provider.BuildManagedRemark(provider.ManagedRemark{Instance: tt.instance, Source: newSource})
			for _, rec := range fake.records {
				if rec.Value != tt.newValue || rec.Remark != wantRemark {
					t.Errorf("record = value %q, remark %q; want value %q, remark %q", rec.Value, rec.Remark, tt.newValue, wantRemark)
				}
			}
		})
	}
}
