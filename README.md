# flatns

Flatten your CNAME / NS records.

Many DNS providers (Tencent Cloud, Aliyun, ...) gate native CNAME flattening
behind paid plan upgrades. Without flattening, a `CNAME` on the apex (`@`)
conflicts with coexisting records such as `MX`, making it unusable. flatns
solves this the simple way: it resolves the CNAME chain to its terminal A/AAAA
records and keeps your target subdomain in sync automatically.

## How it works

1. Resolve the configured `source` CNAME chain to all reachable A (and
   optionally AAAA) records, using custom recursive DNS servers or the system
   resolver.
2. Compare the resolved addresses against the records flatns manages on the
   target `domain`/`sub_domain`.
3. Create / update / delete records so the target matches, then repeat on a
   timer.

Every record flatns creates is tagged with a remark of the form
`flatns-managed:<source>` (or `flatns-managed:i=<instance>:<source>` when an
`instance` is set). flatns **only ever touches records carrying its own
marker**, so your manually-created records are never modified or deleted.

### Multi-region / multi-instance

When a flattened CNAME returns geo-specific results, a single host can only
serve a subset of the IPs. Set a unique `instance` per deployment (global or
per-entry) so several flatns instances can flatten the **same** source while
each manages only its own records — they never claim or clobber each other.
Leave `instance` empty for a single deployment.

## Architecture

```
main.go                       Entrypoint: tiny Init/Cleanup wiring + signal loop
internal/infra/config         viper YAML singleton; SIGHUP hot reload; OnReload hooks
internal/infra/logger         zap logger: console + JSON files with lumberjack rotation
internal/resolver             CNAME chain resolution; custom NS or system resolver
internal/provider             Vendor-neutral Provider interface + factory registry
internal/provider/tencent     Tencent Cloud DNSPod implementation
internal/provider/aliyun      Alibaba Cloud AliDNS implementation (V2.0 SDK)
internal/flattener            Per-entry reconcile loop + Supervisor (lifecycle/reload)
```

The `provider.Provider` interface is the extension point. To add a new vendor:

1. Implement `ListRecords / CreateRecord / UpdateRecord / DeleteRecord`.
2. Call `provider.Register("<name>", factory)` from the package `init()`.
3. Add a blank import of the package in `main.go`.

No other code needs to change.

## Build

Requires Go 1.26+.

```sh
go build -o flatns .
```

## Configure

Copy `config.example.yaml` to `config.yaml` and edit it. Highlights:

- `log`: zap + lumberjack. `debug` toggles the level; `file.all`/`file.err` set
  the JSON log files (empty disables a sink; stdout is always written);
  `max_size`/`max_backups`/`max_age`/`compress` control rotation.
- `instance`: deployment identifier for multi-instance setups (see above).
  Empty by default. Per-entry `instance` overrides the global value.
- `resolvers`: global recursive DNS servers (`host` or `host:port`, port
  defaults to 53). Empty = system resolver. Per-entry `resolvers` override this.
- `providers`: credentials keyed by an alias you choose; reused across entries.
- `flattens`: a list of jobs, each reconciled in its own goroutine. Fields:
  `source`, `domain`, `sub_domain` (`@` = apex), `ttl`, `interval` (e.g. `30s`,
  `5m`), `ipv4` and `ipv6` (independent address-family switches), optional
  `max_records`/`max_records_total` (see below), and an
  optional `instance`.

`ipv4` controls A records and `ipv6` controls AAAA records. They default to
`true` and `false`, respectively, so older configurations keep their previous
behavior. At least one family must be enabled.

### Limiting record count

Some providers' plans cap how many values a single sub-domain may resolve to —
DNSPod's free tier, for example, allows only a couple of records per host.
Flattening a source with more addresses than that quota would make the provider
reject the surplus `CreateRecord` calls. Two optional per-entry knobs bound the
managed record set so this never happens:

- `max_records`: per-type cap. At most this many A records **and** this many
  AAAA records are kept ("V4/V6 each N"). `0` (default) means unlimited.
- `max_records_total`: combined cap across A and AAAA ("N in total"), for
  providers that count both types against one quota. The budget is split evenly
  between the enabled types, and either type's unused share flows to the other
  (IPv4 gets the odd slot). `0` (default) means unlimited.

Both may be set at once; each is enforced independently. Surplus addresses are
dropped deterministically (the resolver's sorted order is stable, so the cap
never causes create/delete churn) and the dropped values are logged at warn
level.

## Run

flatns reads `config.yaml` from the working directory (no flags).

```sh
./flatns
```

- Send `SIGHUP` (`kill -HUP <pid>`) to hot-reload the config. Workers are
  gracefully torn down and respawned with the new config; an invalid edit is
  logged and ignored, leaving the previous config running.
- `SIGINT` / `SIGTERM` shut down cleanly.

## Supported providers

| Provider      | `type`    | Credentials                                               |
|---------------|-----------|-----------------------------------------------------------|
| Tencent Cloud | `tencent` | `secret_id` = SecretId, `secret_key` = SecretKey          |
| Alibaba Cloud | `aliyun`  | `secret_id` = AccessKeyId, `secret_key` = AccessKeySecret |
