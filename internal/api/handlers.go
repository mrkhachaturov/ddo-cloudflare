package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mrkhachaturov/ddo-cloudflare/internal/orchestrator"
)

// Provider is the orchestrator-facing surface the handler needs. Defined
// as an interface so tests can fake it without touching real Cloudflare.
type Provider interface {
	Zones(ctx context.Context) ([]string, error)
	ListRecords(ctx context.Context) ([]*orchestrator.Endpoint, error)
	ApplyChanges(ctx context.Context, ch orchestrator.Changes) error
}

// Healther exposes a backend ping check for /healthz. Optional — when nil,
// /healthz reports "ready" unconditionally (useful in unit tests).
type Healther interface {
	Ping(ctx context.Context) error
}

// Handlers owns the four external-dns v1 endpoints plus /healthz.
type Handlers struct {
	provider Provider
	healther Healther
}

// NewHandlers wires the orchestrator and (optional) health-prober into the
// HTTP surface.
func NewHandlers(p Provider, h Healther) *Handlers {
	return &Handlers{provider: p, healther: h}
}

// Negotiate handles GET / — external-dns calls this first to discover the
// domain filter we accept records for. The body is `{"filters":["zone1"]}`.
func (h *Handlers) Negotiate(w http.ResponseWriter, r *http.Request) {
	zones, err := h.provider.Zones(r.Context())
	if err != nil {
		writeWebhookJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeWebhookJSON(w, http.StatusOK, Filters{Filters: zones})
}

// Records handles GET /records — returns every Endpoint the sidecar manages,
// reconstructed from the Cloudflare `comment` field as the ownership bridge.
func (h *Handlers) Records(w http.ResponseWriter, r *http.Request) {
	eps, err := h.provider.ListRecords(r.Context())
	if err != nil {
		writeWebhookJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]*Endpoint, 0, len(eps))
	for _, e := range eps {
		out = append(out, toWireEndpoint(e))
	}
	writeWebhookJSON(w, http.StatusOK, out)
}

// ApplyChanges handles POST /records — decodes Changes and forwards to the
// orchestrator. Success is 204 No Content (external-dns expects an empty
// body on success).
func (h *Handlers) ApplyChanges(w http.ResponseWriter, r *http.Request) {
	var wire Changes
	if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
		writeWebhookJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	if err := h.provider.ApplyChanges(r.Context(), toDomainChanges(&wire)); err != nil {
		writeWebhookJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// AdjustEndpoints handles POST /adjustendpoints — external-dns sometimes
// calls this between planning and applying to let the provider normalise
// targets/TTLs. Cloudflare normalisation lives inside ApplyChanges, so we
// echo the input unchanged.
func (h *Handlers) AdjustEndpoints(w http.ResponseWriter, r *http.Request) {
	var in []*Endpoint
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeWebhookJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json: " + err.Error()})
		return
	}
	writeWebhookJSON(w, http.StatusOK, in)
}

// Healthz reports backend reachability.
//
//   - 200 with status="ready" when the most recent Ping succeeded (or no
//     prober wired — back-compat with the unit tests).
//   - 503 with status="unreachable" when the most recent Ping failed.
func (h *Handlers) Healthz(w http.ResponseWriter, r *http.Request) {
	if h.healther == nil {
		writeWebhookJSON(w, http.StatusOK, HealthResponse{OK: true, Status: "ready"})
		return
	}
	if err := h.healther.Ping(r.Context()); err != nil {
		writeWebhookJSON(w, http.StatusServiceUnavailable, HealthResponse{
			OK: false, Status: "unreachable", Detail: err.Error(),
		})
		return
	}
	writeWebhookJSON(w, http.StatusOK, HealthResponse{OK: true, Status: "ready"})
}

// --- helpers --------------------------------------------------------------

func writeWebhookJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", MediaTypeFormatAndVersion)
	w.Header().Set("Vary", "Content-Type")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func toDomainChanges(c *Changes) orchestrator.Changes {
	return orchestrator.Changes{
		Create:    toDomainEndpoints(c.Create),
		UpdateOld: toDomainEndpoints(c.UpdateOld),
		UpdateNew: toDomainEndpoints(c.UpdateNew),
		Delete:    toDomainEndpoints(c.Delete),
	}
}

func toDomainEndpoints(in []*Endpoint) []*orchestrator.Endpoint {
	if len(in) == 0 {
		return nil
	}
	out := make([]*orchestrator.Endpoint, 0, len(in))
	for _, e := range in {
		if e == nil {
			out = append(out, nil)
			continue
		}
		out = append(out, &orchestrator.Endpoint{
			DNSName:          e.DNSName,
			Targets:          append([]string(nil), e.Targets...),
			RecordType:       e.RecordType,
			RecordTTL:        e.RecordTTL,
			Labels:           copyLabels(e.Labels),
			ProviderSpecific: toDomainProviderSpecific(e.ProviderSpecific),
		})
	}
	return out
}

func toWireEndpoint(e *orchestrator.Endpoint) *Endpoint {
	if e == nil {
		return nil
	}
	return &Endpoint{
		DNSName:          e.DNSName,
		Targets:          append([]string(nil), e.Targets...),
		RecordType:       e.RecordType,
		RecordTTL:        e.RecordTTL,
		Labels:           copyLabels(e.Labels),
		ProviderSpecific: toWireProviderSpecific(e.ProviderSpecific),
	}
}

func copyLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func toDomainProviderSpecific(in []ProviderSpecificProperty) []orchestrator.ProviderSpecificProperty {
	if len(in) == 0 {
		return nil
	}
	out := make([]orchestrator.ProviderSpecificProperty, 0, len(in))
	for _, p := range in {
		out = append(out, orchestrator.ProviderSpecificProperty{Name: p.Name, Value: p.Value})
	}
	return out
}

func toWireProviderSpecific(in []orchestrator.ProviderSpecificProperty) []ProviderSpecificProperty {
	if len(in) == 0 {
		return nil
	}
	out := make([]ProviderSpecificProperty, 0, len(in))
	for _, p := range in {
		out = append(out, ProviderSpecificProperty{Name: p.Name, Value: p.Value})
	}
	return out
}
