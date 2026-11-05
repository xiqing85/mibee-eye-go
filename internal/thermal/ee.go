package thermal

import "math"

// EEPROM calibration decode + per-frame temperature math (Ta/To), a
// faithful Go port of the MLX90640 datasheet Rev 11 §11 chain validated
// against the datasheet worked example (Table 11/12 → To(12,16) ≈
// 80.1–80.4 °C) and the real bench fixture (Ta ≈ 34 °C).

// signed is n-bit two's-complement conversion.
func signed(raw int32, bits uint) int32 {
	if raw > (1<<(bits-1))-1 {
		return raw - (1 << bits)
	}
	return raw
}

func nibble(word uint16, shift uint) float64 {
	return float64(signed(int32((word>>shift)&0xF), 4))
}

// Eeprom holds the decoded calibration parameters (datasheet §11.1).
type Eeprom struct {
	// 11.1.1 VDD
	Kvdd  float64
	Vdd25 float64
	// 11.1.2 PTAT
	KvPtat    float64
	KtPtat    float64
	Ptat25    float64
	AlphaPtat float64
	// 11.1.3 offsets
	OffsetAvg   float64
	OccRow      [Height]float64
	OccCol      [Width]float64
	ScaleOccRow uint
	ScaleOccCol uint
	ScaleOccRem uint
	Offset      [Pixels]float64
	// 11.1.4 alpha
	AlphaRef    float64
	AccRow      [Height]float64
	AccCol      [Width]float64
	ScaleAccRow uint
	ScaleAccCol uint
	ScaleAccRem uint
	AlphaPixel  [Pixels]float64
	AlphaScale  uint
	// 11.1.5/11.1.6 Kv / Kta per pixel
	Kv  [Pixels]float64
	Kta [Pixels]float64
	// 11.1.7 gain
	Gain float64
	// 11.1.8 KsTa
	KsTa float64
	// 11.1.9 corner temperatures
	Ct3 float64
	Ct4 float64
	// 11.1.10 KsTo per range (1-based range1..range4)
	KsTo        [4]float64
	KsToScale   uint
	AlphaCpSp0  float64
	AlphaCpSp1  float64
	OffCpSp0    float64
	OffCpSp1    float64
	Kvcp        float64
	Ktacp       float64
	Tgc         float64
	ResolutionE uint
	DeadPixel   [Pixels]bool
}

// CommonParams are the per-frame common quantities (§11.2.2.1–4).
type CommonParams struct {
	ResCorr float64
	Vdd     float64
	Ta      float64
	KGain   float64
}

