package cloudflare

import (
	"testing"

	"github.com/cloudflare/cloudflare-go/v7/dns"
)

// recordToNewBody/recordToUpdateBody are the only non-trivial parts of
// the SDK adapter; full HTTP integration is exercised via the orchestrator
// + a fake Client in internal/orchestrator/. Here we just verify the body
// dispatch and the Proxied-only-on-A-CNAME contract.

func TestRecordToNewBody_AIncludesProxied(t *testing.T) {
	body, err := recordToNewBody(Record{
		Type: "A", Name: "x.example.com", Content: "10.1.2.3",
		Comment: "docker-dns-operator:1", Proxied: true, TTL: 1,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	a, ok := body.(dns.ARecordParam)
	if !ok {
		t.Fatalf("expected ARecordParam, got %T", body)
	}
	if a.Proxied.Value != true || a.Content.Value != "10.1.2.3" {
		t.Fatalf("bad body: %+v", a)
	}
	if a.Comment.Value != "docker-dns-operator:1" {
		t.Fatalf("comment not set on new body: %+v", a.Comment)
	}
}

func TestRecordToNewBody_CNAMEIncludesProxied(t *testing.T) {
	body, err := recordToNewBody(Record{
		Type: "CNAME", Name: "www.example.com", Content: "app.example.com",
		Comment: "x", Proxied: false, TTL: 60,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	c, ok := body.(dns.CNAMERecordParam)
	if !ok {
		t.Fatalf("expected CNAMERecordParam, got %T", body)
	}
	if c.Proxied.Value != false {
		t.Fatalf("proxied should round-trip false: %+v", c.Proxied)
	}
}

func TestRecordToNewBody_MXSetsPriorityAndContent(t *testing.T) {
	body, err := recordToNewBody(Record{
		Type: "MX", Name: "example.com", Content: "mx1.example.com",
		Priority: 10, Comment: "x", TTL: 3600,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	m, ok := body.(dns.MXRecordParam)
	if !ok {
		t.Fatalf("expected MXRecordParam, got %T", body)
	}
	if m.Priority.Value != 10 || m.Content.Value != "mx1.example.com" {
		t.Fatalf("bad MX body: %+v", m)
	}
}

func TestRecordToNewBody_RejectsBadMXPriority(t *testing.T) {
	_, err := recordToNewBody(Record{
		Type: "MX", Name: "example.com", Content: "mx1.example.com",
		Priority: 70000, Comment: "x", TTL: 3600,
	})
	if err == nil {
		t.Fatalf("expected priority-range error")
	}
}

func TestRecordToNewBody_RejectsUnsupportedType(t *testing.T) {
	_, err := recordToNewBody(Record{Type: "TXT", Name: "x.example.com", Content: "v=spf1"})
	if err == nil {
		t.Fatalf("expected unsupported-type error")
	}
}

func TestRecordToNewBody_TTLZeroDefaultsToAutomatic(t *testing.T) {
	body, err := recordToNewBody(Record{
		Type: "A", Name: "x.example.com", Content: "10.1.2.3", TTL: 0,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	a := body.(dns.ARecordParam)
	if a.TTL.Value != dns.TTL(1) {
		t.Fatalf("TTL should default to 1 (automatic), got %v", a.TTL.Value)
	}
}

func TestRecordToUpdateBody_NSOmitsComment(t *testing.T) {
	// Comment must NOT be carried into the Update body — ownership transfer
	// guard rail. The Update params for NS don't even expose Comment in
	// our builder; assert the body type and absence of comment by reading
	// the resulting param's optional Comment field through MarshalJSON.
	body, err := recordToUpdateBody(Record{
		Type: "NS", Name: "example.com", Content: "ns1.example.com",
		Comment: "should-be-ignored", TTL: 3600,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	n, ok := body.(dns.NSRecordParam)
	if !ok {
		t.Fatalf("expected NSRecordParam, got %T", body)
	}
	// param.Field zero value means "absent on the wire" — assert that.
	if n.Comment.Present {
		t.Fatalf("Comment must NOT be present on Update body, got: %+v", n.Comment)
	}
}

func TestRecordResponseToRecord_SkipsUnknownTypes(t *testing.T) {
	rr := &dns.RecordResponse{Type: "TXT", Name: "x.example.com"}
	_, ok := recordResponseToRecord("z", rr)
	if ok {
		t.Fatalf("TXT should be skipped")
	}
}

func TestRecordResponseToRecord_RoundTripsCoreFields(t *testing.T) {
	rr := &dns.RecordResponse{
		ID: "abc", Type: "A", Name: "x.example.com",
		Content: "10.1.2.3", TTL: 300, Comment: "owner:1", Proxied: true,
	}
	got, ok := recordResponseToRecord("z1", rr)
	if !ok {
		t.Fatalf("expected ok")
	}
	if got.ID != "abc" || got.ZoneID != "z1" || got.Name != "x.example.com" ||
		got.Type != "A" || got.Content != "10.1.2.3" || got.TTL != 300 ||
		got.Comment != "owner:1" || !got.Proxied {
		t.Fatalf("round-trip wrong: %+v", got)
	}
}
