package thermal

import (
	_ "embed"
	"math"
	"testing"
)

//go:embed testdata/mlx-ee-0x1901.bin
var benchFixture []byte

// datasheetEE rebuilds the worked-example EEPROM (Table 12) plus the
// pixel (12,16) word (offset=2, α=10, Kt4=0 from the §11.2 chain).
func datasheetEE() []uint16 {
	ee := make([]uint16, 832)
	set := func(addr, v uint16) { ee[addr-0x2400] = v }
	set(0x2410, 0x4210)
	set(0x2411, 0xFFBB)
	set(0x2412, 0x0202)
	set(0x2413, 0xF202)
	set(0x2414, 0xF2F2)
	set(0x2415, 0xE2E2)
	set(0x2416, 0xD1E1)
	set(0x2417, 0xB1D1)
	set(0x2418, 0xF10F)
	set(0x2419, 0xF00F)
	set(0x241A, 0xE0EF)
	set(0x241B, 0xE0EF)
	set(0x241C, 0xE1E1)
	set(0x241D, 0xF3F2)
	set(0x241E, 0xF404)
	set(0x241F, 0xE504)
	set(0x2420, 0x79A6)
	set(0x2421, 0x2F44)
	set(0x2422, 0xFFDD)
	set(0x2423, 0x2210)
	set(0x2424, 0x3333)
	set(0x2425, 0x2233)
	set(0x2426, 0xEF01)
	set(0x2427, 0x9ACC)
	set(0x2428, 0xEEDC)
	set(0x2429, 0x10FF)
	set(0x242A, 0x2221)
	set(0x242B, 0x3333)
	set(0x242C, 0x2333)
	set(0x242D, 0x0112)
	set(0x242E, 0xEEFF)
	set(0x242F, 0xBBDD)
	set(0x2430, 0x18EF)
	set(0x2431, 0x2FF1)
	set(0x2432, 0x5952)
	set(0x2433, 0x9D68)
	set(0x2434, 0x5454)
	set(0x2435, 0x0994)
	set(0x2436, 0x6956)
	set(0x2437, 0x5354)
	set(0x2438, 0x2363)
	set(0x2439, 0xE446)
	set(0x243A, 0xFBB5)
	set(0x243B, 0x044B)
	set(0x243C, 0xF020)
	set(0x243D, 0x9797)
	set(0x243E, 0x9797)
	set(0x243F, 0x2889)
	// pixel (12,16): pn = 368, word @0x25AF = offset(2)<<10 | α(10)<<4.
	set(0x2440+367, (2<<10)|(10<<4))
	return ee
}

func table11Aux() AuxReadings {
	return AuxReadings{
		Vbe: 19442, CpSp0: -54, CpSp1: -56, Gain: 6273,
		Ptat: 1711, Vdd: -13115,
		Control: 0x0901, // resolution bits [11:10] = 2
		Status:  0,
	}
}

func closeTo(a, b, tol float64) bool { return math.Abs(a-b) < tol }

