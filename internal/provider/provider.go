// Package provider defines the record-operation abstraction layer used by
// flatns. Every DNS hosting service (Tencent Cloud, Aliyun, ...) is wrapped
// behind the Provider interface so that the flattening logic stays completely
// decoupled from any specific vendor SDK. Adding support for a new vendor only
// requires implementing this interface and registering a factory.
package provider

import (
	"context"
	"fmt"
	"strings"
)

// RecordType enumerates the DNS record types flatns needs to manage. We only
// care about address records (the result of flattening a CNAME chain).
type RecordType string

const (
	// RecordTypeA is an IPv4 address record.
	RecordTypeA RecordType = "A"
	// RecordTypeAAAA is an IPv6 address record.
	RecordTypeAAAA RecordType = "AAAA"
)

// ManagedMarker is the prefix written into a record's remark/description field
// so that flatns can reliably recognise records it created itself. It is never
// applied to records created by the user, which guarantees we never modify or
// delete records we do not own.
const ManagedMarker = "flatns-managed"

// ManagedRemark identifies a record flatns maintains. Instance distinguishes
// independent flatns deployments (e.g. per-region single-host instances that
// each manage a different subset of IPs), so they never claim or clobber each
// other's records even when flattening the same source. Source is the CNAME
// being flattened. A record is "ours" only when both fields match this entry.
type ManagedRemark struct {
	// Instance is the deployment identifier. Empty means the unnamed/default
	// instance, which keeps the legacy single-instance remark format.
	Instance string
	// Source is the CNAME being flattened.
	Source string
}

// BuildManagedRemark builds the remark/description string stored on every
// auto-maintained record.
//
// Format:
//
//	flatns-managed:<source>             (instance empty; legacy-compatible)
//	flatns-managed:i=<instance>:<source> (named instance)
//
// The "i=" prefix on the instance segment makes named-instance remarks
// unambiguous to parse and impossible to confuse with a legacy remark whose
// source happened to look like an instance.
func BuildManagedRemark(m ManagedRemark) string {
	if m.Instance == "" {
		return fmt.Sprintf("%s:%s", ManagedMarker, m.Source)
	}
	return fmt.Sprintf("%s:i=%s:%s", ManagedMarker, m.Instance, m.Source)
}

// ParseManagedRemark reports whether a remark was produced by flatns and, if so,
// returns the embedded instance and source. Records whose remark does not carry
// the marker are considered user-owned and must never be touched.
func ParseManagedRemark(remark string) (ManagedRemark, bool) {
	const prefix = ManagedMarker + ":"
	if !strings.HasPrefix(remark, prefix) {
		return ManagedRemark{}, false
	}
	rest := strings.TrimPrefix(remark, prefix)

	// Named-instance form: "i=<instance>:<source>".
	if strings.HasPrefix(rest, "i=") {
		body := strings.TrimPrefix(rest, "i=")
		// Split on the first colon: instance, then source (a hostname, which
		// never contains a colon).
		idx := strings.IndexByte(body, ':')
		if idx < 0 {
			// Malformed; treat as not ours to be safe.
			return ManagedRemark{}, false
		}
		return ManagedRemark{Instance: body[:idx], Source: body[idx+1:]}, true
	}

	// Legacy form: "<source>" with the default (empty) instance.
	return ManagedRemark{Source: rest}, true
}

// Record is the vendor-neutral representation of a single DNS resource record.
// Each provider implementation is responsible for translating between this
// struct and its own SDK types.
type Record struct {
	// ID is the provider-assigned unique identifier of the record. It is empty
	// for records that have not yet been created.
	ID string
	// Domain is the registrable zone, e.g. "example.cn".
	Domain string
	// SubDomain is the host part relative to Domain, e.g. "@" for the apex or
	// "www" for a subdomain.
	SubDomain string
	// Type is the record type (A or AAAA).
	Type RecordType
	// Value is the record payload, i.e. the IP address.
	Value string
	// TTL is the record time-to-live in seconds.
	TTL uint64
	// Remark holds the provider remark/description. flatns stores its managed
	// marker here so records can be reconciled across runs.
	Remark string
}

// Provider abstracts the CRUD operations flatns performs against a DNS hosting
// service. Implementations must be safe for concurrent use because one worker
// goroutine runs per flatten entry and several entries may share a provider.
type Provider interface {
	// Name returns the registered provider identifier (e.g. "tencent").
	Name() string

	// ListRecords returns every record for the given domain/subdomain pair that
	// matches one of the requested types. Passing an empty types slice means
	// "all supported types". Implementations should return records regardless of
	// whether they are flatns-managed; filtering by remark happens upstream.
	ListRecords(ctx context.Context, domain, subDomain string, types []RecordType) ([]Record, error)

	// CreateRecord creates a new record and returns it populated with the
	// provider-assigned ID.
	CreateRecord(ctx context.Context, rec Record) (Record, error)

	// UpdateRecord updates an existing record identified by rec.ID.
	UpdateRecord(ctx context.Context, rec Record) error

	// DeleteRecord removes the record identified by id within the given domain.
	DeleteRecord(ctx context.Context, domain, id string) error
}

// Config carries the credentials and tuning knobs needed to construct a
// provider. Vendor-specific fields are intentionally kept generic; each
// implementation reads only the keys it understands from Extra.
type Config struct {
	// Name selects which registered factory to use (e.g. "tencent", "aliyun").
	Name string
	// SecretID / AccessKey identifies the API caller. The semantics map onto the
	// vendor's own naming (SecretId for Tencent, AccessKeyId for Aliyun).
	SecretID string
	// SecretKey is the API secret.
	SecretKey string
	// Region is an optional region hint used by some SDKs.
	Region string
	// Extra holds provider-specific options that do not fit the common fields.
	Extra map[string]string
}
