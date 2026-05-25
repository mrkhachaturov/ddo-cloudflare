// Package cloudflare wraps the cloudflare-go/v7 SDK behind a small
// Client interface so the orchestrator can be unit-tested with a fake.
// Conversion between the wire (FQDN strings, integer TTL seconds) and
// the SDK's typed param/response structs lives here and only here.
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	cf "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/zones"
)

// Zone is the orchestrator-facing view of a Cloudflare zone — just the
// pair we need: the API id and the user-visible name.
type Zone struct {
	ID   string
	Name string
}

// Record models a single DNS row at rest on Cloudflare.
//
// The set of fields we care about is small. Type-specific rdata fits into
// Content + Priority (Cloudflare's API returns A/AAAA/CNAME/NS targets as
// `content`; MX additionally has `priority`). Proxied is meaningful only
// for A/CNAME — Cloudflare ignores it for MX/NS.
type Record struct {
	// ID is the Cloudflare row identifier. Required for Set/Remove;
	// opaque to the orchestrator.
	ID string

	// ZoneID is the parent zone — required for the SDK's per-record API.
	ZoneID string

	// Name is the FQDN (no trailing dot) as Cloudflare stores it.
	Name string

	// Type is one of "A", "AAAA", "CNAME", "MX", "NS".
	Type string

	// TTL is the integer second value. 1 means "automatic" on Cloudflare.
	TTL int

	// Comment is the ownership bridge — we persist Labels["owner"] here
	// verbatim and read it back the same way.
	Comment string

	// Content is the type-specific value:
	//   A/AAAA  -> IP literal
	//   CNAME   -> target hostname
	//   NS      -> nameserver hostname
	//   MX      -> mail server hostname (priority lives in Priority)
	Content string

	// Priority is the MX preference (0..65535). Ignored for other types.
	Priority int

	// Proxied is the orange-cloud toggle. Honored only for A and CNAME;
	// Cloudflare ignores the field on other types.
	Proxied bool
}

// Client is the surface the orchestrator depends on. All methods take a
// context for cancellation and return wrapped errors that include the
// operation.
type Client interface {
	ListZones(ctx context.Context) ([]Zone, error)
	ListRecords(ctx context.Context, zoneID string) ([]Record, error)
	Add(ctx context.Context, r Record) (string, error)
	Set(ctx context.Context, id string, r Record) error
	Remove(ctx context.Context, zoneID, id string) error
	Ping(ctx context.Context) error
	Close() error
}

// APIClient is the production Client backed by cloudflare-go/v7.
type APIClient struct {
	apiToken string

	mu     sync.Mutex
	client *cf.Client
}

// NewAPIClient constructs an APIClient. No I/O happens here — the
// underlying SDK client is created lazily on first use.
func NewAPIClient(apiToken string) *APIClient {
	return &APIClient{apiToken: apiToken}
}

func (c *APIClient) ensureClient() *cf.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client
	}
	// cloudflare-go uses http.DefaultClient with no timeout if WithHTTPClient
	// is not set (README: "Requests do not time out by default"). Idle HTTP/2
	// connections to api.cloudflare.com get silently dropped by intermediate
	// NAT/edge after seconds-to-minutes; the Go h2 transport keeps the dead
	// conn in its pool and the next request blocks on a TLS read that never
	// returns. We mitigate at two layers:
	//   1. DisableKeepAlives — fresh TCP for every request. Adds ~100ms TLS
	//      handshake per call but eliminates pool poisoning entirely. With a
	//      CRON-tick cadence measured in tens of seconds this overhead is
	//      irrelevant.
	//   2. option.WithRequestTimeout — defence in depth: per-retry HTTP-level
	//      timeout so a stuck request can never hold the orchestrator's
	//      applyMu forever.
	httpClient := &http.Client{
		Timeout: 60 * time.Second,
		Transport: &http.Transport{
			Proxy: http.ProxyFromEnvironment,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			IdleConnTimeout:       20 * time.Second,
			DisableKeepAlives:     true,
		},
	}
	opts := []option.RequestOption{
		option.WithAPIToken(c.apiToken),
		option.WithHTTPClient(httpClient),
		option.WithRequestTimeout(30 * time.Second),
		option.WithMaxRetries(2),
	}
	if os.Getenv("CLOUDFLARE_DEBUG") == "true" {
		opts = append(opts, option.WithDebugLog(log.New(os.Stderr, "cf-sdk: ", log.LstdFlags)))
	}
	c.client = cf.NewClient(opts...)
	return c.client
}