func TestDecodeGlobalsMatchTable12(t *testing.T) {
	ee := EepromFromWords(datasheetEE())
	if ee.Kvdd != -3168.0 {
		t.Errorf("Kvdd = %v, want -3168", ee.Kvdd)
	}
	if ee.Vdd25 != -13056.0 {
		t.Errorf("Vdd25 = %v, want -13056", ee.Vdd25)
	}
	if !closeTo(ee.KvPtat, 0.005371094, 1e-9) {
		t.Errorf("KvPtat = %v", ee.KvPtat)
	}
	if ee.KtPtat != 42.25 {
		t.Errorf("KtPtat = %v", ee.KtPtat)
	}
	if ee.Ptat25 != 12273.0 {
		t.Errorf("Ptat25 = %v", ee.Ptat25)
	}
	if ee.AlphaPtat != 9.0 {
		t.Errorf("AlphaPtat = %v", ee.AlphaPtat)
	}
	if ee.OffsetAvg != -69.0 {
		t.Errorf("OffsetAvg = %v", ee.OffsetAvg)
	}
	if ee.OccRow[11] != -1.0 { // row 12
		t.Errorf("OccRow[11] = %v, want -1", ee.OccRow[11])
	}
	if ee.OccCol[15] != -2.0 { // column 16
		t.Errorf("OccCol[15] = %v, want -2", ee.OccCol[15])
	}
	if ee.AlphaRef != 12100.0 {
		t.Errorf("AlphaRef = %v", ee.AlphaRef)
	}
	if ee.AccRow[11] != 3.0 || ee.AccCol[15] != 3.0 {
		t.Errorf("Acc = %v/%v, want 3/3", ee.AccRow[11], ee.AccCol[15])
	}
	if ee.AlphaScale != 37 {
		t.Errorf("AlphaScale = %v, want 37", ee.AlphaScale)
	}
	if ee.Gain != 6383.0 {
		t.Errorf("Gain = %v", ee.Gain)
	}
	if !closeTo(ee.KsTa, -0.001953125, 1e-12) {
		t.Errorf("KsTa = %v", ee.KsTa)
	}
	if ee.Ct3 != 160.0 || ee.Ct4 != 320.0 {
		t.Errorf("Ct3/Ct4 = %v/%v, want 160/320", ee.Ct3, ee.Ct4)
	}
	if !closeTo(ee.KsTo[0], -0.00080108642578125, 1e-15) {
		t.Errorf("KsTo[0] = %v", ee.KsTo[0])
	}
	if !closeTo(ee.AlphaCpSp0, 4.0745362639427e-09, 1e-20) {
		t.Errorf("AlphaCpSp0 = %v", ee.AlphaCpSp0)
	}
	if !closeTo(ee.AlphaCpSp1, 3.85171006200835e-09, 1e-20) {
		t.Errorf("AlphaCpSp1 = %v", ee.AlphaCpSp1)
	}
	if ee.OffCpSp0 != -75.0 || ee.OffCpSp1 != -77.0 {
		t.Errorf("OffCp = %v/%v, want -75/-77", ee.OffCpSp0, ee.OffCpSp1)
	}
	if ee.Kvcp != 0.5 {
		t.Errorf("Kvcp = %v, want 0.5", ee.Kvcp)
	}
	if !closeTo(ee.Ktacp, 0.00457763671875, 1e-14) {
		t.Errorf("Ktacp = %v", ee.Ktacp)
	}
	if ee.Tgc != 1.0 {
		t.Errorf("Tgc = %v, want 1", ee.Tgc)
	}
	if ee.ResolutionE != 2 {
		t.Errorf("ResolutionE = %v, want 2", ee.ResolutionE)
	}
}

func TestPerPixelParamsMatchWorkedExample(t *testing.T) {
	ee := EepromFromWords(datasheetEE())
	p := 11*32 + 15 // pixel (12,16)
	if ee.Offset[p] != 2.0 || ee.AlphaPixel[p] != 10.0 {
		t.Errorf("offset/alpha = %v/%v, want 2/10", ee.Offset[p], ee.AlphaPixel[p])
	}
	if !closeTo(ee.Kta[p], 0.005126953125, 1e-12) {
		t.Errorf("Kta = %v", ee.Kta[p])
	}
	if ee.Kv[p] != 0.5 {
		t.Errorf("Kv = %v, want 0.5", ee.Kv[p])
	}
	osRef := ee.OffsetAvg +
		ee.OccRow[11]*float64(uint64(1)<<ee.ScaleOccRow) +
		ee.OccCol[15]*float64(uint64(1)<<ee.ScaleOccCol) +
		ee.Offset[p]*float64(uint64(1)<<ee.ScaleOccRem)
	if osRef != -75.0 {
		t.Errorf("osRef = %v, want -75", osRef)
	}
	a := (ee.AlphaRef +
		ee.AccRow[11]*float64(uint64(1)<<ee.ScaleAccRow) +
		ee.AccCol[15]*float64(uint64(1)<<ee.ScaleAccCol) +
		ee.AlphaPixel[p]*float64(uint64(1)<<ee.ScaleAccRem)) /
		float64(uint64(1)<<ee.AlphaScale)
	if !closeTo(a, 1.26223312690854e-07, 1e-19) {
		t.Errorf("alpha(12,16) = %v", a)
	}
}

