// Package orchestrator owns the high-level Cloudflare logic the operator
// used to drive in-process: list managed records (every record whose
// `comment` is non-empty), apply external-dns Changes (create/update/delete
// DNS records), and honour the ownership-label round-trip without the
// sidecar inventing its own owner identity.
//
// The HTTP handler layer translates external-dns webhook v1 requests into
// calls on this package; the internal/cloudflare client layer owns the
// Cloudflare REST API details (typed SDK params, per-record ids, etc.).
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"

	cfclient "github.com/mrkhachaturov/ddo-cloudflare/internal/cloudflare"
)

// CloudflareProxiedKey is the providerSpecific property name used to
// round-trip the Cloudflare "orange-cloud" proxy toggle on the wire.
// Matches upstream external-dns' Cloudflare provider convention so the
// wire format stays interoperable with anyone else reading it.
const CloudflareProxiedKey = "external-dns.alpha.kubernetes.io/cloudflare-proxied"

// Options configures an Orchestrator. All fields are required (DefaultTTL
// may be zero — the cloudflare client converts <=0 to 1 ("automatic")).
//
// Note: there is no ownership-label field here. The sidecar is
// intentionally ownership-agnostic — it round-trips whatever value the
// caller stamps on Labels["owner"] of each Endpoint through the persistent
// Cloudflare `comment` field, and surfaces it back on ListRecords.
type Options struct {
	// Zones is the optional zone allow-list. Empty = all token-visible
	// zones (resolved on first use from the Cloudflare API).
	Zones []string

	// DefaultTTL is applied when an inbound Endpoint omits recordTTL.
	// Cloudflare interprets `1` as "automatic".
	DefaultTTL int

	// ProxiedDefault is the Proxied value applied to A/CNAME records when
	// the inbound Endpoint has no cloudflare-proxied providerSpecific
	// property.
	ProxiedDefault bool
}

// ProviderSpecificProperty mirrors the wire shape — we use it to carry
// the cloudflare-proxied toggle in and out without depending on the api
// package (which depends on us).
type ProviderSpecificProperty struct {
	Name  string
	Value string
}

// Endpoint is the wire-compatible external-dns endpoint shape. We carry
// the fields needed to faithfully round-trip A/AAAA/CNAME/MX/NS records
// plus the cloudflare-proxied providerSpecific entry.
type Endpoint struct {
	DNSName          string
	Targets          []string
	RecordType       string
	RecordTTL        int64
	Labels           map[string]string
	ProviderSpecific []ProviderSpecificProperty
}

// Changes is the inbound change-set from external-dns POST /records.
type Changes struct {
	Create    []*Endpoint
	UpdateOld []*Endpoint
	UpdateNew []*Endpoint
	Delete    []*Endpoint
}

// Orchestrator is the runtime object. Construct once at boot.
type Orchestrator struct {
	opts   Options
	client cfclient.Client

	// applyMu serialises ApplyChanges across concurrent webhook callers.
	// The cloudflare-go HTTP layer is happy with concurrency, but the
	// orchestrator's read-snapshot-then-write pattern needs serialisation
	// to be sound.
	applyMu sync.Mutex

	// zonesMu guards the lazy zone cache. Zones change rarely; we resolve
	// once at first use and re-resolve only on explicit error.
	zonesMu      sync.Mutex
	resolvedZ    []cfclient.Zone
	zoneFilter   []string // configured allow-list (lowercased, dot-stripped)
	filteredOnce bool
}

// New constructs an Orchestrator over the given Cloudflare client.
func New(opts Options, client cfclient.Client) *Orchestrator {
	o := &Orchestrator{opts: opts, client: client}
	for _, z := range opts.Zones {
		o.zoneFilter = append(o.zoneFilter, normalizeName(z))
	}
	return o
}

// Zones returns the set of zone names this sidecar manages, for the
// GET / domain-filter response. When CLOUDFLARE_ZONES is set, the result
// is exactly that list (verbatim). When unset, every token-visible zone
// name is returned, resolved from the Cloudflare API on first call.
func (o *Orchestrator) Zones(ctx context.Context) ([]string, error) {
	zones, err := o.resolveZones(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(zones))
	for _, z := range zones {
		out = append(out, z.Name)
	}
	return out, nil
}

