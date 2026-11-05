//go:build linux

// Package i2c provides the two MLX90640 transports proven on the bench:
// the kernel hardware I2C (/dev/i2c-N via I2C_RDWR combined transfers) and
// a sysfs-GPIO bit-bang fallback for modules without usable pull-ups.
//
// The register protocol is 16-bit big-endian addressing with SMBus-style
// word/block reads: START, addr+W, regH, regL, RESTART, addr+R, …, STOP.
package i2c

import (
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

const (
	i2cRdwr = 0x0707 // I2C_RDWR (kernel uapi raw constant, sizeless)
	i2cMRD  = 0x0001
)

// Bus is the transport-agnostic MLX90640 contract.
type Bus interface {
	// ReadReg16 reads a single 16-bit register (big-endian on the wire).
	ReadReg16(reg uint16) (uint16, error)
	// ReadBlock reads len(buf) bytes starting at reg (auto-incrementing).
	ReadBlock(reg uint16, buf []byte) error
	// WriteReg16 writes a single 16-bit register.
	WriteReg16(reg, val uint16) error
}

// ReadWords fills out with big-endian 16-bit words from a block read,
// chunked (256-byte chunks validated on real hardware on the bench).
func ReadWords(b Bus, base uint16, out []uint16) error {
	const chunkWords = 128
	byteIdx := 0
	for byteIdx < len(out)*2 {
		reg := base + uint16(byteIdx/2)
		take := ((len(out)*2 - byteIdx) / 2)
		if take > chunkWords {
			take = chunkWords
		}
		take *= 2
		tmp := make([]byte, take)
		if err := b.ReadBlock(reg, tmp); err != nil {
			return err
		}
		for i := 0; i+1 < len(tmp); i += 2 {
			out[byteIdx/2+i/2] = uint16(tmp[i])<<8 | uint16(tmp[i+1])
		}
		byteIdx += take
	}
	return nil
}

// ————————————————————————————————————————————————————————————————
// Twi: hardware I2C via /dev/i2c-N
// ————————————————————————————————————————————————————————————————

type i2cMsg struct {
	addr  uint16
	flags uint16
	len   uint16
	_     uint16 // tail pad to the C ABI
	buf   *byte
}

type i2cRdwrIoctlData struct {
	msgs  *i2cMsg
	nmsgs uint32
}

// Twi is a /dev/i2c-N handle using combined I2C_RDWR transfers.
type Twi struct {
	file *os.File
	addr uint8
}

// OpenTwi opens the bus device; addr is the 7-bit slave address.
func OpenTwi(path string, addr uint8) (*Twi, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	return &Twi{file: f, addr: addr}, nil
}

func (t *Twi) ioctl(data *i2cRdwrIoctlData) error {
	rc, _, errno := syscall.Syscall(syscall.SYS_IOCTL, t.file.Fd(),
		uintptr(i2cRdwr), uintptr(unsafe.Pointer(data)))
	if rc < 0 {
		return errno
	}
	return nil
}

// wrRd does one ioctl: [write regH,regL] + repeated-start + [read n bytes].
func (t *Twi) wrRd(reg uint16, rdLen int) ([]byte, error) {
	wr := []byte{byte(reg >> 8), byte(reg)}
	out := make([]byte, rdLen)
	msgs := []i2cMsg{
		{addr: uint16(t.addr), flags: 0, len: uint16(len(wr)), buf: &wr[0]},
		{addr: uint16(t.addr), flags: i2cMRD, len: uint16(rdLen), buf: &out[0]},
	}
	data := i2cRdwrIoctlData{msgs: &msgs[0], nmsgs: 2}
	if err := t.ioctl(&data); err != nil {
		return nil, fmt.Errorf("i2c %s I2C_RDWR %#06x (%dB read): %w", t.file.Name(), reg, rdLen, err)
	}
	return out, nil
}

func (t *Twi) ReadReg16(reg uint16) (uint16, error) {
	b, err := t.wrRd(reg, 2)
	if err != nil {
		return 0, err
	}
	return uint16(b[0])<<8 | uint16(b[1]), nil
}

func (t *Twi) ReadBlock(reg uint16, buf []byte) error {
	b, err := t.wrRd(reg, len(buf))
	if err != nil {
		return err
	}
	copy(buf, b)
	return nil
}

func (t *Twi) WriteReg16(reg, val uint16) error {
	wr := []byte{byte(reg >> 8), byte(reg), byte(val >> 8), byte(val)}
	msgs := []i2cMsg{{addr: uint16(t.addr), len: uint16(len(wr)), buf: &wr[0]}}
	data := i2cRdwrIoctlData{msgs: &msgs[0], nmsgs: 1}
	if err := t.ioctl(&data); err != nil {
		return fmt.Errorf("i2c %s I2C_RDWR write %#06x: %w", t.file.Name(), reg, err)
	}
	return nil
}

func (t *Twi) Close() error { return t.file.Close() }

// ————————————————————————————————————————————————————————————————
// BitBang: sysfs-GPIO open-drain bit-bang (~1.6 kHz SCL)
// ————————————————————————————————————————————————————————————————

const halfPeriod = 400 * time.Microsecond

type gpio struct {
	num  uint32
	root string
}

func openGpio(num uint32) (*gpio, error) {
	root := fmt.Sprintf("/sys/class/gpio/gpio%d", num)
	if _, err := os.Stat(root); os.IsNotExist(err) {
		if err := os.WriteFile("/sys/class/gpio/export", []byte(fmt.Sprintf("%d", num)), 0o200); err != nil {
			return nil, err
		}
		time.Sleep(50 * time.Millisecond)
	}
	g := &gpio{num: num, root: root}
	if err := g.rz(); err != nil { // idle: released (input), pulled high
		return nil, err
	}
	return g, nil
}

func (g *gpio) write(name, val string) error {
	return os.WriteFile(g.root+"/"+name, []byte(val), 0o200)
}

// lo drives the line low (open-drain "0"); rz releases it (pull-up wins).
func (g *gpio) lo() error { return g.write("direction", "out\n") }
func (g *gpio) rz() error { return g.write("direction", "in\n") }

func (g *gpio) rd() (int, error) {
	b, err := os.ReadFile(g.root + "/value")
	if err != nil {
		return 0, err
	}
	v := 0
	for _, c := range b {
		if c >= '0' && c <= '9' {
			v = int(c - '0')
		}
	}
	return v, nil
}

func (g *gpio) close() {
	_ = os.WriteFile("/sys/class/gpio/unexport", []byte(fmt.Sprintf("%d", g.num)), 0o200)
}

func d(n time.Duration) { time.Sleep(halfPeriod * n) }

// recoverBus clocks SCL with SDA released until the slave lets SDA rise
// (≤32 clocks covers any outstanding transfer), then a STOP resyncs it.
// MLX90640 has no SMBus timeout and parks SDA low if a master died
// mid-block-read.
func recoverBus(sda, scl *gpio) error {
	for i := 0; i < 32; i++ {
		if err := scl.lo(); err != nil {
			return err
		}
		d(1)
		if err := scl.rz(); err != nil {
			return err
		}
		d(2) // generous: weak pull-up rise is 240-510 µs
		v, err := sda.rd()
		if err != nil {
			return err
		}
		if v != 0 {
			// SDA released — finish with a STOP condition.
			_ = scl.lo()
			d(1)
			_ = sda.lo()
			d(1)
			_ = scl.rz()
			d(2)
			_ = sda.rz()
			d(2)
			return nil
		}
	}
	return nil // still low after 32 clocks — genuinely stuck hardware
}

// BitBang bit-bangs I2C over two sysfs GPIO lines.
type BitBang struct {
	addr uint8
	sda  *gpio
	scl  *gpio
}

// NewBitBang exports both pins (idempotent), recovers a wedged bus and
// idles the lines high. sda/scl are global sysfs GPIO numbers.
func NewBitBang(sda, scl uint32, addr uint8) (*BitBang, error) {
	gsda, err := openGpio(sda)
	if err != nil {
		return nil, err
	}
	gscl, err := openGpio(scl)
	if err != nil {
		gsda.close()
		return nil, err
	}
	b := &BitBang{addr: addr, sda: gsda, scl: gscl}
	d(2)
	v, err := gsda.rd()
	if err == nil && v == 0 {
		if err := recoverBus(gsda, gscl); err != nil {
			b.Close()
			return nil, err
		}
	}
	if v, err := gscl.rd(); err != nil || v == 0 {
		b.Close()
		return nil, fmt.Errorf("i2c bitbang: SCL stuck low: check module VCC/GND and pull-ups")
	}
	d(2)
	if v, err := gsda.rd(); err == nil && v == 0 {
		b.Close()
		return nil, fmt.Errorf("i2c bitbang: SDA stuck low after bus recovery: check module VCC/GND and pull-ups")
	}
	return b, nil
}

// SetAddr retargets the probe address without re-opening the bus — the
// discovery scan walks 0x30..=0x3E on one exported pair.
func (b *BitBang) SetAddr(addr uint8) { b.addr = addr }

func (b *BitBang) start() error {
	if err := b.sda.rz(); err != nil {
		return err
	}
	if err := b.scl.rz(); err != nil {
		return err
	}
	d(2)
	if err := b.sda.lo(); err != nil {
		return err
	}
	d(1)
	return b.scl.lo()
}

func (b *BitBang) rstart() error {
	if err := b.sda.rz(); err != nil {
		return err
	}
	d(1)
	if err := b.scl.rz(); err != nil {
		return err
	}
	d(3)
	if err := b.sda.lo(); err != nil {
		return err
	}
	d(1)
	return b.scl.lo()
}

func (b *BitBang) stop() error {
	if err := b.sda.lo(); err != nil {
		return err
	}
	d(1)
	if err := b.scl.rz(); err != nil {
		return err
	}
	d(3)
	if err := b.sda.rz(); err != nil {
		return err
	}
	d(2)
	return nil
}

func (b *BitBang) wrBit(bit bool) error {
	var err error
	if bit {
		err = b.sda.rz()
	} else {
		err = b.sda.lo()
	}
	if err != nil {
		return err
	}
	d(1)
	if err := b.scl.rz(); err != nil {
		return err
	}
	d(3)
	if err := b.scl.lo(); err != nil {
		return err
	}
	d(1)
	return nil
}

func (b *BitBang) rdBit() (int, error) {
	if err := b.sda.rz(); err != nil {
		return 0, err
	}
	d(1)
	if err := b.scl.rz(); err != nil {
		return 0, err
	}
	d(3)
	v, err := b.sda.rd()
	if err != nil {
		return 0, err
	}
	if err := b.scl.lo(); err != nil {
		return 0, err
	}
	d(1)
	return v, nil
}

func (b *BitBang) sendByte(v byte) (bool, error) {
	for i := 7; i >= 0; i-- {
		if err := b.wrBit(v>>uint(i)&1 == 1); err != nil {
			return false, err
		}
	}
	ack, err := b.rdBit()
	if err != nil {
		return false, err
	}
	return ack == 0, nil // slave ACK
}

func (b *BitBang) mack() error {
	if err := b.sda.lo(); err != nil {
		return err
	}
	d(1)
	if err := b.scl.rz(); err != nil {
		return err
	}
	d(3)
	if err := b.scl.lo(); err != nil {
		return err
	}
	d(1)
	return b.sda.rz()
}

func (b *BitBang) mnak() error {
	if err := b.sda.rz(); err != nil {
		return err
	}
	d(1)
	if err := b.scl.rz(); err != nil {
		return err
	}
	d(3)
	return b.scl.lo()
}

func (b *BitBang) readBlockAt(reg uint16, buf []byte) error {
	addr := b.addr
	if err := b.start(); err != nil {
		return err
	}
	ack, err := b.sendByte(byte(addr << 1))
	if err != nil {
		return err
	}
	if !ack {
		_ = b.stop()
		return fmt.Errorf("i2c bitbang: NAK addr %#04x (write phase)", addr)
	}
	if ack, err = b.sendByte(byte(reg >> 8)); err != nil || !ack {
		_ = b.stop()
		return fmt.Errorf("i2c bitbang: NAK reg-hi")
	}
	if ack, err = b.sendByte(byte(reg)); err != nil || !ack {
		_ = b.stop()
		return fmt.Errorf("i2c bitbang: NAK reg-lo")
	}
	if err := b.rstart(); err != nil {
		return err
	}
	if ack, err = b.sendByte(byte(addr<<1 | 1)); err != nil || !ack {
		_ = b.stop()
		return fmt.Errorf("i2c bitbang: NAK addr %#04x (read phase)", addr)
	}
	n := len(buf)
	for i := range buf {
		var v byte
		for k := 0; k < 8; k++ {
			bit, err := b.rdBit()
			if err != nil {
				return err
			}
			v = v<<1 | byte(bit)
		}
		if i+1 < n {
			err = b.mack()
		} else {
			err = b.mnak()
		}
		if err != nil {
			return err
		}
		buf[i] = v
	}
	return b.stop()
}

func (b *BitBang) ReadReg16(reg uint16) (uint16, error) {
	var buf [2]byte
	if err := b.readBlockAt(reg, buf[:]); err != nil {
		return 0, err
	}
	return uint16(buf[0])<<8 | uint16(buf[1]), nil
}

func (b *BitBang) ReadBlock(reg uint16, buf []byte) error {
	return b.readBlockAt(reg, buf)
}

func (b *BitBang) WriteReg16(_, _ uint16) error {
	// Only needed later (refresh-rate / chess-mode configuration); the
	// bring-up path is read-only.
	return fmt.Errorf("i2c bitbang: write not implemented")
}

func (b *BitBang) Close() {
	if b.sda != nil {
		b.sda.close()
	}
	if b.scl != nil {
		b.scl.close()
	}
}
