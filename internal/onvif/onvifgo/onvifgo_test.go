package onvifgo

import (
	"context"
	"crypto/sha1" //nolint:gosec // SHA1 is the ONVIF digest formula
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/camera"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/config"
	"github.com/Mi-Bee-Studio/mibee-eye-raspi/internal/onvif"
)

// The tests in this file lock the wire-level contract the MiBee NVR depends
// on (raw SOAP local-name matching): GetStreamUriResponse → MediaUri → Uri,
// ProbeMatches structure with name/hardware scopes, capabilities with PTZ
// absent, and the write-auth/read-open ladder.

const (
	testAdvertiseIP = "192.0.2.10"
	testRTSPPort    = 18554
	testONVIFPort   = 18080
	testPassword    = "testpass"
)

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()

	cfg := config.DefaultConfig()
	cfg.ONVIF.Port = testONVIFPort
	cfg.ONVIF.Username = "admin"
	cfg.ONVIF.Password = testPassword
	cfg.RTSP.Port = testRTSPPort
	cfg.Camera.Width = 1280
	cfg.Camera.Height = 720
	cfg.Camera.FPS = 25
	cfg.Camera.Bitrate = 2000000
	cfg.Device.Name = "TestCam"
	cfg.Device.Manufacturer = "MiBee"
	cfg.Device.Model = "Eye"
	cfg.Device.Firmware = "9.9.9"
	cfg.Device.SerialNumber = "SN-42"
	cfg.Device.HardwareID = "IMX219"

	pm := camera.NewParamManager(newMockCamera())
	srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true))
	if err != nil {
		t.Fatalf("onvifgo.New: %v", err)
	}

	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts
}

// soapRequest builds a SOAP 1.2 POST body for the given action.
func soapRequest(action, inner string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<t:%s xmlns:t="http://www.onvif.org/ver10/media/wsdl">%s</t:%s>
</s:Body>
</s:Envelope>`, action, inner, action)
}

// postSOAP posts a SOAP envelope and returns status + body.
func postSOAP(t *testing.T, ts *httptest.Server, path, envelope string) (int, string) {
	t.Helper()

	resp, err := ts.Client().Post(ts.URL+path, "application/soap+xml", strings.NewReader(envelope))
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	return resp.StatusCode, string(body)
}

// passwordDigest computes the WS-UsernameToken PasswordDigest.
func passwordDigest(nonceB64, created, password string) string {
	nonce, _ := base64.StdEncoding.DecodeString(nonceB64)
	sum := sha1.Sum(append(append(nonce, []byte(created)...), []byte(password)...))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// withAuth wraps a SOAP envelope with a WS-Security UsernameToken using the
// given password type ("digest" or "text").
func withAuth(envelope, passwordType string) string {
	nonce := "dGVzdA=="
	created := "2024-01-01T00:00:00.000Z"

	var cred, createdElem string
	createdElem = fmt.Sprintf(
		`<wsu:Created xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-utility-1.0.xsd">%s</wsu:Created>`,
		created)
	switch passwordType {
	case "digest":
		cred = fmt.Sprintf(
			`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>`,
			passwordDigest(nonce, created, testPassword))
	case "bad-digest":
		cred = fmt.Sprintf(
			`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>`,
			passwordDigest(nonce, created, "wrongpass"))
	case "digest-bad-ns":
		// #40: digest under the common misspelled utility namespace —
		// accepted since upstream v2.0.0-rc3 (CreatedVariant).
		cred = fmt.Sprintf(
			`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">%s</wsse:Password>`,
			passwordDigest(nonce, created, testPassword))
		createdElem = fmt.Sprintf(
			`<wsu:Created xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">%s</wsu:Created>`,
			created)
	default:
		cred = fmt.Sprintf(
			`<wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordText">%s</wsse:Password>`,
			testPassword)
	}

	header := fmt.Sprintf(`<s:Header>`+
		`<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd" s:mustUnderstand="1">`+
		`<wsse:UsernameToken>`+
		`<wsse:Username>admin</wsse:Username>`+
		`%s`+
		`<wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">%s</wsse:Nonce>`+
		`%s`+
		`</wsse:UsernameToken>`+
		`</wsse:Security>`+
		`</s:Header>`, cred, nonce, createdElem)

	return strings.Replace(envelope, "<s:Body>", header+"\n<s:Body>", 1)
}