// resolveZones returns the effective zone list — either the configured
// allow-list intersected with what the token can see (so we surface IDs
// for API calls), or every token-visible zone when no allow-list is set.
func (o *Orchestrator) resolveZones(ctx context.Context) ([]cfclient.Zone, error) {
	o.zonesMu.Lock()
	defer o.zonesMu.Unlock()
	if o.resolvedZ != nil {
		return o.resolvedZ, nil
	}
	all, err := o.client.ListZones(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve zones: %w", err)
	}
	if len(o.zoneFilter) == 0 {
		o.resolvedZ = all
		return all, nil
	}
	allow := map[string]bool{}
	for _, z := range o.zoneFilter {
		allow[z] = true
	}
	out := make([]cfclient.Zone, 0, len(all))
	for _, z := range all {
		if allow[normalizeName(z.Name)] {
			out = append(out, z)
		}
	}
	o.resolvedZ = out
	return out, nil
}

// ListRecords paginates every managed zone, drops the rows that aren't
// operator-managed (empty `comment`), groups the rest by (name, type),
// and emits one Endpoint per group. Labels["owner"] is populated verbatim
// from the row's comment; the cloudflare-proxied providerSpecific is
// emitted for every A/CNAME row.
func (o *Orchestrator) ListRecords(ctx context.Context) ([]*Endpoint, error) {
	zones, err := o.resolveZones(ctx)
	if err != nil {
		return nil, err
	}

	type key struct{ name, rtype string }
	bucket := map[key]*Endpoint{}
	var order []key
	// Track the canonical Proxied value per group — the operator-side
	// drift detection compares the providerSpecific value, so we must
	// surface what's actually on Cloudflare.
	proxied := map[key]bool{}

	for _, z := range zones {
		rows, err := o.client.ListRecords(ctx, z.ID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if r.Comment == "" {
				continue
			}
			k := key{normalizeName(r.Name), strings.ToUpper(r.Type)}
			ep, ok := bucket[k]
			if !ok {
				ep = &Endpoint{
					DNSName:    k.name,
					RecordType: k.rtype,
					RecordTTL:  int64(r.TTL),
					Labels:     map[string]string{"owner": r.Comment},
				}
				bucket[k] = ep
				order = append(order, k)
				if k.rtype == "A" || k.rtype == "CNAME" {
					proxied[k] = r.Proxied
				}
			}
			switch k.rtype {
			case "A", "AAAA", "CNAME", "NS":
				ep.Targets = append(ep.Targets, r.Content)
			case "MX":
				ep.Targets = append(ep.Targets, fmt.Sprintf("%d %s", r.Priority, r.Content))
			}
		}
	}

	sort.Slice(order, func(i, j int) bool {
		if order[i].name != order[j].name {
			return order[i].name < order[j].name
		}
		return order[i].rtype < order[j].rtype
	})
	out := make([]*Endpoint, 0, len(order))
	for _, k := range order {
		ep := bucket[k]
		if k.rtype == "A" || k.rtype == "CNAME" {
			ep.ProviderSpecific = append(ep.ProviderSpecific, ProviderSpecificProperty{
				Name:  CloudflareProxiedKey,
				Value: strconv.FormatBool(proxied[k]),
			})
		}
		out = append(out, ep)
	}
	return out, nil
}

