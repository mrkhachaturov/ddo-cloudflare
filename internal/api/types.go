// Package api implements the kubernetes-sigs/external-dns webhook provider
// v1 wire contract:
//
//	GET  /         -> {"include": ["<zone1>", ...]} (DomainFilter)
//	GET  /records  -> []Endpoint
//	POST /records  -> 204, body = Changes
//	GET  /healthz  -> 200/503, body = HealthResponse
//
// The Accept/Content-Type media type is
// "application/external.dns.webhook+json;version=1".
//
// Field shapes mirror sigs.k8s.io/external-dns/endpoint.Endpoint, with
// omitempty applied so a sparse Endpoint serialises minimally.
package api

// MediaTypeFormatAndVersion is the Content-Type/Accept value external-dns
// uses to negotiate the v1 webhook contract.
const MediaTypeFormatAndVersion = "application/external.dns.webhook+json;version=1"

// ProviderSpecificProperty mirrors endpoint.ProviderSpecificProperty.
// For Cloudflare we round-trip the
// "external-dns.alpha.kubernetes.io/cloudflare-proxied" property between
// the wire (string "true"/"false") and the SDK's Proxied bool, matching
// upstream external-dns' Cloudflare provider semantics.
type ProviderSpecificProperty struct {
	Name  string `json:"name,omitempty"`
	Value string `json:"value,omitempty"`
}

// Endpoint mirrors endpoint.Endpoint with json tags identical to upstream.
// Targets are decoded per record type:
//   - A/AAAA: each target is a single IP literal.
//   - CNAME/NS: each target is a hostname (with or without trailing dot).
//   - MX: each target is "<priority> <server>" (canonical zone-file form).
type Endpoint struct {
	DNSName          string                     `json:"dnsName,omitempty"`
	Targets          []string                   `json:"targets,omitempty"`
	RecordType       string                     `json:"recordType,omitempty"`
	RecordTTL        int64                      `json:"recordTTL,omitempty"`
	SetIdentifier    string                     `json:"setIdentifier,omitempty"`
	Labels           map[string]string          `json:"labels,omitempty"`
	ProviderSpecific []ProviderSpecificProperty `json:"providerSpecific,omitempty"`
}

// Changes is the body of POST /records — paired UpdateOld/UpdateNew slices
// describe in-place mutations; Create/Delete are exactly what they say.
type Changes struct {
	Create    []*Endpoint `json:"create,omitempty"`
	UpdateOld []*Endpoint `json:"updateOld,omitempty"`
	UpdateNew []*Endpoint `json:"updateNew,omitempty"`
	Delete    []*Endpoint `json:"delete,omitempty"`
}

// DomainFilter is the response body for GET /. Mirrors the JSON form of
// endpoint.DomainFilter in upstream external-dns (domainFilterSerde). The
// `include` field is what controllers actually read — sending the legacy
// `filters` key here would parse upstream as "no filter" and the
// controller would route every record through us regardless of zone.
//
// We only populate Include today (the sidecar's configured zone list).
// Exclude / RegexInclude / RegexExclude exist in the contract but aren't
// useful for this sidecar yet; omit them rather than serialise empty
// strings that upstream would treat as "this filter is set".
type DomainFilter struct {
	Include []string `json:"include,omitempty"`
}

// HealthResponse is the body of GET /healthz. Reports backend reachability
// (we issue a cheap zones.list against the Cloudflare API).
type HealthResponse struct {
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}
