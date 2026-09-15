package gbposition

import "testing"

func TestStaticPositionReportsConfiguredCoordinates(t *testing.T) {
	p := &StaticPosition{Longitude: "1163942.55E", Latitude: "395436.30N"}
	r := p.CurrentPosition()
	if r == nil {
		t.Fatal("always reports")
	}
	if r.Longitude != "1163942.55E" || r.Latitude != "395436.30N" {
		t.Fatalf("coordinates = %+v", r)
	}
	if len(r.Time) != 19 || r.Time[10] != 'T' {
		t.Fatalf("GB time format: %q", r.Time)
	}
	if r.Speed != "" {
		t.Fatalf("speed = %q, want empty (fixed camera)", r.Speed)
	}
}