func TestGetStreamUriContract(t *testing.T) {
	ts := newTestServer(t)

	req := soapRequest("GetStreamUri", "<ProfileToken>main</ProfileToken>")
	status, body := postSOAP(t, ts, "/onvif/device_service", req)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	// The NVR's raw fallback extracts MediaUri/Uri by local name.
	if !strings.Contains(body, "GetStreamUriResponse") {
		t.Errorf("response missing GetStreamUriResponse element:\n%s", body)
	}
	if !strings.Contains(body, "MediaUri") {
		t.Errorf("response missing MediaUri element:\n%s", body)
	}

	m := regexp.MustCompile(`<(?:[A-Za-z0-9]+:)?Uri>([^<]+)</(?:[A-Za-z0-9]+:)?Uri>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no Uri element in response:\n%s", body)
	}
	want := fmt.Sprintf("rtsp://%s:%d/stream", testAdvertiseIP, testRTSPPort)
	if m[1] != want {
		t.Errorf("Uri = %q, want %q", m[1], want)
	}
}

func TestGetCapabilitiesContract(t *testing.T) {
	ts := newTestServer(t)

	req := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"/>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/device_service", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	// NVR expects Device/Media/Imaging present with the camera's own IP as
	// host, and PTZ absent.
	for _, want := range []string{
		fmt.Sprintf("http://%s:%d/onvif/device_service", testAdvertiseIP, testONVIFPort),
		fmt.Sprintf("http://%s:%d/onvif/media_service", testAdvertiseIP, testONVIFPort),
		fmt.Sprintf("http://%s:%d/onvif/imaging_service", testAdvertiseIP, testONVIFPort),
	} {
		if !strings.Contains(body, want) {
			t.Errorf("capabilities missing advertised XAddr %q:\n%s", want, body)
		}
	}

	if regexp.MustCompile(`<(?:[A-Za-z0-9]+:)?PTZ[>:]`).MatchString(body) {
		t.Errorf("capabilities must not advertise PTZ:\n%s", body)
	}
}

func TestGetProfilesContract(t *testing.T) {
	ts := newTestServer(t)

	req := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/media_service", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	if !strings.Contains(body, `token="main"`) {
		t.Errorf("profile token main missing:\n%s", body)
	}
	if !strings.Contains(body, "<Encoding>H264</Encoding>") && !strings.Contains(body, ":Encoding>H264<") {
		t.Errorf("H264 encoding missing:\n%s", body)
	}
	if !strings.Contains(body, "<Width>1280</Width>") && !strings.Contains(body, ":Width>1280<") {
		t.Errorf("width 1280 missing:\n%s", body)
	}
	if !strings.Contains(body, "<Height>720</Height>") && !strings.Contains(body, ":Height>720<") {
		t.Errorf("height 720 missing:\n%s", body)
	}
}

func TestGetDeviceInformationContract(t *testing.T) {
	ts := newTestServer(t)

	req := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/device_service", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	for _, want := range []string{"MiBee", "Eye", "SN-42", "9.9.9"} {
		if !strings.Contains(body, want) {
			t.Errorf("device information missing %q:\n%s", want, body)
		}
	}
}

func TestGetScopesContract(t *testing.T) {
	ts := newTestServer(t)

	req := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetScopes xmlns="http://www.onvif.org/ver10/device/wsdl"/>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/device_service", req)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	for _, want := range []string{
		"onvif://www.onvif.org/type/video_encoder",
		"onvif://www.onvif.org/name/TestCam",
		"onvif://www.onvif.org/hardware/IMX219",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("scopes missing %q:\n%s", want, body)
		}
	}
}

func TestGetSnapshotUriContract(t *testing.T) {
	ts := newTestServer(t)

	req := soapRequest("GetSnapshotUri", "<ProfileToken>main</ProfileToken>")
	status, body := postSOAP(t, ts, "/onvif/media_service", req)

	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	want := fmt.Sprintf("http://%s:%d/snapshot", testAdvertiseIP, testONVIFPort)
	m := regexp.MustCompile(`<(?:[A-Za-z0-9]+:)?Uri>([^<]+)</(?:[A-Za-z0-9]+:)?Uri>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no Uri element in response:\n%s", body)
	}
	if m[1] != want {
		t.Errorf("snapshot Uri = %q, want %q", m[1], want)
	}
}

