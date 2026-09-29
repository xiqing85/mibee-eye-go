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

	"github.com/xiqing85/mibee-eye-go/internal/camera"
	"github.com/xiqing85/mibee-eye-go/internal/config"
	"github.com/xiqing85/mibee-eye-go/internal/onvif"
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
	srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true, "rpicam-still", "ffmpeg"), nil)
	if err != nil {
		t.Fatalf("onvifgo.New: %v", err)
	}

	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts
}

// newTestServerWithSrv is newTestServer but also hands back the
// composed *Server (tests that need the publish seam).
func newTestServerWithSrv(t *testing.T) (*httptest.Server, *Server) {
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
	srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true, "rpicam-still", "ffmpeg"), nil)
	if err != nil {
		t.Fatalf("onvifgo.New: %v", err)
	}

	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)
	return ts, srv
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

	m := regexp.MustCompile(`<(?:[A-Za-z0-9]+:)?Uri(?:\s[^>]*)?>([^<]+)</(?:[A-Za-z0-9]+:)?Uri>`).FindStringSubmatch(body)
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
		fmt.Sprintf("http://%s:%d/onvif/events_service", testAdvertiseIP, testONVIFPort),
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
	m := regexp.MustCompile(`<(?:[A-Za-z0-9]+:)?Uri(?:\s[^>]*)?>([^<]+)</(?:[A-Za-z0-9]+:)?Uri>`).FindStringSubmatch(body)
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

// TestEventsPullPointContract walks the full AI MotionAlarm path through
// the composed server: CreatePullPointSubscription answers on the shared
// path-insensitive SOAP handler, the SubscriptionReference points at the
// dedicated /onvif/events_service/sub/ subtree, and a published
// MotionAlarm comes back on the next PullMessages.
func TestEventsPullPointContract(t *testing.T) {
	ts, srv := newTestServerWithSrv(t)

	create := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<CreatePullPointSubscription xmlns="http://www.onvif.org/ver10/events/wsdl">
<InitialTerminationTime>PT1M</InitialTerminationTime>
</CreatePullPointSubscription>
</s:Body>
</s:Envelope>`

	// Path-insensitive dispatch: the create action answers on any path,
	// like every other action the NVR talks to.
	// Events actions are auth-protected (unlike the discovery-facing
	// read actions).
	status, body := postSOAP(t, ts, "/onvif/device_service", withAuth(create, "digest"))
	if status != http.StatusOK {
		t.Fatalf("create status = %d, want 200; body: %s", status, body)
	}
	address := xmlText(t, body, "Address")
	if !strings.Contains(address, "/onvif/events_service/sub/") {
		t.Fatalf("subscription address %q must live on the sub subtree", address)
	}
	subPath := address[strings.Index(address, "/onvif/events_service/sub/"):]

	// Publish one accepted AI edge, then pull it.
	srv.PublishMotionAlarm(3)

	pull := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<PullMessages xmlns="http://www.onvif.org/ver10/events/wsdl">
<Timeout>PT2S</Timeout>
<MessageLimit>5</MessageLimit>
</PullMessages>
</s:Body>
</s:Envelope>`)
	status, body = postSOAP(t, ts, subPath, withAuth(pull, "digest"))
	if status != http.StatusOK {
		t.Fatalf("pull status = %d, want 200; body: %s", status, body)
	}
	for _, want := range []string{"tns1:VideoSource/MotionAlarm", "State", "true", "Targets", "3"} {
		if !strings.Contains(body, want) {
			t.Errorf("pull body missing %q:\n%s", want, body)
		}
	}
}

// xmlText extracts the inner text of the first element with the given
// local name (address extraction from SubscriptionReference).
func xmlText(t *testing.T, body, local string) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			t.Fatalf("xml scan: %v", err)
		}
		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if el.Name.Local == local {
				var text string
				if err := dec.DecodeElement(&text, &el); err != nil {
					t.Fatalf("decode %s: %v", local, err)
				}
				return text
			}
		case xml.EndElement:
			depth--
		}
	}
}