// ApplyChanges fans out a webhook Changes payload to Cloudflare
// operations. The ownership rules:
//
//   - Create/Update/Delete require Labels["owner"] on the payload Endpoint.
//     Missing label => log warning and skip the entry (contract violation
//     by the caller; we never invent an owner ourselves).
//   - For Create: refuse if a same-(name, type) row already exists with a
//     different non-empty comment (different owner) — that's a collision.
//   - For Update/Delete: match the existing row by (name, type) AND
//     comment equal to the payload owner. A mismatched comment is a
//     collision: log and skip without touching the row.
//   - All writes set the row's comment to the payload owner verbatim.
//
// Returns the first error encountered, after attempting every entry.
func (o *Orchestrator) ApplyChanges(ctx context.Context, ch Changes) error {
	o.applyMu.Lock()
	defer o.applyMu.Unlock()

	zones, err := o.resolveZones(ctx)
	if err != nil {
		return err
	}
	if len(zones) == 0 {
		return errors.New("ApplyChanges: no zones visible to API token")
	}
	zoneByName := map[string]cfclient.Zone{}
	for _, z := range zones {
		zoneByName[normalizeName(z.Name)] = z
	}

	// Snapshot existing rows per zone so we can index by (name, type)
	// without chatting to the API for every change.
	byKey := map[recKey][]cfclient.Record{}
	for _, z := range zones {
		rows, err := o.client.ListRecords(ctx, z.ID)
		if err != nil {
			return fmt.Errorf("list (apply) zone=%s: %w", z.Name, err)
		}
		for _, r := range rows {
			k := recKey{normalizeName(r.Name), strings.ToUpper(r.Type)}
			byKey[k] = append(byKey[k], r)
		}
	}

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Deletes first to free up (name, type) slots before creates.
	for _, e := range ch.Delete {
		record(o.applyDelete(ctx, e, byKey))
	}

	n := len(ch.UpdateOld)
	if n > len(ch.UpdateNew) {
		n = len(ch.UpdateNew)
	}
	for i := 0; i < n; i++ {
		record(o.applyUpdate(ctx, ch.UpdateOld[i], ch.UpdateNew[i], byKey, zoneByName))
	}

	for _, e := range ch.Create {
		record(o.applyCreate(ctx, e, byKey, zoneByName))
	}

	return firstErr
}

// --- per-operation handlers ----------------------------------------------

