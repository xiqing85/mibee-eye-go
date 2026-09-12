package onvifgo

import (
	"bytes"
	"encoding/xml"
	"io"
	"net/http"
)

// maxBodyBytes caps sniffed request bodies (same limit the historical
// server applied).
const maxBodyBytes = 1 << 20

// probeSniffer routes WS-Discovery HTTP probes to the discovery responder
// and everything else to the SOAP handler.
//
// Deliberately interception rather than a registered "Probe" action with
// soap.RawEnvelope (#39): delegating to the responder's ServeHTTP keeps
// its exact semantics — 204 No Content on Types-filter miss, 400 on
// garbage — which a SOAP handler return value cannot express — and
// answers on any path, like the historical server did.
type probeSniffer struct {
	soap  http.Handler
	probe http.Handler
}

func (p probeSniffer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		p.soap.ServeHTTP(w, r)
		return
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(data))

	if bodyAction(data) == "Probe" {
		p.probe.ServeHTTP(w, r)
		return
	}

	p.soap.ServeHTTP(w, r)
}

// bodyAction returns the local name of the first element inside the SOAP
// Body (namespace-agnostic), or "" when it cannot be determined. Same
// algorithm the SOAP handler uses to pick the action.
func bodyAction(body []byte) string {
	decoder := xml.NewDecoder(bytes.NewReader(body))
	inBody := false
	depth := 0

	for {
		token, err := decoder.Token()
		if err != nil {
			return ""
		}

		switch t := token.(type) {
		case xml.StartElement:
			depth++
			if t.Name.Local == "Body" {
				inBody = true
			} else if inBody && depth > 2 {
				return t.Name.Local
			}
		case xml.EndElement:
			depth--
			if t.Name.Local == "Body" {
				inBody = false
			}
		}
	}
}