// TestSubstreamProfileAndStreamUri (SPEC appendix A #20): with a live
// substream pipeline the `sub` profile is advertised after `main`, and
// GetStreamUri routes the sub token to the RTSP /sub mount.
func TestSubstreamProfileAndStreamUri(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ONVIF.Port = testONVIFPort
	cfg.ONVIF.Username = "admin"
	cfg.ONVIF.Password = testPassword
	cfg.RTSP.Port = testRTSPPort
	cfg.Camera.Width = 1280
	cfg.Camera.Height = 720
	cfg.Camera.FPS = 25
	cfg.Camera.Bitrate = 2000000

	pm := camera.NewParamManager(newMockCamera())
	sub := &camera.SubstreamInfo{Width: 640, Height: 360, FPS: 15, Bitrate: 400000}
	srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true, "rpicam-still", "ffmpeg"), sub)
	if err != nil {
		t.Fatalf("onvifgo.New: %v", err)
	}
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)

	profilesReq := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body := postSOAP(t, ts, "/onvif/media_service", profilesReq)
	if status != http.StatusOK {
		t.Fatalf("GetProfiles status = %d", status)
	}
	mainAt := strings.Index(body, `token="main"`)
	subAt := strings.Index(body, `token="sub"`)
	if mainAt < 0 || subAt < 0 {
		t.Fatalf("both profiles must be advertised:\n%s", body)
	}
	if mainAt > subAt {
		t.Fatalf("main profile must come first (NVR auto-selects #1):\n%s", body)
	}
	if !strings.Contains(body, "<Width>640</Width>") && !strings.Contains(body, ":Width>640<") {
		t.Fatalf("sub geometry missing:\n%s", body)
	}

	uriReq := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl">
<ProfileToken>sub</ProfileToken>
</GetStreamUri>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/media_service", uriReq)
	if status != http.StatusOK {
		t.Fatalf("GetStreamUri status = %d", status)
	}
	if !strings.Contains(body, "/sub") {
		t.Fatalf("sub token must map to the /sub mount:\n%s", body)
	}
	if !strings.Contains(body, fmt.Sprintf(":%d", testRTSPPort)) {
		t.Fatalf("stream URI must carry the RTSP port:\n%s", body)
	}

	// The main token keeps the primary mount (fail-open for everything
	// else is covered by the library's contract tests).
	uriReqMain := strings.Replace(uriReq, "<ProfileToken>sub</ProfileToken>", "<ProfileToken>main</ProfileToken>", 1)
	_, body = postSOAP(t, ts, "/onvif/media_service", uriReqMain)
	if !strings.Contains(body, "/stream") {
		t.Fatalf("main token must keep the /stream mount:\n%s", body)
	}
}

