//go:build !certs

package controlapi

import "net/http"

func (runtime *Runtime) registerCertificateHandlers(_ *http.ServeMux) {}
