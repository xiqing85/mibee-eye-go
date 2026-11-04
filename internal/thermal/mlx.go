// Package thermal implements the MLX90640 radiometric side sensor (SPEC
// §3.7): the datasheet §11 compensation math, discovery scan, ironbow
// rendering and the poll service behind /api/thermal/* and the hardware
// bring-up API. Ported from the mibee-eye-thermal bring-up repo.
package thermal

import "github.com/xiqing85/mibee-eye-go/internal/i2c"

const (
	Width  = 32
	Height = 24
	Pixels = Width * Height

	DefaultAddr uint8 = 0x33

	RamIRBase uint16 = 0x0400
	RegStatus uint16 = 0x8000
	RegCtl    uint16 = 0x800D
	// RAM special data area (datasheet Figure 15).
	RegVbe   uint16 = 0x0700
	RegCpSp0 uint16 = 0x0708 // CP(SP0) — NOT PTAT; reads as a small negative.
	RegGain  uint16 = 0x070A
	RegPtat  uint16 = 0x0720 // die-temperature proxy; positive when healthy.
	RegCpSp1 uint16 = 0x0728
	RegVdd   uint16 = 0x072A
	EEBase   uint16 = 0x2400
	EEWords         = 832
	RegID    uint16 = 0x240C
)

// AuxReadings are the per-frame common readings from the RAM special area.
type AuxReadings struct {
	Vbe     int16
	CpSp0   int16
	CpSp1   int16
	Gain    int16
	Ptat    int16
	Vdd     int16
	Control uint16 // control register 1; resolution bits [11:10] feed Res_corr.
	Status  uint16
}

// Mlx90640 is the register-level sensor over any i2c.Bus.
type Mlx90640 struct {
	bus i2c.Bus
}

// NewMlx90640 wraps a bus.
func NewMlx90640(bus i2c.Bus) *Mlx90640 { return &Mlx90640{bus: bus} }

// ReadID reads the identity register (a NAK means wiring/address trouble).
func (m *Mlx90640) ReadID() (uint16, error) { return m.bus.ReadReg16(RegID) }

// ReadAux reads the RAM special area + control registers (§11.2.2.1–4).
func (m *Mlx90640) ReadAux() (AuxReadings, error) {
	var a AuxReadings
	var err error
	if a.Vbe, err = readI16(m.bus, RegVbe); err != nil {
		return a, err
	}
	if a.CpSp0, err = readI16(m.bus, RegCpSp0); err != nil {
		return a, err
	}
	if a.CpSp1, err = readI16(m.bus, RegCpSp1); err != nil {
		return a, err
	}
	if a.Gain, err = readI16(m.bus, RegGain); err != nil {
		return a, err
	}
	if a.Ptat, err = readI16(m.bus, RegPtat); err != nil {
		return a, err
	}
	if a.Vdd, err = readI16(m.bus, RegVdd); err != nil {
		return a, err
	}
	if a.Control, err = m.bus.ReadReg16(RegCtl); err != nil {
		return a, err
	}
	a.Status, err = m.bus.ReadReg16(RegStatus)
	return a, err
}

func readI16(b i2c.Bus, reg uint16) (int16, error) {
	w, err := b.ReadReg16(reg)
	return int16(w), err
}

// ReadFrameRaw reads one full IR frame (768 raw signed words, row-major).
func (m *Mlx90640) ReadFrameRaw() ([Pixels]int16, error) {
	var words [Pixels]uint16
	if err := i2c.ReadWords(m.bus, RamIRBase, words[:]); err != nil {
		return [Pixels]int16{}, err
	}
	var out [Pixels]int16
	for i, w := range words {
		out[i] = int16(w)
	}
	return out, nil
}

// ReadEEPROM reads the full calibration block (832 words, 0x2400–0x273F).
func (m *Mlx90640) ReadEEPROM() ([]uint16, error) {
	words := make([]uint16, EEWords)
	err := i2c.ReadWords(m.bus, EEBase, words)
	return words, err
}