func TestAuthLadder(t *testing.T) {
	ts := newTestServer(t)

	readNoCreds := soapRequest("GetProfiles", "")
	setBody := soapRequest("SetImagingSettings", `<VideoSourceToken>videoSrc0</VideoSourceToken>
<ImagingSettings xmlns="http://www.onvif.org/ver20/imaging/wsdl">
<Brightness>0.1</Brightness>
</ImagingSettings>`)

	// SetImagingSettings requests must use the imaging namespace on the
	// request root for the library's decoder.
	setBody = `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<SetImagingSettings xmlns="http://www.onvif.org/ver20/imaging/wsdl">
<VideoSourceToken>videoSrc0</VideoSourceToken>
<ImagingSettings><Brightness>0.1</Brightness></ImagingSettings>
</SetImagingSettings>
</s:Body>
</s:Envelope>`

	t.Run("read without credentials is open", func(t *testing.T) {
		status, _ := postSOAP(t, ts, "/onvif/device_service", readNoCreds)
		if status != http.StatusOK {
			t.Fatalf("read without creds status = %d, want 200", status)
		}
	})

	t.Run("write without credentials is rejected", func(t *testing.T) {
		status, body := postSOAP(t, ts, "/onvif/imaging_service", setBody)
		if status == http.StatusOK {
			t.Fatalf("write without creds unexpectedly succeeded; body: %s", body)
		}
		if !strings.Contains(body, "Fault") {
			t.Errorf("expected SOAP fault, got: %s", body)
		}
	})

	t.Run("write with valid digest succeeds", func(t *testing.T) {
		status, body := postSOAP(t, ts, "/onvif/imaging_service", withAuth(setBody, "digest"))
		if status != http.StatusOK {
			t.Fatalf("digest status = %d, want 200; body: %s", status, body)
		}
	})

	t.Run("write with valid password text succeeds", func(t *testing.T) {
		status, body := postSOAP(t, ts, "/onvif/imaging_service", withAuth(setBody, "text"))
		if status != http.StatusOK {
			t.Fatalf("text status = %d, want 200; body: %s", status, body)
		}
	})

	t.Run("write with wrong password is rejected", func(t *testing.T) {
		status, body := postSOAP(t, ts, "/onvif/imaging_service", withAuth(setBody, "bad-digest"))
		if status == http.StatusOK {
			t.Fatalf("bad digest unexpectedly succeeded; body: %s", body)
		}
		if !strings.Contains(body, "Fault") {
			t.Errorf("expected SOAP fault, got: %s", body)
		}
	})

	t.Run("write with misspelled Created namespace is accepted (#40)", func(t *testing.T) {
		status, body := postSOAP(t, ts, "/onvif/imaging_service", withAuth(setBody, "digest-bad-ns"))
		if status != http.StatusOK {
			t.Fatalf("bad-ns digest status = %d, want 200; body: %s", status, body)
		}
	})
}

func TestImagingRoundTrip(t *testing.T) {
	ts := newTestServer(t)

	getReq := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetImagingSettings xmlns="http://www.onvif.org/ver20/imaging/wsdl">
<VideoSourceToken>videoSrc0</VideoSourceToken>
</GetImagingSettings>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/imaging_service", getReq)
	if status != http.StatusOK {
		t.Fatalf("get status = %d; body: %s", status, body)
	}

	var resp struct {
		Body struct {
			GetImagingSettingsResponse struct {
				ImagingSettings struct {
					Brightness float64 `xml:"Brightness"`
					Contrast   float64 `xml:"Contrast"`
					Exposure   struct {
						Mode string `xml:"Mode"`
					} `xml:"Exposure"`
					WhiteBalance struct {
						Mode string `xml:"Mode"`
					} `xml:"WhiteBalance"`
				} `xml:"ImagingSettings"`
			} `xml:"GetImagingSettingsResponse"`
		} `xml:"Body"`
	}
	if err := xml.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("decode GetImagingSettings response: %v\n%s", err, body)
	}

	settings := resp.Body.GetImagingSettingsResponse.ImagingSettings
	if settings.Brightness != 0 {
		t.Errorf("brightness = %v, want 0 (mock default)", settings.Brightness)
	}
	if settings.Contrast != 1 {
		t.Errorf("contrast = %v, want 1 (mock default)", settings.Contrast)
	}
	if settings.Exposure.Mode != "AUTO" {
		t.Errorf("exposure mode = %q, want AUTO; body: %s", settings.Exposure.Mode, body)
	}
	if settings.WhiteBalance.Mode != "AUTO" {
		t.Errorf("white balance mode = %q, want AUTO", settings.WhiteBalance.Mode)
	}
}

