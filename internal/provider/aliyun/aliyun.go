// Package aliyun implements the provider.Provider interface for Alibaba Cloud
// DNS (AliDNS) using the V2.0 SDK (alibabacloud-go/alidns-20150109/v4).
package aliyun

import (
	"context"
	"fmt"

	alidns "github.com/alibabacloud-go/alidns-20150109/v4/client"
	openapi "github.com/alibabacloud-go/darabonba-openapi/v2/utils"
	"github.com/alibabacloud-go/tea/dara"

	"flatns/internal/provider"
)

// providerName is the registry key for this implementation.
const providerName = "aliyun"

// defaultEndpoint is AliDNS's global endpoint. AliDNS is a global service, so a
// single endpoint serves every region.
const defaultEndpoint = "alidns.cn-hangzhou.aliyuncs.com"

// init registers the Aliyun factory.
func init() {
	provider.Register(providerName, newFromConfig)
}

// aliyunProvider adapts the AliDNS V2.0 client to provider.Provider.
type aliyunProvider struct {
	client *alidns.Client
}

// newFromConfig builds an aliyunProvider from generic provider.Config. The
// endpoint can be overridden via Extra["endpoint"] for private/edge cases.
func newFromConfig(cfg provider.Config) (provider.Provider, error) {
	if cfg.SecretID == "" || cfg.SecretKey == "" {
		return nil, fmt.Errorf("aliyun: secret_id (AccessKeyId) and secret_key (AccessKeySecret) are required")
	}
	endpoint := defaultEndpoint
	if ep, ok := cfg.Extra["endpoint"]; ok && ep != "" {
		endpoint = ep
	}

	openapiCfg := &openapi.Config{
		AccessKeyId:     dara.String(cfg.SecretID),
		AccessKeySecret: dara.String(cfg.SecretKey),
		Endpoint:        dara.String(endpoint),
	}
	if cfg.Region != "" {
		openapiCfg.RegionId = dara.String(cfg.Region)
	}

	client, err := alidns.NewClient(openapiCfg)
	if err != nil {
		return nil, fmt.Errorf("aliyun: create client: %w", err)
	}
	return &aliyunProvider{client: client}, nil
}

// Name implements provider.Provider.
func (p *aliyunProvider) Name() string { return providerName }

// ListRecords implements provider.Provider. AliDNS pages its record listing, so
// we iterate until every page has been consumed. The V2.0 SDK is not
// context-aware, so we honour ctx with a cancellation check before each call.
func (p *aliyunProvider) ListRecords(ctx context.Context, domain, subDomain string, types []provider.RecordType) ([]provider.Record, error) {
	wanted := typeSet(types)

	var out []provider.Record
	var pageNumber int64 = 1
	const pageSize int64 = 100

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req := &alidns.DescribeDomainRecordsRequest{
			DomainName: dara.String(domain),
			RRKeyWord:  dara.String(subDomain),
			PageNumber: dara.Int64(pageNumber),
			PageSize:   dara.Int64(pageSize),
		}

		resp, err := p.client.DescribeDomainRecordsWithOptions(req, runtimeOptions())
		if err != nil {
			return nil, fmt.Errorf("aliyun: describe records: %w", err)
		}
		if resp.Body == nil || resp.Body.DomainRecords == nil {
			break
		}

		for _, item := range resp.Body.DomainRecords.Record {
			rt := provider.RecordType(dara.StringValue(item.Type))
			if _, ok := wanted[rt]; !ok {
				continue
			}
			// RRKeyWord is a fuzzy match; enforce an exact RR comparison so we do
			// not accidentally manage records of a different subdomain.
			if dara.StringValue(item.RR) != subDomain {
				continue
			}
			out = append(out, provider.Record{
				ID:        dara.StringValue(item.RecordId),
				Domain:    domain,
				SubDomain: dara.StringValue(item.RR),
				Type:      rt,
				Value:     dara.StringValue(item.Value),
				TTL:       uint64(dara.Int64Value(item.TTL)),
				Remark:    dara.StringValue(item.Remark),
			})
		}

		total := dara.Int64Value(resp.Body.TotalCount)
		if pageNumber*pageSize >= total {
			break
		}
		pageNumber++
	}
	return out, nil
}

// CreateRecord implements provider.Provider. AliDNS sets the remark with a
// separate call, so we create the record first and then attach the marker.
func (p *aliyunProvider) CreateRecord(ctx context.Context, rec provider.Record) (provider.Record, error) {
	if err := ctx.Err(); err != nil {
		return provider.Record{}, err
	}
	req := &alidns.AddDomainRecordRequest{
		DomainName: dara.String(rec.Domain),
		RR:         dara.String(rec.SubDomain),
		Type:       dara.String(string(rec.Type)),
		Value:      dara.String(rec.Value),
		TTL:        dara.Int64(int64(rec.TTL)),
	}

	resp, err := p.client.AddDomainRecordWithOptions(req, runtimeOptions())
	if err != nil {
		return provider.Record{}, fmt.Errorf("aliyun: add record: %w", err)
	}
	rec.ID = dara.StringValue(resp.Body.RecordId)

	if rec.Remark != "" {
		if err := p.setRemark(rec.ID, rec.Remark); err != nil {
			return rec, err
		}
	}
	return rec, nil
}

// UpdateRecord implements provider.Provider. It updates the address value and
// refreshes the remark so the managed marker survives.
func (p *aliyunProvider) UpdateRecord(ctx context.Context, rec provider.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := &alidns.UpdateDomainRecordRequest{
		RecordId: dara.String(rec.ID),
		RR:       dara.String(rec.SubDomain),
		Type:     dara.String(string(rec.Type)),
		Value:    dara.String(rec.Value),
		TTL:      dara.Int64(int64(rec.TTL)),
	}

	if _, err := p.client.UpdateDomainRecordWithOptions(req, runtimeOptions()); err != nil {
		return fmt.Errorf("aliyun: update record: %w", err)
	}
	if rec.Remark != "" {
		if err := p.setRemark(rec.ID, rec.Remark); err != nil {
			return err
		}
	}
	return nil
}

// DeleteRecord implements provider.Provider.
func (p *aliyunProvider) DeleteRecord(ctx context.Context, _ string, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	req := &alidns.DeleteDomainRecordRequest{
		RecordId: dara.String(id),
	}
	if _, err := p.client.DeleteDomainRecordWithOptions(req, runtimeOptions()); err != nil {
		return fmt.Errorf("aliyun: delete record: %w", err)
	}
	return nil
}

// setRemark applies a remark to an existing record via the dedicated AliDNS API.
func (p *aliyunProvider) setRemark(recordID, remark string) error {
	req := &alidns.UpdateDomainRecordRemarkRequest{
		RecordId: dara.String(recordID),
		Remark:   dara.String(remark),
	}
	if _, err := p.client.UpdateDomainRecordRemarkWithOptions(req, runtimeOptions()); err != nil {
		return fmt.Errorf("aliyun: set remark: %w", err)
	}
	return nil
}

// runtimeOptions returns per-call runtime options with sensible timeouts so a
// hung request cannot stall a worker indefinitely.
func runtimeOptions() *dara.RuntimeOptions {
	return &dara.RuntimeOptions{
		ConnectTimeout: dara.Int(5000),
		ReadTimeout:    dara.Int(10000),
	}
}

// typeSet builds a lookup set, defaulting to A+AAAA when empty.
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
