//go:build certs

package controlapi

import "net/http"

func (runtime *Runtime) registerCertificateHandlers(mux *http.ServeMux) {
	mux.HandleFunc("/v1/certs", runtime.handleCerts)
	mux.HandleFunc("/v1/certs/review", runtime.handleCertsReview)
	mux.HandleFunc("/v1/certs/", runtime.handleCertDetail)
}
