package orchestrator

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	cfclient "github.com/mrkhachaturov/ddo-cloudflare/internal/cloudflare"
)

// fakeClient is a deterministic in-memory cloudflare.Client for unit tests.
// IDs are assigned monotonically so tests can assert against stable values.
type fakeClient struct {
	mu      sync.Mutex
	zones   []cfclient.Zone
	rows    []cfclient.Record
	nextID  int
	addErr  error
	setErr  error
	rmErr   error
	listErr error
	zoneErr error
	pingErr error

	addCalls    []cfclient.Record
	setCalls    []setCall
	removeCalls []removeCall
	listCalls   int
}

type setCall struct {
	id     string
	record cfclient.Record
}

type removeCall struct {
	zoneID string
	id     string
}

func newFakeClient(zones []cfclient.Zone, initial ...cfclient.Record) *fakeClient {
	f := &fakeClient{zones: zones, rows: append([]cfclient.Record(nil), initial...)}
	for i := range f.rows {
		if f.rows[i].ID == "" {
			f.nextID++
			f.rows[i].ID = "id-" + itoa(f.nextID)
		}
	}
	return f
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func (f *fakeClient) ListZones(_ context.Context) ([]cfclient.Zone, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.zoneErr != nil {
		return nil, f.zoneErr
	}
	return append([]cfclient.Zone(nil), f.zones...), nil
}

func (f *fakeClient) ListRecords(_ context.Context, zoneID string) ([]cfclient.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]cfclient.Record, 0, len(f.rows))
	for _, r := range f.rows {
		if r.ZoneID == zoneID {
			out = append(out, r)
		}
	}
	return out, nil
}

func (f *fakeClient) Add(_ context.Context, r cfclient.Record) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.addErr != nil {
		return "", f.addErr
	}
	f.nextID++
	r.ID = "id-" + itoa(f.nextID)
	f.rows = append(f.rows, r)
	f.addCalls = append(f.addCalls, r)
	return r.ID, nil
}

func (f *fakeClient) Set(_ context.Context, id string, r cfclient.Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.setErr != nil {
		return f.setErr
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			r.ID = id
			r.Comment = f.rows[i].Comment // never mutated via Set
			f.rows[i] = r
			f.setCalls = append(f.setCalls, setCall{id, r})
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeClient) Remove(_ context.Context, zoneID, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rmErr != nil {
		return f.rmErr
	}
	for i := range f.rows {
		if f.rows[i].ID == id {
			f.rows = append(f.rows[:i], f.rows[i+1:]...)
			f.removeCalls = append(f.removeCalls, removeCall{zoneID, id})
			return nil
		}
	}
	return errors.New("not found")
}

func (f *fakeClient) Ping(_ context.Context) error { return f.pingErr }

func (f *fakeClient) Close() error { return nil }

var (
	zoneExample = cfclient.Zone{ID: "zone-ex", Name: "example.com"}
	zoneInt     = cfclient.Zone{ID: "zone-int", Name: "internal.example.com"}
)

func defaultOpts() Options {
	return Options{DefaultTTL: 1}
}

// -- Zones / ListRecords ---------------------------------------------------

func TestZones_FallsBackToAllVisibleWhenUnconfigured(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample, zoneInt})
	o := New(defaultOpts(), fc)
	got, err := o.Zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 zones, got %+v", got)
	}
}

func TestZones_RespectsConfiguredAllowList(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample, zoneInt})
	o := New(Options{Zones: []string{"example.com."}}, fc)
	got, err := o.Zones(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "example.com" {
		t.Fatalf("allow-list not applied: %+v", got)
	}
}

func TestListRecords_RoundTripsOwnerFromComment(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 300, Comment: "docker-dns-operator:1", Proxied: false},
		cfclient.Record{ZoneID: "zone-ex", Name: "manual.example.com", Type: "A", Content: "10.0.0.99", TTL: 300, Comment: ""},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("expected exactly one managed endpoint, got %d", len(got))
	}
	if got[0].DNSName != "app.example.com" || got[0].Labels["owner"] != "docker-dns-operator:1" {
		t.Fatalf("owner not round-tripped: %+v", got[0])
	}
	if got[0].RecordTTL != 300 {
		t.Fatalf("TTL not preserved: %d", got[0].RecordTTL)
	}
}

func TestListRecords_TwoOperatorsBothRoundTrip(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "a.example.com", Type: "A", Content: "10.0.0.1", Comment: "docker-dns-operator:alpha"},
		cfclient.Record{ZoneID: "zone-ex", Name: "b.example.com", Type: "A", Content: "10.0.0.2", Comment: "docker-dns-operator:beta"},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(got))
	}
	byName := map[string]string{}
	for _, e := range got {
		byName[e.DNSName] = e.Labels["owner"]
	}
	if byName["a.example.com"] != "docker-dns-operator:alpha" || byName["b.example.com"] != "docker-dns-operator:beta" {
		t.Fatalf("owners not preserved per row: %+v", byName)
	}
}

