package api

import (
	"context"
	"time"
)

// HTTPS reports the ACME certificate of the domain setting. It is optional:
// without it, the settings have no https field.
type HTTPS interface {
	CertificateStatus(ctx context.Context) HTTPSStatus
}

type HTTPSStatus struct {
	State    string     `json:"state" enum:"none,requesting,valid,error" doc:"none: no domain is set. requesting: runplane waits for the first certificate. error: port 443 or 80 does not open (runplane tries again each minute), or the last request failed and no valid certificate exists."`
	NotAfter *time.Time `json:"not_after" doc:"End of the current certificate. Null without a certificate."`
	Error    string     `json:"error" doc:"Why a port does not open, or why the last certificate request failed."`
}

func (h *Handler) httpsStatus(ctx context.Context) *HTTPSStatus {
	if h.d.HTTPS == nil {
		return nil
	}
	s := h.d.HTTPS.CertificateStatus(ctx)
	return &s
}