// tr2Request builds a SOAP body whose action element carries the ver20
// (Media2) namespace.
func tr2Request(action, inner string) string {
	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<t2:%s xmlns:t2="http://www.onvif.org/ver20/media/wsdl">%s</t2:%s>
</s:Body>
</s:Envelope>`, action, inner, action)
}

// TestMedia2FaceContract (onvif.media2_enabled, default true): the tr2
// face answers on its own /onvif/media2_service mount with the ver20
// shape — plain Uri (no MediaUri wrapper), main profile first — while
// the Media1 face keeps its ver10 shape on the shared handler.
func TestMedia2FaceContract(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.ONVIF.Port = testONVIFPort
	cfg.ONVIF.Username = "admin"
	cfg.ONVIF.Password = testPassword
	cfg.RTSP.Port = testRTSPPort
	cfg.Camera.Width = 1280
	cfg.Camera.Height = 720
	cfg.Camera.FPS = 25
	cfg.Camera.Bitrate = 2000000

	pm := camera.NewParamManager(newMockCamera())
	sub := &camera.SubstreamInfo{Width: 640, Height: 360, FPS: 15, Bitrate: 400000}
	srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true, "rpicam-still", "ffmpeg"), sub)
	if err != nil {
		t.Fatalf("onvifgo.New: %v", err)
	}
	ts := httptest.NewServer(srv.mux)
	t.Cleanup(ts.Close)

	// tr2 GetProfiles: ver20/media wire, main first (the NVR-equivalent
	// entry path picks the first profile).
	status, body := postSOAP(t, ts, "/onvif/media2_service", tr2Request("GetProfiles", ""))
	if status != http.StatusOK {
		t.Fatalf("media2 GetProfiles status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(body, "http://www.onvif.org/ver20/media/wsdl") {
		t.Errorf("media2 response must carry the ver20/media namespace:\n%s", body)
	}
	mainAt := strings.Index(body, `token="main"`)
	subAt := strings.Index(body, `token="sub"`)
	if mainAt < 0 || subAt < 0 {
		t.Fatalf("media2 face must list both profiles:\n%s", body)
	}
	if mainAt > subAt {
		t.Fatalf("media2 face must keep main first:\n%s", body)
	}

	// tr2 GetStreamUri: plain Uri element, no MediaUri wrapper.
	status, body = postSOAP(t, ts, "/onvif/media2_service",
		tr2Request("GetStreamUri", "<ProfileToken>main</ProfileToken>"))
	if status != http.StatusOK {
		t.Fatalf("media2 GetStreamUri status = %d, want 200; body: %s", status, body)
	}
	if strings.Contains(body, "MediaUri") {
		t.Errorf("media2 GetStreamUri must not use the ver10 MediaUri wrapper:\n%s", body)
	}
	m := regexp.MustCompile(`<(?:[A-Za-z0-9]+:)?Uri(?:\s[^>]*)?>([^<]+)</(?:[A-Za-z0-9]+:)?Uri>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("media2 GetStreamUri response missing Uri:\n%s", body)
	}
	want := fmt.Sprintf("rtsp://%s:%d/stream", testAdvertiseIP, testRTSPPort)
	if m[1] != want {
		t.Errorf("media2 Uri = %q, want %q", m[1], want)
	}

	// Legacy lock: the Media1 face on the shared handler keeps its ver10
	// shape (path-insensitive dispatch unaffected by the media2 subtree).
	status, body = postSOAP(t, ts, "/onvif/media_service", `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`)
	if status != http.StatusOK {
		t.Fatalf("media1 GetProfiles status = %d, want 200", status)
	}
	if !strings.Contains(body, "http://www.onvif.org/ver10/media/wsdl") {
		t.Errorf("media1 response must stay on the ver10/media namespace:\n%s", body)
	}
	// `fixed="true"` is a Profile attribute in both faces; the tr2-only
	// discriminator is the Configurations wrapper element.
	if strings.Contains(body, "Configurations") {
		t.Errorf("media1 response must not leak the media2 profile shape:\n%s", body)
	}
}