// Close releases the cached SDK client. The cloudflare-go HTTP layer owns
// the connection pool; nothing to release explicitly, but we drop the
// reference so a fresh client is created on next use.
func (c *APIClient) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = nil
	return nil
}

// ListZones pages through every zone the API token can see.
func (c *APIClient) ListZones(ctx context.Context) ([]Zone, error) {
	api := c.ensureClient()
	iter := api.Zones.ListAutoPaging(ctx, zones.ZoneListParams{})
	var out []Zone
	for iter.Next() {
		z := iter.Current()
		out = append(out, Zone{ID: z.ID, Name: z.Name})
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("zones.list: %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// ListRecords pages through every DNS record in the given zone. The
// orchestrator filters non-managed rows by checking Comment == "" — we
// don't filter here, because /healthz also wants raw counts.
func (c *APIClient) ListRecords(ctx context.Context, zoneID string) ([]Record, error) {
	api := c.ensureClient()
	iter := api.DNS.Records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cf.F(zoneID),
	})
	var out []Record
	for iter.Next() {
		r := iter.Current()
		rec, ok := recordResponseToRecord(zoneID, &r)
		if !ok {
			continue
		}
		out = append(out, rec)
	}
	if err := iter.Err(); err != nil {
		return nil, fmt.Errorf("dns.records.list zone=%s: %w", zoneID, err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].Type < out[j].Type
	})
	return out, nil
}

// Add creates a new DNS record in the given zone. Returns the new row id.
func (c *APIClient) Add(ctx context.Context, r Record) (string, error) {
	if r.ZoneID == "" {
		return "", errors.New("Record.ZoneID is required")
	}
	body, err := recordToNewBody(r)
	if err != nil {
		return "", err
	}
	api := c.ensureClient()
	resp, err := api.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cf.F(r.ZoneID),
		Body:   body,
	})
	if err != nil {
		return "", fmt.Errorf("dns.records.new %s/%s: %w", r.Type, r.Name, err)
	}
	return resp.ID, nil
}

// Set replaces an existing DNS record by id. Comment is intentionally NOT
// included in the request — once a row exists, ownership must never
// silently transfer between operators. The orchestrator refuses to touch
// rows with a foreign owner long before reaching here.
func (c *APIClient) Set(ctx context.Context, id string, r Record) error {
	if r.ZoneID == "" {
		return errors.New("Record.ZoneID is required")
	}
	if id == "" {
		return errors.New("set: id is required")
	}
	body, err := recordToUpdateBody(r)
	if err != nil {
		return err
	}
	api := c.ensureClient()
	_, err = api.DNS.Records.Update(ctx, id, dns.RecordUpdateParams{
		ZoneID: cf.F(r.ZoneID),
		Body:   body,
	})
	if err != nil {
		return fmt.Errorf("dns.records.update %s: %w", id, err)
	}
	return nil
}

// Remove deletes a record by id.
func (c *APIClient) Remove(ctx context.Context, zoneID, id string) error {
	if zoneID == "" || id == "" {
		return errors.New("Remove: zoneID and id are required")
	}
	api := c.ensureClient()
	_, err := api.DNS.Records.Delete(ctx, id, dns.RecordDeleteParams{
		ZoneID: cf.F(zoneID),
	})
	if err != nil {
		return fmt.Errorf("dns.records.delete %s: %w", id, err)
	}
	return nil
}

// Ping issues a cheap zones.list call to verify the API token is valid
// and Cloudflare is reachable. Used by /healthz.
func (c *APIClient) Ping(ctx context.Context) error {
	api := c.ensureClient()
	iter := api.Zones.ListAutoPaging(ctx, zones.ZoneListParams{
		PerPage: cf.F(float64(1)),
	})
	// The first Next() call performs the request. We don't need a result.
	iter.Next()
	if err := iter.Err(); err != nil {
		return fmt.Errorf("zones.list: %w", err)
	}
	return nil
}

// --- decode helpers -------------------------------------------------------

