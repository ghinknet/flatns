// Package resolver follows a CNAME chain to its terminal address records.
// It can use a user-supplied set of recursive DNS servers (for reliability and
// freedom from a flaky local resolver) or fall back to the operating system's
// resolver when none are configured.
package resolver

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// maxCNAMEDepth bounds CNAME chain following to defend against loops or
// pathologically long chains.
const maxCNAMEDepth = 16

// Result holds the terminal addresses discovered for a name.
type Result struct {
	// IPv4 is the sorted, de-duplicated set of A record values.
	IPv4 []string
	// IPv6 is the sorted, de-duplicated set of AAAA record values.
	IPv6 []string
}

// Resolver resolves names using either custom recursive servers or the system
// resolver. A zero-value Resolver (no servers) uses the system resolver.
type Resolver struct {
	// servers is the list of recursive resolvers as host:port. When empty the
	// system resolver is used.
	servers []string
	// timeout bounds each individual DNS exchange.
	timeout time.Duration
	client  *dns.Client
}

// New builds a Resolver. servers may be host or host:port; a missing port is
// defaulted to 53. An empty servers slice selects the system resolver.
func New(servers []string, timeout time.Duration) *Resolver {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	normalised := make([]string, 0, len(servers))
	for _, s := range servers {
		normalised = append(normalised, ensurePort(s))
	}
	return &Resolver{
		servers: normalised,
		timeout: timeout,
		client:  &dns.Client{Timeout: timeout},
	}
}

// ensurePort appends the default DNS port when none is present, handling bare
// IPv6 literals correctly.
func ensurePort(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	// IPv6 literals must be bracketed before adding a port.
	if strings.Contains(server, ":") {
		return fmt.Sprintf("[%s]:53", server)
	}
	return net.JoinHostPort(server, "53")
}

// Resolve returns the terminal A/AAAA records reachable from name. When the
// Resolver has custom servers it follows the CNAME chain explicitly; otherwise
// it delegates to the system resolver which performs recursion for us.
func (r *Resolver) Resolve(ctx context.Context, name string, wantIPv6 bool) (Result, error) {
	if len(r.servers) == 0 {
		return r.resolveSystem(ctx, name, wantIPv6)
	}
	return r.resolveCustom(ctx, name, wantIPv6)
}

// resolveSystem uses Go's built-in resolver, which already follows CNAMEs.
func (r *Resolver) resolveSystem(ctx context.Context, name string, wantIPv6 bool) (Result, error) {
	var resolver net.Resolver
	network := "ip4"
	if wantIPv6 {
		network = "ip"
	}
	ips, err := resolver.LookupIP(ctx, network, name)
	if err != nil {
		return Result{}, fmt.Errorf("resolver: system lookup %q: %w", name, err)
	}
	var res Result
	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil {
			res.IPv4 = append(res.IPv4, v4.String())
		} else if wantIPv6 {
			res.IPv6 = append(res.IPv6, ip.String())
		}
	}
	res.normalise()
	if len(res.IPv4) == 0 && len(res.IPv6) == 0 {
		return res, fmt.Errorf("resolver: %q produced no address records", name)
	}
	return res, nil
}

// resolveCustom performs explicit CNAME chain following against the configured
// recursive servers and gathers the resulting address records.
func (r *Resolver) resolveCustom(ctx context.Context, name string, wantIPv6 bool) (Result, error) {
	var res Result

	v4, err := r.queryWithChain(ctx, name, dns.TypeA)
	if err != nil {
		return Result{}, err
	}
	res.IPv4 = v4

	if wantIPv6 {
		v6, err := r.queryWithChain(ctx, name, dns.TypeAAAA)
		if err != nil {
			return Result{}, err
		}
		res.IPv6 = v6
	}

	res.normalise()
	if len(res.IPv4) == 0 && len(res.IPv6) == 0 {
		return res, fmt.Errorf("resolver: %q produced no address records", name)
	}
	return res, nil
}

// queryWithChain queries for qtype, following CNAME records returned in the
// answer section and re-querying as needed until address records are found or
// the depth limit is reached. Most recursive resolvers already inline the full
// chain in a single answer, so the explicit loop is a safety net.
func (r *Resolver) queryWithChain(ctx context.Context, name string, qtype uint16) ([]string, error) {
	current := dns.Fqdn(name)
	for depth := 0; depth < maxCNAMEDepth; depth++ {
		msg, err := r.exchange(ctx, current, qtype)
		if err != nil {
			return nil, err
		}

		var addrs []string
		var nextCNAME string
		for _, rr := range msg.Answer {
			switch v := rr.(type) {
			case *dns.A:
				if qtype == dns.TypeA {
					addrs = append(addrs, v.A.String())
				}
			case *dns.AAAA:
				if qtype == dns.TypeAAAA {
					addrs = append(addrs, v.AAAA.String())
				}
			case *dns.CNAME:
				nextCNAME = v.Target
			}
		}

		if len(addrs) > 0 {
			return addrs, nil
		}
		if nextCNAME == "" || nextCNAME == current {
			// No address records and nothing further to follow.
			return nil, nil
		}
		current = nextCNAME
	}
	return nil, fmt.Errorf("resolver: CNAME chain for %q exceeded depth %d", name, maxCNAMEDepth)
}

// exchange sends a single query, trying each configured server in turn until
// one succeeds. This gives basic redundancy across multiple resolvers.
func (r *Resolver) exchange(ctx context.Context, name string, qtype uint16) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(name, qtype)
	m.RecursionDesired = true

	var lastErr error
	for _, server := range r.servers {
		resp, _, err := r.client.ExchangeContext(ctx, m, server)
		if err != nil {
			lastErr = err
			continue
		}
		if resp.Rcode != dns.RcodeSuccess {
			lastErr = fmt.Errorf("resolver: %s returned rcode %s for %q", server, dns.RcodeToString[resp.Rcode], name)
			continue
		}
		return resp, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("resolver: no servers configured")
	}
	return nil, lastErr
}

// normalise sorts and de-duplicates both address sets so callers can compare
// results across polls without worrying about ordering noise.
func (res *Result) normalise() {
	res.IPv4 = dedupSort(res.IPv4)
	res.IPv6 = dedupSort(res.IPv6)
}

func dedupSort(in []string) []string {
	if len(in) == 0 {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	out := in[:0]
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
