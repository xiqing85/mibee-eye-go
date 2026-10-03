package onvifgo

import "testing"

func TestSoapActionFromEnvelope(t *testing.T) {
	cases := map[string]string{
		// SOAP 1.2 qualified body child.
		`<?xml version="1.0"?><env:Envelope xmlns:env="http://www.w3.org/2003/05/soap-envelope"><env:Body><tds:GetDeviceInformation></tds:GetDeviceInformation></env:Body></env:Envelope>`: "GetDeviceInformation",
		// Unqualified child.
		`<soap:Envelope><soap:Body><GetServices/></soap:Body></soap:Envelope>`: "GetServices",
		// Attributes on the child element.
		`<Body><trt:GetStreamUri ProfileToken="main"></trt:GetStreamUri></Body>`: "GetStreamUri",
		// WS-Discovery probe (no SOAP body) counts as probe.
		`<?xml version="1.0"?><Probe xmlns="http://schemas.xmlsoap.org/ws/2005/04/discovery">`: "probe",
		// Empty read.
		"": "probe",
	}
	for in, want := range cases {
		if got := soapActionFromEnvelope([]byte(in)); got != want {
			t.Errorf("soapActionFromEnvelope(%q) = %q, want %q", in, got, want)
		}
	}
}