// EepromFromWords decodes the full 832-word block (0x2400–0x273F).
func EepromFromWords(ee []uint16) *Eeprom {
	if len(ee) != 832 {
		panic("thermal: EEPROM block must be 832 words")
	}
	e := func(addr uint16) uint16 { return ee[addr-0x2400] }

	// 11.1.1 VDD
	kvdd := float64(signed(int32((e(0x2433)&0xFF00)>>8), 8)) * 32.0
	vdd25 := (float64(int32(e(0x2433)&0x00FF))-256.0)*32.0 - 8192.0

	// 11.1.2 PTAT
	kvPtat := float64(signed(int32((e(0x2432)&0xFC00)>>10), 6)) / 4096.0
	ktPtat := float64(signed(int32(e(0x2432)&0x03FF), 10)) / 8.0
	ptat25 := float64(signed(int32(e(0x2431)), 16))
	alphaPtat := float64(e(0x2410)&0xF000>>12)/4.0 + 8.0

	// 11.1.3 offsets
	offsetAvg := float64(signed(int32(e(0x2411)), 16))
	scaleOccRow := uint(e(0x2410) & 0x0F00 >> 8)
	scaleOccCol := uint(e(0x2410) & 0x00F0 >> 4)
	scaleOccRem := uint(e(0x2410) & 0x000F)
	var occRow [Height]float64
	for i := 0; i < Height; i++ {
		occRow[i] = nibble(e(0x2412+uint16(i/4)), uint(4*(i%4)))
	}
	var occCol [Width]float64
	for j := 0; j < Width; j++ {
		occCol[j] = nibble(e(0x2418+uint16(j/4)), uint(4*(j%4)))
	}

	// 11.1.4 alpha tables
	alphaRef := float64(signed(int32(e(0x2421)), 16))
	scaleAccRow := uint(e(0x2420) & 0x0F00 >> 8)
	scaleAccCol := uint(e(0x2420) & 0x00F0 >> 4)
	scaleAccRem := uint(e(0x2420) & 0x000F)
	alphaScale := uint(e(0x2420)&0xF000>>12) + 30
	var accRow [Height]float64
	for i := 0; i < Height; i++ {
		accRow[i] = nibble(e(0x2422+uint16(i/4)), uint(4*(i%4)))
	}
	var accCol [Width]float64
	for j := 0; j < Width; j++ {
		accCol[j] = nibble(e(0x2428+uint16(j/4)), uint(4*(j%4)))
	}

	// 11.1.5 Kv scale + 11.1.6 Kta scales
	kvScale := uint(e(0x2438) & 0x0F00 >> 8)
	ktaScale1 := uint(e(0x2438)&0x00F0>>4) + 8
	ktaScale2 := uint(e(0x2438) & 0x000F)

	// Per-pixel tables.
	var offset, alphaPixel, kv, kta [Pixels]float64
	var dead [Pixels]bool
	kvNibbles := [4]int32{
		int32(e(0x2434) & 0xF000 >> 12),
		int32(e(0x2434) & 0x0F00 >> 8),
		int32(e(0x2434) & 0x00F0 >> 4),
		int32(e(0x2434) & 0x000F),
	}
	ktaBytes := [4]int32{
		int32(e(0x2436) & 0xFF00 >> 8),
		int32(e(0x2436) & 0x00FF),
		int32(e(0x2437) & 0xFF00 >> 8),
		int32(e(0x2437) & 0x00FF),
	}
	for p := 0; p < Pixels; p++ {
		row, col := p/Width, p%Width
		word := e(0x2440 + uint16(p))
		dead[p] = word == 0x0000 || word&0x0001 != 0
		offset[p] = float64(signed(int32(word>>10), 6))
		alphaPixel[p] = float64(signed(int32(word&0x03F0>>4), 6))
		ktaPix := float64(signed(int32(word&0x000E>>1), 3))

		// Kv/Kta nibble by (row odd/even, col odd/even) — datasheet
		// numbering is 1-based, parity comes from row+1/col+1.
		r1, c1 := (row+1)%2, (col+1)%2
		kvi := 3
		if r1 == 1 && c1 == 1 {
			kvi = 0
		} else if r1 == 0 && c1 == 1 {
			kvi = 1
		} else if r1 == 1 && c1 == 0 {
			kvi = 2
		}
		kv[p] = float64(signed(kvNibbles[kvi], 4)) / float64(uint64(1)<<kvScale)

		kti := 3
		if r1 == 1 && c1 == 1 {
			kti = 0
		} else if r1 == 0 && c1 == 1 {
			kti = 1
		} else if r1 == 1 && c1 == 0 {
			kti = 2
		}
		kta[p] = (float64(signed(ktaBytes[kti], 8)) +
			ktaPix*float64(uint64(1)<<ktaScale2)) / float64(uint64(1)<<ktaScale1)
	}

	// 11.1.7 GAIN, 11.1.8 KsTa
	gain := float64(signed(int32(e(0x2430)), 16))
	ksTa := float64(signed(int32((e(0x243C)&0xFF00)>>8), 8)) / 8192.0

	// 11.1.9 corner temperatures
	step := float64(e(0x243F)&0x3000>>12) * 10.0
	ct3 := float64(e(0x243F)&0x00F0>>4) * step
	ct4 := float64(e(0x243F)&0x0F00>>8)*step + ct3

	// 11.1.10 KsTo per range
	ksToScale := uint(e(0x243F)&0x000F) + 8
	den := float64(uint64(1) << ksToScale)
	ksTo := [4]float64{
		float64(signed(int32(e(0x243D)&0x00FF), 8)) / den,
		float64(signed(int32((e(0x243D)&0xFF00)>>8), 8)) / den,
		float64(signed(int32(e(0x243E)&0x00FF), 8)) / den,
		float64(signed(int32((e(0x243E)&0xFF00)>>8), 8)) / den,
	}

	// 11.1.12 CP alpha
	alphaScaleCp := uint(e(0x2420)&0xF000>>12) + 27
	alphaCpSp0 := float64(e(0x2439)&0x03FF) / float64(uint64(1)<<alphaScaleCp)
	cpRatio := float64(signed(int32((e(0x2439)&0xFC00)>>10), 6)) / 128.0
	alphaCpSp1 := alphaCpSp0 * (1.0 + cpRatio)

	// 11.1.13 CP offsets
	offCpSp0 := float64(signed(int32(e(0x243A)&0x03FF), 10))
	offCpDelta := float64(signed(int32((e(0x243A)&0xFC00)>>10), 6))
	offCpSp1 := offCpSp0 + offCpDelta

	// 11.1.14/15 CP Kv/Kta (same scales as pixels)
	kvcp := float64(signed(int32((e(0x243B)&0xFF00)>>8), 8)) / float64(uint64(1)<<kvScale)
	ktacp := float64(signed(int32(e(0x243B)&0x00FF), 8)) / float64(uint64(1)<<ktaScale1)

	// 11.1.16 TGC, 11.1.17 calibration resolution
	tgc := float64(signed(int32(e(0x243C)&0x00FF), 8)) / 32.0
	resolutionE := uint(e(0x2438) & 0x3000 >> 12)

	return &Eeprom{
		Kvdd: kvdd, Vdd25: vdd25,
		KvPtat: kvPtat, KtPtat: ktPtat, Ptat25: ptat25, AlphaPtat: alphaPtat,
		OffsetAvg: offsetAvg, OccRow: occRow, OccCol: occCol,
		ScaleOccRow: scaleOccRow, ScaleOccCol: scaleOccCol, ScaleOccRem: scaleOccRem,
		Offset:   offset,
		AlphaRef: alphaRef, AccRow: accRow, AccCol: accCol,
		ScaleAccRow: scaleAccRow, ScaleAccCol: scaleAccCol, ScaleAccRem: scaleAccRem,
		AlphaPixel: alphaPixel, AlphaScale: alphaScale,
		Kv: kv, Kta: kta,
		Gain: gain, KsTa: ksTa, Ct3: ct3, Ct4: ct4,
		KsTo: ksTo, KsToScale: ksToScale,
		AlphaCpSp0: alphaCpSp0, AlphaCpSp1: alphaCpSp1,
		OffCpSp0: offCpSp0, OffCpSp1: offCpSp1,
		Kvcp: kvcp, Ktacp: ktacp, Tgc: tgc,
		ResolutionE: resolutionE, DeadPixel: dead,
	}
}

