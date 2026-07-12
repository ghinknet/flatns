package flattener

import (
	"reflect"
	"testing"

	"flatns/internal/infra/config"
	"flatns/internal/provider"
	"flatns/internal/resolver"

	"go.uber.org/zap"
)

// newTestWorker builds a Worker with the given limits and a no-op logger, so
// buildDesired can be exercised without a live provider or global logger.
func newTestWorker(ipv6 bool, maxRecords, maxTotal int) *Worker {
	return &Worker{
		cfg: config.FlattenConfig{
			IPv6:            ipv6,
			MaxRecords:      maxRecords,
			MaxRecordsTotal: maxTotal,
		},
		log: zap.NewNop(),
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