func TestListRecords_EmitsCloudflareProxiedOnAAndCNAME(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "a.example.com", Type: "A", Content: "10.0.0.1", Comment: "x", Proxied: true},
		cfclient.Record{ZoneID: "zone-ex", Name: "cn.example.com", Type: "CNAME", Content: "a.example.com", Comment: "x", Proxied: false},
		cfclient.Record{ZoneID: "zone-ex", Name: "mail.example.com", Type: "MX", Content: "mx1.example.com", Priority: 10, Comment: "x"},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"A:a.example.com":      "true",
		"CNAME:cn.example.com": "false",
	}
	for _, e := range got {
		k := e.RecordType + ":" + e.DNSName
		if expected, ok := want[k]; ok {
			if len(e.ProviderSpecific) != 1 || e.ProviderSpecific[0].Name != CloudflareProxiedKey || e.ProviderSpecific[0].Value != expected {
				t.Fatalf("bad providerSpecific on %s: %+v", k, e.ProviderSpecific)
			}
			delete(want, k)
		} else if e.RecordType == "MX" {
			if len(e.ProviderSpecific) != 0 {
				t.Fatalf("MX must not carry cloudflare-proxied: %+v", e.ProviderSpecific)
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("missing expected endpoints: %+v", want)
	}
}

func TestListRecords_GroupsMultipleTargetsAndMxFormatsWithPriority(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "mail.example.com", Type: "MX", Content: "mx1.example.com", Priority: 10, Comment: "docker-dns-operator:1"},
		cfclient.Record{ZoneID: "zone-ex", Name: "mail.example.com", Type: "MX", Content: "mx2.example.com", Priority: 20, Comment: "docker-dns-operator:1"},
	)
	o := New(defaultOpts(), fc)
	got, err := o.ListRecords(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Targets) != 2 {
		t.Fatalf("expected one MX endpoint with two targets, got %+v", got)
	}
	want := map[string]bool{"10 mx1.example.com": true, "20 mx2.example.com": true}
	for _, tgt := range got[0].Targets {
		if !want[tgt] {
			t.Fatalf("unexpected MX target %q", tgt)
		}
	}
}

// -- ApplyChanges create ---------------------------------------------------

func TestApplyChanges_CreateHonorsOwnerFromPayload(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName:    "app.example.com",
			RecordType: "A",
			RecordTTL:  120,
			Targets:    []string{"10.1.2.3"},
			Labels:     map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 1 {
		t.Fatalf("expected one Add call, got %d", len(fc.addCalls))
	}
	added := fc.addCalls[0]
	if added.Comment != "docker-dns-operator:alpha" {
		t.Fatalf("comment must be the payload owner, got %q", added.Comment)
	}
	if added.Type != "A" || added.Content != "10.1.2.3" || added.Name != "app.example.com" {
		t.Fatalf("rdata wrong: %+v", added)
	}
	if added.TTL != 120 {
		t.Fatalf("TTL not preserved: %d", added.TTL)
	}
	if added.ZoneID != "zone-ex" {
		t.Fatalf("zone resolution wrong: %q", added.ZoneID)
	}
}