// ComputeCommon: §11.2.2.1–4 — resolution correction, Vdd, Ta, K_gain.
func (ee *Eeprom) ComputeCommon(aux *AuxReadings) CommonParams {
	resolutionReg := uint(aux.Control&0x0C00) >> 10
	resCorr := math.Pow(2, float64(int(ee.ResolutionE)-int(resolutionReg)))

	vddRam := float64(aux.Vdd)
	vdd := (resCorr*vddRam-ee.Vdd25)/ee.Kvdd + 3.3
	dv := (vddRam - ee.Vdd25) / ee.Kvdd

	ptat := float64(aux.Ptat)
	vbe := float64(aux.Vbe)
	ptatArt := (ptat / (ptat*ee.AlphaPtat + vbe)) * 262144.0
	ta := ((ptatArt/(1.0+ee.KvPtat*dv))-ee.Ptat25)/ee.KtPtat + 25.0

	return CommonParams{
		ResCorr: resCorr,
		Vdd:     vdd,
		Ta:      ta,
		KGain:   ee.Gain / float64(aux.Gain),
	}
}

// patternChess is the chess-mode subpage selector for a 1-based pixel
// number (§11.2.2.7): 0 → CP_SP0/α_CP_SP0, 1 → CP_SP1/α_CP_SP1.
func patternChess(pn int) uint {
	p := int32(pn - 1)
	rowPar := p/32 - (p/32)/2*2
	colPar := p - (p/2)*2
	return uint(rowPar ^ colPar)
}