func TestPatternChess(t *testing.T) {
	// Datasheet worked example: pn=368 → chess Pattern = 0.
	for _, tc := range []struct{ pn, want int }{{368, 0}, {367, 1}, {1, 0}} {
		if got := int(patternChess(tc.pn)); got != tc.want {
			t.Errorf("patternChess(%d) = %d, want %d", tc.pn, got, tc.want)
		}
	}
}

func TestCommonParamsMatchWorkedExample(t *testing.T) {
	ee := EepromFromWords(datasheetEE())
	aux := table11Aux()
	c := ee.ComputeCommon(&aux)
	if c.ResCorr != 1.0 {
		t.Errorf("ResCorr = %v, want 1", c.ResCorr)
	}
	if !closeTo(c.Vdd, 3.3186, 1e-3) {
		t.Errorf("Vdd = %v, want ≈3.3186", c.Vdd)
	}
	if !closeTo(c.Ta, 39.184, 5e-3) {
		t.Errorf("Ta = %v, want ≈39.184", c.Ta)
	}
	if !closeTo(c.KGain, 1.01753546947234, 1e-12) {
		t.Errorf("KGain = %v", c.KGain)
	}
}

func TestFullToChainMatchesWorkedExample(t *testing.T) {
	ee := EepromFromWords(datasheetEE())
	var frame [Pixels]int16
	frame[367] = 609 // pixel (12,16) = RAM[0x056F]
	aux := table11Aux()
	to, c := ee.ComputeTo(&frame, &aux, 1.0, 31.0)
	if !closeTo(c.Ta, 39.184, 5e-3) {
		t.Errorf("Ta = %v", c.Ta)
	}
	got := float64(to[367])
	// Formula-faithful result (the datasheet example prints 80.36 mixing
	// CP_SP1 into the IR path at Pattern=0; the formulas give ≈80.1).
	if got <= 79.8 || got >= 80.6 {
		t.Errorf("To(12,16) = %v, expected ≈80.1 (formula) / 80.36 (print)", got)
	}
	for _, v := range to {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("non-finite To in frame: %v", v)
		}
	}
}

func TestRealDeviceFixture(t *testing.T) {
	if len(benchFixture) != 1664 {
		t.Fatalf("fixture = %d bytes, want 1664", len(benchFixture))
	}
	words := make([]uint16, 832)
	for i := range words {
		words[i] = uint16(benchFixture[i*2])<<8 | uint16(benchFixture[i*2+1])
	}
	ee := EepromFromWords(words)
	aux := AuxReadings{
		Vbe: 18942, CpSp0: -51, CpSp1: -49, Gain: 5750,
		Ptat: 1599, Vdd: -12217, Control: 0x1901,
	}
	c := ee.ComputeCommon(&aux)
	if !closeTo(c.Vdd, 3.287, 0.01) {
		t.Errorf("bench Vdd = %v, want ≈3.287", c.Vdd)
	}
	if c.Ta <= 30.0 || c.Ta >= 38.0 {
		t.Errorf("bench Ta = %v, want 30..38 °C", c.Ta)
	}
	var frame [Pixels]int16
	for i := range frame {
		frame[i] = -70
	}
	to, _ := ee.ComputeTo(&frame, &aux, 1.0, 0)
	for _, v := range to {
		if math.IsNaN(float64(v)) || float64(v) <= -50.0 || float64(v) >= 120.0 {
			t.Fatalf("To out of plausible band: %v", v)
		}
	}
}

func TestExtendedRanges(t *testing.T) {
	ee := EepromFromWords(datasheetEE())
	if ee.classify(-10.0) != 1 || ee.classify(25.0) != 2 ||
		ee.classify(200.0) != 3 || ee.classify(400.0) != 4 {
		t.Error("classify boundaries broken")
	}
	if ee.alphaCorr(2) != 1.0 {
		t.Error("alphaCorr(2) must be 1")
	}
	if math.Abs(ee.alphaCorr(1)-1.0) < 1e-9 || math.Abs(ee.alphaCorr(3)-1.0) < 1e-9 {
		t.Error("range1/range3 corrections should move alpha")
	}
}
