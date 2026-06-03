// Package tencent implements the provider.Provider interface for Tencent Cloud
// DNSPod using the official tencentcloud-sdk-go (dnspod v20210323 API).
package tencent

import (
	"context"
	"fmt"
	"strconv"

	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/errors"
	"github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/common/profile"
	dnspod "github.com/tencentcloud/tencentcloud-sdk-go/tencentcloud/dnspod/v20210323"

	"flatns/internal/provider"
)

// providerName is the registry key for this implementation.
const providerName = "tencent"

// defaultRecordLine is DNSPod's "default" resolution line. Tencent expects the
// localised string; the SDK accepts the Chinese default line name.
const defaultRecordLine = "默认"

// init registers the Tencent factory so importing this package wires it into
// the provider registry.
func init() {
	provider.Register(providerName, newFromConfig)
}

// tencentProvider is a thin adapter around the DNSPod client.
type tencentProvider struct {
	client *dnspod.Client
}

// newFromConfig builds a tencentProvider from generic provider.Config.
func newFromConfig(cfg provider.Config) (provider.Provider, error) {
	if cfg.SecretID == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("tencent: secret_id and secret_key are required")
	}
	credential := common.NewCredential(cfg.SecretID, cfg.SecretKey)
	cpf := profile.NewClientProfile()
	// Region is optional for DNSPod (a global service) but harmless to pass.
	client, err := dnspod.NewClient(credential, cfg.Region, cpf)
	if err != nil {
		return nil, fmt.Errorf("tencent: create client: %w", err)
	}
	return &tencentProvider{client: client}, nil
}

// Name implements provider.Provider.
func (p *tencentProvider) Name() string { return providerName }

// ListRecords implements provider.Provider. It pages through DescribeRecordList
// filtered by subdomain and collects records of the requested types.
func (p *tencentProvider) ListRecords(ctx context.Context, domain, subDomain string, types []provider.RecordType) ([]provider.Record, error) {
	wanted := typeSet(types)

	var out []provider.Record
	var offset uint64
	const limit uint64 = 100

	for {
		req := dnspod.NewDescribeRecordListRequest()
		req.Domain = new(domain)
		req.Subdomain = new(subDomain)
		req.Offset = new(offset)
		req.Limit = new(limit)

		resp, err := p.client.DescribeRecordListWithContext(ctx, req)
		if err != nil {
			// An empty subdomain legitimately returns a "no records" error code;
			// treat that as an empty result rather than a hard failure.
			if isNoRecordsErr(err) {
				return out, nil
			}
			return nil, fmt.Errorf("tencent: describe records: %w", err)
		}

		for _, item := range resp.Response.RecordList {
			rt := provider.RecordType(deref(item.Type))
			if _, ok := wanted[rt]; !ok {
				continue
			}
			out = append(out, provider.Record{
				ID:        strconv.FormatUint(derefU64(item.RecordId), 10),
				Domain:    domain,
				SubDomain: deref(item.Name),
				Type:      rt,
				Value:     deref(item.Value),
				TTL:       derefU64(item.TTL),
				Remark:    deref(item.Remark),
			})
		}

		got := uint64(len(resp.Response.RecordList))
		if got < limit {
			break
		}
		offset += got
	}
	return out, nil
}

// CreateRecord implements provider.Provider.
func (p *tencentProvider) CreateRecord(ctx context.Context, rec provider.Record) (provider.Record, error) {
	req := dnspod.NewCreateRecordRequest()
	req.Domain = new(rec.Domain)
	req.SubDomain = new(rec.SubDomain)
	req.RecordType = new(string(rec.Type))
	req.RecordLine = new(defaultRecordLine)
	req.Value = new(rec.Value)
	req.TTL = new(rec.TTL)
	if rec.Remark != "" {
		req.Remark = new(rec.Remark)
	}

	resp, err := p.client.CreateRecordWithContext(ctx, req)
	if err != nil {
		return provider.Record{}, fmt.Errorf("tencent: create record: %w", err)
	}
	rec.ID = strconv.FormatUint(derefU64(resp.Response.RecordId), 10)
	return rec, nil
}

// UpdateRecord implements provider.Provider.
func (p *tencentProvider) UpdateRecord(ctx context.Context, rec provider.Record) error {
	id, err := strconv.ParseUint(rec.ID, 10, 64)
	if err != nil {
		return fmt.Errorf("tencent: invalid record id %q: %w", rec.ID, err)
	}
	req := dnspod.NewModifyRecordRequest()
	req.Domain = new(rec.Domain)
	req.RecordId = new(id)
	req.SubDomain = new(rec.SubDomain)
	req.RecordType = new(string(rec.Type))
	req.RecordLine = new(defaultRecordLine)
	req.Value = new(rec.Value)
	req.TTL = new(rec.TTL)
	if rec.Remark != "" {
		req.Remark = new(rec.Remark)
	}

	if _, err := p.client.ModifyRecordWithContext(ctx, req); err != nil {
		return fmt.Errorf("tencent: modify record: %w", err)
	}
	return nil
}

// DeleteRecord implements provider.Provider.
func (p *tencentProvider) DeleteRecord(ctx context.Context, domain, id string) error {
	rid, err := strconv.ParseUint(id, 10, 64)
	if err != nil {
		return fmt.Errorf("tencent: invalid record id %q: %w", id, err)
	}
	req := dnspod.NewDeleteRecordRequest()
	req.Domain = new(domain)
	req.RecordId = new(rid)
	if _, err := p.client.DeleteRecordWithContext(ctx, req); err != nil {
		return fmt.Errorf("tencent: delete record: %w", err)
	}
	return nil
}

// typeSet builds a lookup set from the requested types, defaulting to A+AAAA
// when none are given.
func typeSet(types []provider.RecordType) map[provider.RecordType]struct{} {
	if len(types) == 0 {
		types = []provider.RecordType{provider.RecordTypeA, provider.RecordTypeAAAA}
	}
	set := make(map[provider.RecordType]struct{}, len(types))
	for _, t := range types {
		set[t] = struct{}{}
	}
	return set
}

// isNoRecordsErr reports whether the DNSPod error indicates that the queried
// subdomain simply has no records yet.
func isNoRecordsErr(err error) bool {
	var sdkErr *errors.TencentCloudSDKError
	if ok := asSDKError(err, &sdkErr); !ok {
		return false
	}
	// DNSPod returns this code when the record list is empty for the query.
	return sdkErr.Code == "ResourceNotFound.NoDataOfRecord"
}

// asSDKError unwraps err into a *errors.TencentCloudSDKError if possible.
func asSDKError(err error, target **errors.TencentCloudSDKError) bool {
	if e, ok := err.(*errors.TencentCloudSDKError); ok {
		*target = e
		return true
	}
	return false
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefU64(v *uint64) uint64 {
	if v == nil {
		return 0
	}
	return *v
}