// recordResponseToRecord projects a Cloudflare RecordResponse into our
// Record. Returns (_, false) for record types the sidecar doesn't manage
// (everything outside A/AAAA/CNAME/MX/NS).
func recordResponseToRecord(zoneID string, rr *dns.RecordResponse) (Record, bool) {
	t := strings.ToUpper(string(rr.Type))
	switch t {
	case "A", "AAAA", "CNAME", "MX", "NS":
	default:
		return Record{}, false
	}
	return Record{
		ID:       rr.ID,
		ZoneID:   zoneID,
		Name:     rr.Name,
		Type:     t,
		TTL:      int(rr.TTL),
		Comment:  rr.Comment,
		Content:  rr.Content,
		Priority: int(rr.Priority),
		Proxied:  rr.Proxied,
	}, true
}

// recordToNewBody builds the typed cloudflare-go param body for a new
// record. The SDK requires a per-type struct (ARecordParam etc.) so we
// dispatch by Record.Type. Proxied is set only on A/CNAME.
func recordToNewBody(r Record) (dns.RecordNewParamsBodyUnion, error) {
	if r.Name == "" {
		return nil, errors.New("Record.Name is required")
	}
	if r.Content == "" {
		return nil, fmt.Errorf("%s: content is required", r.Type)
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 1
	}
	switch r.Type {
	case "A":
		return dns.ARecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.ARecordTypeA),
			Content: cf.F(r.Content),
			Comment: cf.F(r.Comment),
			Proxied: cf.F(r.Proxied),
		}, nil
	case "AAAA":
		return dns.AAAARecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.AAAARecordTypeAAAA),
			Content: cf.F(r.Content),
			Comment: cf.F(r.Comment),
			Proxied: cf.F(r.Proxied),
		}, nil
	case "CNAME":
		return dns.CNAMERecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.CNAMERecordTypeCNAME),
			Content: cf.F(r.Content),
			Comment: cf.F(r.Comment),
			Proxied: cf.F(r.Proxied),
		}, nil
	case "MX":
		if r.Priority < 0 || r.Priority > 65535 {
			return nil, fmt.Errorf("MX: priority out of range: %d", r.Priority)
		}
		return dns.MXRecordParam{
			Name:     cf.F(r.Name),
			TTL:      cf.F(dns.TTL(float64(ttl))),
			Type:     cf.F(dns.MXRecordTypeMX),
			Content:  cf.F(r.Content),
			Priority: cf.F(float64(r.Priority)),
			Comment:  cf.F(r.Comment),
		}, nil
	case "NS":
		return dns.NSRecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.NSRecordTypeNS),
			Content: cf.F(r.Content),
			Comment: cf.F(r.Comment),
		}, nil
	}
	return nil, fmt.Errorf("unsupported type %q", r.Type)
}

// recordToUpdateBody mirrors recordToNewBody but omits Comment — see Set.
func recordToUpdateBody(r Record) (dns.RecordUpdateParamsBodyUnion, error) {
	if r.Name == "" {
		return nil, errors.New("Record.Name is required")
	}
	if r.Content == "" {
		return nil, fmt.Errorf("%s: content is required", r.Type)
	}
	ttl := r.TTL
	if ttl <= 0 {
		ttl = 1
	}
	switch r.Type {
	case "A":
		return dns.ARecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.ARecordTypeA),
			Content: cf.F(r.Content),
			Proxied: cf.F(r.Proxied),
		}, nil
	case "AAAA":
		return dns.AAAARecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.AAAARecordTypeAAAA),
			Content: cf.F(r.Content),
			Proxied: cf.F(r.Proxied),
		}, nil
	case "CNAME":
		return dns.CNAMERecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.CNAMERecordTypeCNAME),
			Content: cf.F(r.Content),
			Proxied: cf.F(r.Proxied),
		}, nil
	case "MX":
		if r.Priority < 0 || r.Priority > 65535 {
			return nil, fmt.Errorf("MX: priority out of range: %d", r.Priority)
		}
		return dns.MXRecordParam{
			Name:     cf.F(r.Name),
			TTL:      cf.F(dns.TTL(float64(ttl))),
			Type:     cf.F(dns.MXRecordTypeMX),
			Content:  cf.F(r.Content),
			Priority: cf.F(float64(r.Priority)),
		}, nil
	case "NS":
		return dns.NSRecordParam{
			Name:    cf.F(r.Name),
			TTL:     cf.F(dns.TTL(float64(ttl))),
			Type:    cf.F(dns.NSRecordTypeNS),
			Content: cf.F(r.Content),
		}, nil
	}
	return nil, fmt.Errorf("unsupported type %q", r.Type)
}