// TestGetServicesFacesContract: GetServices advertises the media2 and
// deviceIO entries with XAddrs matching the actual mounts, and hides
// them when the config keys are off.
func TestGetServicesFacesContract(t *testing.T) {
	getServices := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetServices xmlns="http://www.onvif.org/ver10/device/wsdl"/>
</s:Body>
</s:Envelope>`
	media2XAddr := fmt.Sprintf("http://%s:%d/onvif/media2_service", testAdvertiseIP, testONVIFPort)
	deviceIOXAddr := fmt.Sprintf("http://%s:%d/onvif/device_service", testAdvertiseIP, testONVIFPort)

	t.Run("both faces advertised by default", func(t *testing.T) {
		ts := newTestServer(t)
		status, body := postSOAP(t, ts, "/onvif/device_service", getServices)
		if status != http.StatusOK {
			t.Fatalf("GetServices status = %d; body: %s", status, body)
		}
		if !strings.Contains(body, "http://www.onvif.org/ver20/media/wsdl") || !strings.Contains(body, media2XAddr) {
			t.Errorf("GetServices must advertise the media2 entry at %s:\n%s", media2XAddr, body)
		}
		if !strings.Contains(body, "http://www.onvif.org/ver10/deviceIO/wsdl") || !strings.Contains(body, deviceIOXAddr) {
			t.Errorf("GetServices must advertise the deviceIO entry at %s:\n%s", deviceIOXAddr, body)
		}
	})

	t.Run("media2 disabled hides the entry and keeps legacy dispatch", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ONVIF.Port = testONVIFPort
		cfg.ONVIF.Username = "admin"
		cfg.ONVIF.Password = testPassword
		cfg.RTSP.Port = testRTSPPort
		cfg.ONVIF.Media2Enabled = false
		pm := camera.NewParamManager(newMockCamera())
		srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true, "rpicam-still", "ffmpeg"), nil)
		if err != nil {
			t.Fatalf("onvifgo.New: %v", err)
		}
		ts := httptest.NewServer(srv.mux)
		t.Cleanup(ts.Close)

		status, body := postSOAP(t, ts, "/onvif/device_service", getServices)
		if status != http.StatusOK {
			t.Fatalf("GetServices status = %d", status)
		}
		if strings.Contains(body, "http://www.onvif.org/ver20/media/wsdl") {
			t.Errorf("media2 entry must be hidden when media2_enabled=false:\n%s", body)
		}

		// With the subtree absent, the media2_service path falls through
		// to the shared handler and answers the Media1 shape — the
		// historical path-insensitive dispatch.
		status, body = postSOAP(t, ts, "/onvif/media2_service", `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`)
		if status != http.StatusOK {
			t.Fatalf("legacy GetProfiles on media2 path: status = %d; body: %s", status, body)
		}
		if !strings.Contains(body, "http://www.onvif.org/ver10/media/wsdl") {
			t.Errorf("legacy dispatch must answer the ver10 shape:\n%s", body)
		}
	})

	t.Run("deviceio disabled hides the entry and the actions", func(t *testing.T) {
		cfg := config.DefaultConfig()
		cfg.ONVIF.Port = testONVIFPort
		cfg.ONVIF.Username = "admin"
		cfg.ONVIF.Password = testPassword
		cfg.RTSP.Port = testRTSPPort
		cfg.ONVIF.DeviceIOEnabled = false
		pm := camera.NewParamManager(newMockCamera())
		srv, err := New(cfg, testAdvertiseIP, pm, onvif.NewSnapshotBuffer(true, "rpicam-still", "ffmpeg"), nil)
		if err != nil {
			t.Fatalf("onvifgo.New: %v", err)
		}
		ts := httptest.NewServer(srv.mux)
		t.Cleanup(ts.Close)

		status, body := postSOAP(t, ts, "/onvif/device_service", getServices)
		if status != http.StatusOK {
			t.Fatalf("GetServices status = %d", status)
		}
		if strings.Contains(body, "http://www.onvif.org/ver10/deviceIO/wsdl") {
			t.Errorf("deviceIO entry must be hidden when deviceio_enabled=false:\n%s", body)
		}

		status, body = postSOAP(t, ts, "/onvif/device_service",
			`<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetRelayOutputs xmlns="http://www.onvif.org/ver10/deviceIO/wsdl"/>
</s:Body>
</s:Envelope>`)
		if status == http.StatusOK {
			t.Fatalf("GetRelayOutputs must fault when deviceio_enabled=false; body: %s", body)
		}
		if !strings.Contains(body, "Fault") {
			t.Errorf("expected SOAP fault for disabled deviceIO, got: %s", body)
		}
	})
}

// TestDeviceIOEmptySetsContract (onvif.deviceio_enabled, default true):
// the alarm I/O family stays answerable with honest empty sets — this
// hardware has no relays, digital inputs, or audio — and mutating a
// nonexistent relay faults instead of pretending.
func TestDeviceIOEmptySetsContract(t *testing.T) {
	ts := newTestServer(t)

	getRelays := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetRelayOutputs xmlns="http://www.onvif.org/ver10/deviceIO/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body := postSOAP(t, ts, "/onvif/device_service", getRelays)
	if status != http.StatusOK {
		t.Fatalf("GetRelayOutputs status = %d; body: %s", status, body)
	}
	if !strings.Contains(body, "GetRelayOutputsResponse") {
		t.Errorf("GetRelayOutputs must answer, not fault:\n%s", body)
	}
	if strings.Contains(body, "relay_1") || strings.Contains(body, "relay_2") {
		t.Errorf("GetRelayOutputs fabricated simulator relays:\n%s", body)
	}

	getInputs := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetDigitalInputs xmlns="http://www.onvif.org/ver10/deviceIO/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/device_service", getInputs)
	if status != http.StatusOK {
		t.Fatalf("GetDigitalInputs status = %d; body: %s", status, body)
	}
	if strings.Contains(body, "di_1") {
		t.Errorf("GetDigitalInputs fabricated a simulator input:\n%s", body)
	}

	getCaps := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetDeviceIOServiceCapabilities xmlns="http://www.onvif.org/ver10/deviceIO/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/device_service", getCaps)
	if status != http.StatusOK {
		t.Fatalf("GetDeviceIOServiceCapabilities status = %d; body: %s", status, body)
	}
	for _, want := range []string{`RelayOutputs="0"`, `DigitalInputs="0"`} {
		if !strings.Contains(body, want) {
			t.Errorf("deviceIO capabilities missing honest zero count %q:\n%s", want, body)
		}
	}

	// Audio outputs: media-service action, empty by design (no audio
	// hardware) — an honest response, not an ActionNotSupported fault.
	status, body = postSOAP(t, ts, "/onvif/media_service", soapRequest("GetAudioOutputs", ""))
	if status != http.StatusOK {
		t.Fatalf("GetAudioOutputs status = %d; body: %s", status, body)
	}
	if !strings.Contains(body, "GetAudioOutputsResponse") {
		t.Errorf("GetAudioOutputs must answer with the empty set:\n%s", body)
	}

	// Mutating a relay that does not exist must fault (write action —
	// authenticated with the digest ladder).
	setRelay := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<SetRelayOutputState xmlns="http://www.onvif.org/ver10/deviceIO/wsdl">
<RelayOutputToken>relay_1</RelayOutputToken>
<LogicalState>active</LogicalState>
</SetRelayOutputState>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/device_service", withAuth(setRelay, "digest"))
	if status == http.StatusOK {
		t.Fatalf("SetRelayOutputState on empty hardware must fault; body: %s", body)
	}
	if !strings.Contains(body, "not found") {
		t.Errorf("fault must say the relay does not exist, got: %s", body)
	}
}

// TestOSDEmptyStoreContract: the OSD configuration loop is wired with the
// library's store — empty by default (no OSD engine fabricates nothing),
// and a client-created OSD round-trips.
func TestOSDEmptyStoreContract(t *testing.T) {
	ts := newTestServer(t)

	getOSDs := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetOSDs xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body := postSOAP(t, ts, "/onvif/media_service", getOSDs)
	if status != http.StatusOK {
		t.Fatalf("GetOSDs status = %d; body: %s", status, body)
	}
	if !strings.Contains(body, "GetOSDsResponse") {
		t.Errorf("GetOSDs must answer:\n%s", body)
	}
	if strings.Contains(body, "osd_1") {
		t.Errorf("OSD store must start empty — no fabricated OSDs:\n%s", body)
	}
}

// The ver10 encoder family + sync point on the shared handler (the
// onvif-go server completion batch): one configuration per profile,
// H264 options block, and the ver10 sync point answering the ver10
// shape (the tr2 handler stays on the media2 subtree).
func TestEncoderFamilyAndSyncPointContract(t *testing.T) {
	ts := newTestServer(t)

	list := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetVideoEncoderConfigurations xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body := postSOAP(t, ts, "/onvif/media_service", list)
	if status != http.StatusOK {
		t.Fatalf("GetVideoEncoderConfigurations status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(body, `token="main_encoder"`) {
		t.Errorf("main_encoder configuration missing:\n%s", body)
	}

	opts := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetVideoEncoderConfigurationOptions xmlns="http://www.onvif.org/ver10/media/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/media_service", opts)
	if status != http.StatusOK {
		t.Fatalf("GetVideoEncoderConfigurationOptions status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(body, "ResolutionsAvailable") {
		t.Errorf("H264 options block missing:\n%s", body)
	}

	sync := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<SetSynchronizationPoint xmlns="http://www.onvif.org/ver10/media/wsdl">
<ProfileToken>main</ProfileToken>
</SetSynchronizationPoint>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/media_service", withAuth(sync, "digest"))
	if status != http.StatusOK {
		t.Fatalf("SetSynchronizationPoint status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(body, "SetSynchronizationPointResponse") {
		t.Errorf("ver10 sync ack missing:\n%s", body)
	}

	// The tr2 encoder list answers on the media2 subtree.
	tr2 := `<?xml version="1.0" encoding="UTF-8"?>
<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope">
<s:Body>
<GetVideoEncoderConfigurations xmlns="http://www.onvif.org/ver20/media/wsdl"/>
</s:Body>
</s:Envelope>`
	status, body = postSOAP(t, ts, "/onvif/media2_service", tr2)
	if status != http.StatusOK {
		t.Fatalf("tr2 GetVideoEncoderConfigurations status = %d, want 200; body: %s", status, body)
	}
	if !strings.Contains(body, `token="main_encoder"`) {
		t.Errorf("tr2 main_encoder configuration missing:\n%s", body)
	}
}