func (o *Orchestrator) applyCreate(
	ctx context.Context,
	e *Endpoint,
	byKey map[recKey][]cfclient.Record,
	zoneByName map[string]cfclient.Zone,
) error {
	if e == nil {
		return nil
	}
	owner, ok := ownerFromEndpoint(e)
	if !ok {
		log.Printf("orchestrator: skip create %s — Labels[\"owner\"] missing on payload", e.DNSName)
		return nil
	}
	zone, ok := o.zoneForName(e.DNSName, zoneByName)
	if !ok {
		log.Printf("orchestrator: skip create %s — no managed zone matches", e.DNSName)
		return nil
	}
	recs, err := o.endpointToRecords(e, owner, zone)
	if err != nil {
		log.Printf("orchestrator: skip create %s — %v", e.DNSName, err)
		return nil
	}
	for _, r := range recs {
		k := recKey{normalizeName(r.Name), r.Type}
		clash := false
		for _, ex := range byKey[k] {
			if ex.Comment != "" && ex.Comment != owner {
				log.Printf("orchestrator: skip create %s/%s — existing row owned by %q (request owner %q)",
					r.Type, r.Name, ex.Comment, owner)
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		// CNAME mutual exclusion (RFC 1034 §3.6.2). Cloudflare enforces
		// this on add too, but a clearer log line helps debugging.
		if r.Type == "CNAME" && hasOtherTypeAtName(byKey, k.name, "CNAME") {
			log.Printf("orchestrator: skip create CNAME %s — other types already present at name", r.Name)
			continue
		}
		if r.Type != "CNAME" && hasTypeAtName(byKey, k.name, "CNAME") {
			log.Printf("orchestrator: skip create %s/%s — CNAME present at name", r.Type, r.Name)
			continue
		}
		id, err := o.client.Add(ctx, r)
		if err != nil {
			log.Printf("orchestrator: add %s/%s failed: %v", r.Type, r.Name, err)
			return err
		}
		stored := r
		stored.ID = id
		byKey[k] = append(byKey[k], stored)
	}
	return nil
}

func (o *Orchestrator) applyUpdate(
	ctx context.Context,
	oldE, newE *Endpoint,
	byKey map[recKey][]cfclient.Record,
	zoneByName map[string]cfclient.Zone,
) error {
	if oldE == nil || newE == nil {
		return nil
	}
	owner, ok := ownerFromEndpoint(newE)
	if !ok {
		log.Printf("orchestrator: skip update %s — Labels[\"owner\"] missing on updateNew payload", newE.DNSName)
		return nil
	}
	zone, ok := o.zoneForName(newE.DNSName, zoneByName)
	if !ok {
		log.Printf("orchestrator: skip update %s — no managed zone matches", newE.DNSName)
		return nil
	}
	newRecs, err := o.endpointToRecords(newE, owner, zone)
	if err != nil {
		log.Printf("orchestrator: skip update %s — %v", newE.DNSName, err)
		return nil
	}
	if len(newRecs) == 0 {
		return nil
	}
	k := recKey{normalizeName(newE.DNSName), strings.ToUpper(newE.RecordType)}
	candidates := byKey[k]
	owned := make([]cfclient.Record, 0, len(candidates))
	for _, c := range candidates {
		if c.Comment == owner {
			owned = append(owned, c)
		} else if c.Comment != "" {
			log.Printf("orchestrator: skip update %s/%s — existing row owned by %q (request owner %q)",
				k.rtype, k.name, c.Comment, owner)
		}
	}
	if len(owned) == 0 {
		log.Printf("orchestrator: skip update %s/%s — no owned row to update", k.rtype, k.name)
		return nil
	}
	pair := len(owned)
	if len(newRecs) < pair {
		pair = len(newRecs)
	}
	for i := 0; i < pair; i++ {
		if err := o.client.Set(ctx, owned[i].ID, newRecs[i]); err != nil {
			log.Printf("orchestrator: set %s/%s failed: %v", k.rtype, k.name, err)
			return err
		}
		owned[i] = mergeForIndex(owned[i], newRecs[i])
	}
	for i := pair; i < len(owned); i++ {
		if err := o.client.Remove(ctx, owned[i].ZoneID, owned[i].ID); err != nil {
			log.Printf("orchestrator: remove (update surplus) %s failed: %v", owned[i].ID, err)
			return err
		}
	}
	for i := pair; i < len(newRecs); i++ {
		id, err := o.client.Add(ctx, newRecs[i])
		if err != nil {
			log.Printf("orchestrator: add (update surplus) %s/%s failed: %v", k.rtype, k.name, err)
			return err
		}
		added := newRecs[i]
		added.ID = id
		owned = append(owned, added)
	}
	byKey[k] = owned
	return nil
}

func (o *Orchestrator) applyDelete(
	ctx context.Context,
	e *Endpoint,
	byKey map[recKey][]cfclient.Record,
) error {
	if e == nil {
		return nil
	}
	owner, ok := ownerFromEndpoint(e)
	if !ok {
		log.Printf("orchestrator: skip delete %s — Labels[\"owner\"] missing on payload", e.DNSName)
		return nil
	}
	k := recKey{normalizeName(e.DNSName), strings.ToUpper(e.RecordType)}
	remaining := byKey[k][:0]
	for _, row := range byKey[k] {
		if row.Comment != owner {
			if row.Comment != "" {
				log.Printf("orchestrator: skip delete %s/%s — existing row owned by %q (request owner %q)",
					k.rtype, k.name, row.Comment, owner)
			}
			remaining = append(remaining, row)
			continue
		}
		if err := o.client.Remove(ctx, row.ZoneID, row.ID); err != nil {
			log.Printf("orchestrator: remove %s failed: %v", row.ID, err)
			return err
		}
	}
	byKey[k] = remaining
	return nil
}

// --- helpers --------------------------------------------------------------

type recKey struct{ name, rtype string }

func hasOtherTypeAtName(byKey map[recKey][]cfclient.Record, name, excludeType string) bool {
	for k := range byKey {
		if k.name == name && k.rtype != excludeType && len(byKey[k]) > 0 {
			return true
		}
	}
	return false
}

func hasTypeAtName(byKey map[recKey][]cfclient.Record, name, rtype string) bool {
	return len(byKey[recKey{name, rtype}]) > 0
}

// zoneForName picks the longest-matching zone whose name is a suffix of
// the FQDN. Cloudflare itself enforces the same — but we need the zone
// id to dispatch the per-record API call.
func (o *Orchestrator) zoneForName(fqdn string, zoneByName map[string]cfclient.Zone) (cfclient.Zone, bool) {
	name := normalizeName(fqdn)
	var best cfclient.Zone
	var bestOK bool
	for zn, z := range zoneByName {
		if name == zn || strings.HasSuffix(name, "."+zn) {
			if !bestOK || len(zn) > len(best.Name) {
				best = z
				bestOK = true
			}
		}
	}
	return best, bestOK
}

func normalizeName(fqdn string) string {
	return strings.ToLower(strings.TrimSuffix(fqdn, "."))
}

func ownerFromEndpoint(e *Endpoint) (string, bool) {
	if e == nil || e.Labels == nil {
		return "", false
	}
	v, ok := e.Labels["owner"]
	if !ok || v == "" {
		return "", false
	}
	return v, true
}

// endpointToRecords expands a single multi-target Endpoint into one or
// more Cloudflare Record values, one per target. TTL falls back to the
// configured default when the payload omits recordTTL. Comment is set
// verbatim from the resolved owner. Proxied falls back to ProxiedDefault
// when the providerSpecific entry is absent; explicit "false" disables.
func (o *Orchestrator) endpointToRecords(e *Endpoint, owner string, zone cfclient.Zone) ([]cfclient.Record, error) {
	if e == nil || len(e.Targets) == 0 {
		return nil, nil
	}
	rtype := strings.ToUpper(e.RecordType)
	switch rtype {
	case "A", "AAAA", "CNAME", "NS", "MX":
	default:
		return nil, fmt.Errorf("unsupported recordType %q", rtype)
	}
	ttl := int(e.RecordTTL)
	if ttl <= 0 {
		ttl = o.opts.DefaultTTL
	}
	if ttl <= 0 {
		ttl = 1 // Cloudflare "automatic"
	}
	proxied := o.proxiedFromEndpoint(e, rtype)
	name := normalizeName(e.DNSName)
	out := make([]cfclient.Record, 0, len(e.Targets))
	for _, t := range e.Targets {
		v := strings.TrimSpace(t)
		if v == "" {
			continue
		}
		r := cfclient.Record{
			ZoneID:  zone.ID,
			Name:    name,
			Type:    rtype,
			TTL:     ttl,
			Comment: owner,
			Proxied: proxied,
		}
		switch rtype {
		case "A", "AAAA":
			r.Content = strings.ToLower(v)
		case "CNAME":
			r.Content = strings.TrimSuffix(strings.ToLower(v), ".")
		case "NS":
			r.Content = strings.TrimSuffix(strings.ToLower(v), ".")
		case "MX":
			parts := strings.Fields(v)
			if len(parts) != 2 {
				return nil, fmt.Errorf("MX target %q: expected '<priority> <host>'", v)
			}
			pref, err := strconv.Atoi(parts[0])
			if err != nil {
				return nil, fmt.Errorf("MX target %q: priority not numeric", v)
			}
			r.Priority = pref
			r.Content = strings.TrimSuffix(strings.ToLower(parts[1]), ".")
		}
		out = append(out, r)
	}
	if len(out) == 0 {
		return nil, errors.New("no usable targets")
	}
	return out, nil
}

// proxiedFromEndpoint resolves the orange-cloud toggle for the given
// record type. Only A and CNAME records can be proxied on Cloudflare —
// for everything else we hard-zero the field.
func (o *Orchestrator) proxiedFromEndpoint(e *Endpoint, rtype string) bool {
	if rtype != "A" && rtype != "CNAME" {
		return false
	}
	for _, p := range e.ProviderSpecific {
		if p.Name != CloudflareProxiedKey {
			continue
		}
		b, err := strconv.ParseBool(p.Value)
		if err != nil {
			log.Printf("orchestrator: bad %s value %q on %s — treating as default", CloudflareProxiedKey, p.Value, e.DNSName)
			return o.opts.ProxiedDefault
		}
		return b
	}
	return o.opts.ProxiedDefault
}

// mergeForIndex returns the existing row with its id intact and the new
// rdata applied. Comment is preserved from the existing row — Set never
// mutates it.
func mergeForIndex(existing cfclient.Record, applied cfclient.Record) cfclient.Record {
	out := applied
	out.ID = existing.ID
	out.Comment = existing.Comment
	return out
}
