package controlapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cloudapp3/vmflow/certstore"
)

// handleCerts handles GET /v1/certs (list) and POST /v1/certs (import).
func (runtime *Runtime) handleCerts(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		runtime.handleCertsList(w, r)
	case http.MethodPost:
		if !runtime.authorizeWrite(w, r) {
			return
		}
		runtime.handleCertsImport(w, r)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// handleCertDetail handles GET/DELETE /v1/certs/{domain} and POST /v1/certs/obtain.
func (runtime *Runtime) handleCertDetail(w http.ResponseWriter, r *http.Request) {
	// Extract path suffix after "/v1/certs/"
	suffix := strings.TrimPrefix(r.URL.Path, "/v1/certs/")
	if suffix == "" {
		http.Redirect(w, r, "/v1/certs", http.StatusMovedPermanently)
		return
	}

	switch {
	case suffix == "obtain" && r.Method == http.MethodPost:
		if !runtime.authorizeWrite(w, r) {
			return
		}
		runtime.handleCertsObtain(w, r)
	case r.Method == http.MethodGet:
		runtime.handleCertsGet(w, r, suffix)
	case r.Method == http.MethodDelete:
		if !runtime.authorizeWrite(w, r) {
			return
		}
		runtime.handleCertsDelete(w, r, suffix)
	default:
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
	}
}

// certStore returns the cert store, or writes 503 and returns nil if unavailable.
func (runtime *Runtime) certStore() *certstore.Store {
	if runtime != nil && runtime.CertStore != nil {
		return runtime.CertStore
	}
	return nil
}

func (runtime *Runtime) handleCertsList(w http.ResponseWriter, r *http.Request) {
	store := runtime.certStore()
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "certificate store not available",
		})
		return
	}
	items := store.List()
	if items == nil {
		items = []certstore.CertMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (runtime *Runtime) handleCertsImport(w http.ResponseWriter, r *http.Request) {
	store := runtime.certStore()
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "certificate store not available",
		})
		return
	}

	var req struct {
		Domain string `json:"domain"`
		Cert   string `json:"cert"`
		Key    string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}

	if strings.TrimSpace(req.Domain) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing domain"})
		return
	}
	if strings.TrimSpace(req.Cert) == "" || strings.TrimSpace(req.Key) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing cert or key"})
		return
	}

	meta, err := store.Import(r.Context(), req.Domain, []byte(req.Cert), []byte(req.Key))
	if err != nil {
		runtime.log(r).Warn("cert import failed", "component", "controlapi", "event", "cert_import_failed", "domain", req.Domain, "error", err)
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}

	runtime.log(r).Info("cert imported", "component", "controlapi", "event", "cert_import", "domain", req.Domain)
	writeJSON(w, http.StatusCreated, map[string]any{"ok": true, "cert": meta})
}

func (runtime *Runtime) handleCertsGet(w http.ResponseWriter, r *http.Request, domain string) {
	store := runtime.certStore()
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "certificate store not available",
		})
		return
	}

	meta, ok := store.Get(domain)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": fmt.Sprintf("certificate not found: %s", domain)})
		return
	}

	// Add computed fields
	response := map[string]any{
		"cert":           meta,
		"days_remaining": daysUntilExpiry(meta.NotAfter),
	}
	writeJSON(w, http.StatusOK, response)
}

func (runtime *Runtime) handleCertsDelete(w http.ResponseWriter, r *http.Request, domain string) {
	store := runtime.certStore()
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "certificate store not available",
		})
		return
	}

	if err := store.Delete(r.Context(), domain); err != nil {
		runtime.log(r).Warn("cert delete failed", "component", "controlapi", "event", "cert_delete_failed", "domain", domain, "error", err)
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
		return
	}

	runtime.log(r).Info("cert deleted", "component", "controlapi", "event", "cert_delete", "domain", domain)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (runtime *Runtime) handleCertsObtain(w http.ResponseWriter, r *http.Request) {
	store := runtime.certStore()
	if store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "certificate store not available",
		})
		return
	}

	var req struct {
		Domains []string `json:"domains"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json body"})
		return
	}

	if len(req.Domains) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "missing domains"})
		return
	}

	runtime.log(r).Info("cert obtain requested", "component", "controlapi", "event", "cert_obtain", "domains", strings.Join(req.Domains, ","))

	if err := store.Obtain(r.Context(), req.Domains); err != nil {
		runtime.log(r).Warn("cert obtain failed", "component", "controlapi", "event", "cert_obtain_failed", "domains", strings.Join(req.Domains, ","), "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "domains": req.Domains})
}

// daysUntilExpiry parses an RFC3339 timestamp and returns days until that time.
// Returns -1 if the timestamp is empty or unparseable.
func daysUntilExpiry(notAfter string) int {
	if notAfter == "" {
		return -1
	}
	t, err := time.Parse(time.RFC3339, notAfter)
	if err != nil {
		return -1
	}
	d := int(time.Until(t).Hours() / 24)
	if d < 0 {
		return 0
	}
	return d
}

// handleCertsReview handles GET/POST /v1/certs/review.
func (runtime *Runtime) handleCertsReview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}

	if runtime.CertReviewer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"error": "certificate review not available",
		})
		return
	}

	result := runtime.CertReviewer.Review()

	status := http.StatusOK
	if !result.OK {
		status = http.StatusConflict
	}

	runtime.log(r).Info("cert review completed", "component", "controlapi", "event", "cert_review", "ok", result.OK, "critical", result.Critical, "warnings", result.Warnings, "info", result.Info)
	writeJSON(w, status, result)
}