func TestApplyChanges_CreateUsesProxiedDefaultWhenAbsent(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(Options{ProxiedDefault: true, DefaultTTL: 1}, fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"1.2.3.4"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fc.addCalls[0].Proxied {
		t.Fatalf("proxied default should propagate when providerSpecific absent: %+v", fc.addCalls[0])
	}
}

func TestApplyChanges_CreateHonorsCloudflareProxiedFalse(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(Options{ProxiedDefault: true, DefaultTTL: 1}, fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"1.2.3.4"},
			Labels: map[string]string{"owner": "x"},
			ProviderSpecific: []ProviderSpecificProperty{
				{Name: CloudflareProxiedKey, Value: "false"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fc.addCalls[0].Proxied {
		t.Fatalf("explicit cloudflare-proxied=false must override ProxiedDefault=true")
	}
}

func TestApplyChanges_CreateMXIgnoresProxied(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(Options{ProxiedDefault: true, DefaultTTL: 1}, fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "example.com", RecordType: "MX", Targets: []string{"10 mx1.example.com"},
			Labels: map[string]string{"owner": "x"},
			ProviderSpecific: []ProviderSpecificProperty{
				{Name: CloudflareProxiedKey, Value: "true"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fc.addCalls[0].Proxied {
		t.Fatalf("MX must never be proxied even if requested: %+v", fc.addCalls[0])
	}
	if fc.addCalls[0].Priority != 10 || fc.addCalls[0].Content != "mx1.example.com" {
		t.Fatalf("MX not parsed: %+v", fc.addCalls[0])
	}
}

func TestApplyChanges_CreateSkippedWhenOwnerLabelMissing(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.1.2.3"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 0 {
		t.Fatalf("must not add when owner missing")
	}
}

func TestApplyChanges_CreateSkipsCollisionWithDifferentOwner(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.99", Comment: "docker-dns-operator:beta"},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.1.2.3"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 0 {
		t.Fatalf("collision must not add")
	}
}

func TestApplyChanges_CreateMultiTargetAExpandsToMultipleRows(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.0.0.1", "10.0.0.2"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 2 {
		t.Fatalf("multi-target A must add one row per target, got %d", len(fc.addCalls))
	}
}

func TestApplyChanges_CreateSkipsCNAMEWhenOtherTypePresent(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", Comment: "x"},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "CNAME", Targets: []string{"other.example.com"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 0 {
		t.Fatalf("CNAME mutual exclusion violated")
	}
}

func TestApplyChanges_CreateSkipsUnsupportedRecordType(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "x.example.com", RecordType: "TXT", Targets: []string{"v=spf1"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.addCalls) != 0 {
		t.Fatalf("TXT must be skipped")
	}
}

// -- ApplyChanges update ---------------------------------------------------

func TestApplyChanges_UpdateMutatesProxyOnSameOwner(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", TTL: 300, Comment: "docker-dns-operator:alpha", Proxied: false},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		UpdateOld: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.0.0.1"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
		UpdateNew: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.0.0.1"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
			ProviderSpecific: []ProviderSpecificProperty{
				{Name: CloudflareProxiedKey, Value: "true"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.setCalls) != 1 {
		t.Fatalf("expected one Set call, got %d", len(fc.setCalls))
	}
	if !fc.setCalls[0].record.Proxied {
		t.Fatalf("proxied not flipped to true: %+v", fc.setCalls[0])
	}
}

func TestApplyChanges_UpdateSkippedOnOwnerMismatch(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", Comment: "docker-dns-operator:beta"},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		UpdateOld: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.0.0.1"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
		UpdateNew: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"10.9.9.9"},
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.setCalls) != 0 {
		t.Fatalf("must not modify a row owned by a different operator")
	}
}

// -- ApplyChanges delete ---------------------------------------------------

func TestApplyChanges_DeleteRemovesOwnedRow(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", Comment: "docker-dns-operator:alpha"},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Delete: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A",
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.removeCalls) != 1 || fc.removeCalls[0].zoneID != "zone-ex" {
		t.Fatalf("expected one Remove call against zone-ex, got %+v", fc.removeCalls)
	}
}

func TestApplyChanges_DeleteSkippedOnOwnerMismatch(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", Comment: "docker-dns-operator:beta"},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Delete: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A",
			Labels: map[string]string{"owner": "docker-dns-operator:alpha"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.removeCalls) != 0 {
		t.Fatalf("must not delete a row owned by a different operator")
	}
}

func TestApplyChanges_DeleteSkippedWhenOwnerMissing(t *testing.T) {
	fc := newFakeClient(
		[]cfclient.Zone{zoneExample},
		cfclient.Record{ZoneID: "zone-ex", Name: "app.example.com", Type: "A", Content: "10.0.0.1", Comment: "docker-dns-operator:alpha"},
	)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Delete: []*Endpoint{{DNSName: "app.example.com", RecordType: "A"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fc.removeCalls) != 0 {
		t.Fatalf("must not delete when owner missing")
	}
}

// -- ApplyChanges error propagation ---------------------------------------

func TestApplyChanges_ListErrorBubbles(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	fc.listErr = errors.New("network down")
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{})
	if err == nil || !strings.Contains(err.Error(), "network down") {
		t.Fatalf("expected list error to surface, got %v", err)
	}
}

func TestApplyChanges_NoZonesIsHardError(t *testing.T) {
	fc := newFakeClient(nil)
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{})
	if err == nil || !strings.Contains(err.Error(), "no zones") {
		t.Fatalf("expected no-zones error, got %v", err)
	}
}

func TestApplyChanges_AddErrorReturned(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample})
	fc.addErr = errors.New("CF rejected")
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.example.com", RecordType: "A", Targets: []string{"1.1.1.1"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err == nil || !strings.Contains(err.Error(), "CF rejected") {
		t.Fatalf("expected add error, got %v", err)
	}
}

// -- zone routing ----------------------------------------------------------

func TestApplyChanges_PicksLongestMatchingZone(t *testing.T) {
	fc := newFakeClient([]cfclient.Zone{zoneExample, zoneInt})
	o := New(defaultOpts(), fc)
	err := o.ApplyChanges(context.Background(), Changes{
		Create: []*Endpoint{{
			DNSName: "app.internal.example.com", RecordType: "A", Targets: []string{"10.0.0.1"},
			Labels: map[string]string{"owner": "x"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if fc.addCalls[0].ZoneID != "zone-int" {
		t.Fatalf("expected longest-suffix zone match, got %q", fc.addCalls[0].ZoneID)
	}
}