func TestSetImagingSettingsOutOfRangeRejected(t *testing.T) {
	ts := newTestServer(t)

	setBody := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<SetImagingSettings xmlns="http://www.onvif.org/ver20/imaging/wsdl">
<VideoSourceToken>videoSrc0</VideoSourceToken>
<ImagingSettings><Brightness>99</Brightness></ImagingSettings>
</SetImagingSettings>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/imaging_service", withAuth(setBody, "digest"))
	if status == http.StatusOK {
		t.Fatalf("out-of-range brightness unexpectedly succeeded; body: %s", body)
	}
	if !strings.Contains(body, "Fault") {
		t.Errorf("expected SOAP fault, got: %s", body)
	}
}

func TestProbeOverHTTPContract(t *testing.T) {
	ts := newTestServer(t)

	probe := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing">
<s:Header>
<a:Action s:mustUnderstand="1">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action>
<a:MessageID>urn:uuid:e722c59c-28d1-4c2a-abcd-12ab34cd56ef</a:MessageID>
</s:Header>
<s:Body>
<Probe xmlns="http://schemas.xmlsoap.org/ws/2005/04/discovery"/>
</s:Body>
</s:Envelope>`

	// Directed probes land on the device service endpoint, like the
	// historical server intercepted them.
	status, body := postSOAP(t, ts, "/onvif/device_service", probe)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", status, body)
	}

	for _, want := range []string{
		"ProbeMatches",
		"ProbeMatch",
		"EndpointReference",
		fmt.Sprintf("http://%s:%d/onvif/device_service", testAdvertiseIP, testONVIFPort),
		"onvif://www.onvif.org/name/TestCam",
		"onvif://www.onvif.org/hardware/IMX219",
		"NetworkVideoTransmitter",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("ProbeMatches missing %q:\n%s", want, body)
		}
	}

	// The XAddr host must be the camera's own address — never the probing
	// client's — or the NVR enrolls itself.
	if strings.Contains(body, "127.0.0.1") {
		t.Errorf("ProbeMatches echoes the client IP (127.0.0.1) — must advertise the device IP:\n%s", body)
	}
}

func TestUnknownActionFault(t *testing.T) {
	ts := newTestServer(t)

	req := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<NoSuchAction xmlns="http://www.onvif.org/ver10/device/wsdl"/>
</s:Body>
</s:Envelope>`

	status, body := postSOAP(t, ts, "/onvif/device_service", req)
	if status == http.StatusOK {
		t.Fatalf("unknown action unexpectedly succeeded; body: %s", body)
	}
	if !strings.Contains(body, "Fault") {
		t.Errorf("expected SOAP fault, got: %s", body)
	}
}

func TestSnapshotEndpoint(t *testing.T) {
	ts := newTestServer(t)

	resp, err := ts.Client().Get(ts.URL + "/snapshot")
	if err != nil {
		t.Fatalf("GET /snapshot: %v", err)
	}
	defer resp.Body.Close()

	// Without a camera frame the dual-tier buffer reports unavailability.
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (no frame available)", resp.StatusCode)
	}
}

// mockImagingCamera is a test double recording param reads/writes.
type mockImagingCamera struct {
	mu     sync.Mutex
	params map[string]interface{}
}

func newMockCamera() *mockImagingCamera {
	return &mockImagingCamera{
		params: map[string]interface{}{
			"brightness":   float64(0.0),
			"contrast":     float64(1.0),
			"saturation":   float64(1.0),
			"sharpness":    float64(1.0),
			"shutter":      float64(0),
			"gain":         float64(1.0),
			"exposureMode": "normal",
			"awbMode":      "auto",
		},
	}
}

func (m *mockImagingCamera) Start(_ context.Context) error { return nil }
func (m *mockImagingCamera) Stop() error                   { return nil }
func (m *mockImagingCamera) Frames() <-chan camera.Frame   { return nil }

func (m *mockImagingCamera) SetParam(name string, value interface{}) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.params[name] = value
	return nil
}

func (m *mockImagingCamera) GetParam(name string) (interface{}, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.params[name]
	if !ok {
		return 0, nil
	}
	return v, nil
}

func (m *mockImagingCamera) Info() camera.CameraInfo {
	return camera.CameraInfo{}
}