// ComputeTo: §11.2.2.5–9 full per-pixel To array (°C). tr<=0 means "use
// the datasheet fallback Ta-8". Returns the To grid + common params.
func (ee *Eeprom) ComputeTo(frame *[Pixels]int16, aux *AuxReadings, emissivity, trIn float64) ([Pixels]float32, CommonParams) {
	common := ee.ComputeCommon(aux)
	tr := trIn
	if tr == 0 {
		tr = common.Ta - 8.0
	}

	// CP path (§11.2.2.6) — same for every pixel this frame.
	taFac := 1.0 + ee.Ktacp*(common.Ta-25.0)
	vddFac := 1.0 + ee.Kvcp*(common.Vdd-3.3)
	cpSp0 := float64(aux.CpSp0)*common.KGain - ee.OffCpSp0*taFac*vddFac
	cpSp1 := float64(aux.CpSp1)*common.KGain - ee.OffCpSp1*taFac*vddFac

	taK4 := math.Pow(common.Ta+273.15, 4)
	trK4 := math.Pow(tr+273.15, 4)
	taR := trK4 - (trK4-taK4)/emissivity

	var out [Pixels]float32
	for p := 0; p < Pixels; p++ {
		pn := p + 1
		pattern := patternChess(pn)

		// 11.2.2.5 gain → offset/Vdd/Ta → emissivity
		pixGain := float64(frame[p]) * common.KGain
		osRef := ee.OffsetAvg +
			ee.OccRow[p/Width]*float64(uint64(1)<<ee.ScaleOccRow) +
			ee.OccCol[p%Width]*float64(uint64(1)<<ee.ScaleOccCol) +
			ee.Offset[p]*float64(uint64(1)<<ee.ScaleOccRem)
		pixOs := pixGain -
			osRef*(1.0+ee.Kta[p]*(common.Ta-25.0))*(1.0+ee.Kv[p]*(common.Vdd-3.3))
		vIr := pixOs / emissivity

		// 11.2.2.7 gradient compensation via CP
		cp := cpSp1
		if pattern == 0 {
			cp = cpSp0
		}
		vIrComp := vIr - ee.Tgc*cp

		// 11.2.2.8 α_comp
		a := (ee.AlphaRef +
			ee.AccRow[p/Width]*float64(uint64(1)<<ee.ScaleAccRow) +
			ee.AccCol[p%Width]*float64(uint64(1)<<ee.ScaleAccCol) +
			ee.AlphaPixel[p]*float64(uint64(1)<<ee.ScaleAccRem)) /
			float64(uint64(1)<<ee.AlphaScale)
		aCp := ee.AlphaCpSp1
		if pattern == 0 {
			aCp = ee.AlphaCpSp0
		}
		alphaComp := (a - ee.Tgc*aCp) * (1.0 + ee.KsTa*(common.Ta-25.0))

		out[p] = float32(ee.toFromIR(vIrComp, alphaComp, taR, 1))
	}
	return out, common
}

// toFromIR: §11.2.2.9 + §11.2.2.9.1 — To from compensated IR with the
// extended-range iteration (recompute in the landed range).
func (ee *Eeprom) toFromIR(vIr, alphaComp, taR float64, rangeHint int) float64 {
	rng := rangeHint
	if rng < 1 {
		rng = 1
	} else if rng > 4 {
		rng = 4
	}
	to := 0.0
	for i := 0; i < 3; i++ {
		ksTo := ee.KsTo[rng-1]
		alphaEff := alphaComp / ee.alphaCorr(rng)
		sx := ksTo * math.Pow(math.Pow(alphaEff, 3)*vIr+math.Pow(alphaEff, 4)*taR, 0.25)
		denom := alphaEff*(1.0-ksTo*273.15) + sx
		to = math.Pow(vIr/denom+taR, 0.25) - 273.15
		next := ee.classify(to)
		if next == rng {
			break
		}
		rng = next
	}
	return to
}

// classify returns the 1-based range index for a To estimate.
func (ee *Eeprom) classify(to float64) int {
	switch {
	case to < 0.0:
		return 1
	case to < ee.Ct3:
		return 2
	case to < ee.Ct4:
		return 3
	default:
		return 4
	}
}

// alphaCorr: 11.1.11 α correction factors per 1-based range.
func (ee *Eeprom) alphaCorr(rng int) float64 {
	switch rng {
	case 1:
		return 1.0 / (1.0 + ee.KsTo[0]*40.0)
	case 2:
		return 1.0
	case 3:
		return 1.0 + ee.KsTo[1]*ee.Ct3
	default:
		return (1.0 + ee.KsTo[1]*ee.Ct3) * (1.0 + ee.KsTo[2]*(ee.Ct4-ee.Ct3))
	}
}
