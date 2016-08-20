/*

Package rpio provides GPIO access on the Raspberry PI without any need
for external c libraries (ex: WiringPI or BCM2835).

Supports simple operations such as:
- Pin mode/direction (input/output)
- Pin write (high/low)
- Pin read (high/low)
- Pull up/down/off

Example of use:

	rpio.Open()
	defer rpio.Close()

	pin := rpio.Pin(4)
	pin.Output()

	for {
		pin.Toggle()
		time.Sleep(time.Second)
	}

The library use the raw BCM2835 pinouts, not the ports as they are mapped
on the output pins for the raspberry pi

   Rev 1 Raspberry Pi
+------+------+--------+
| GPIO | Phys | Name   |
+------+------+--------+
|   0  |   3  | SDA    |
|   1  |   5  | SCL    |
|   4  |   7  | GPIO 7 |
|   7  |  26  | CE1    |
|   8  |  24  | CE0    |
|   9  |  21  | MISO   |
|  10  |  19  | MOSI   |
|  11  |  23  | SCLK   |
|  14  |   8  | TxD    |
|  15  |  10  | RxD    |
|  17  |  11  | GPIO 0 |
|  18  |  12  | GPIO 1 |
|  21  |  13  | GPIO 2 |
|  22  |  15  | GPIO 3 |
|  23  |  16  | GPIO 4 |
|  24  |  18  | GPIO 5 |
|  25  |  22  | GPIO 6 |
+------+------+--------+

See the spec for full details of the BCM2835 controller:
http://www.raspberrypi.org/wp-content/uploads/2012/02/BCM2835-ARM-Peripherals.pdf

*/

package rpio

import (
	"bytes"
	"encoding/binary"
	"os"
	"reflect"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

type Direction uint8
type Pin uint8
type State uint8
type Pull uint8

// Memory offsets for gpio, see the spec for more details
const (
	bcm2835Base = 0x20000000
	pi1GPIOBase = bcm2835Base + 0x200000
	memLength   = 4096

	pinMask uint32 = 7 // 0b111 - pinmode is 3 bits
)

// Pin direction, a pin can be set in Input or Output mode
const (
	Input Direction = iota
	Output
)

// State of pin, High / Low
const (
	Low State = iota
	High
)

// Pull Up / Down / Off
const (
	PullOff Pull = iota
	PullDown
	PullUp
)

// Arrays for 8 / 32 bit access to memory and a semaphore for write locking
type GPIO struct {
	memlock sync.Mutex
	mem     []uint32
	mem8    []uint8
}

func Init() *GPIO {
	return &GPIO
}

// Set pin as Input
func (g *GPIO) Input(pin Pin) {
	g.PinMode(pin, Input)
}

// Set pin as Output
func (g *GPIO) Output(pin Pin) {
	g.PinMode(pin, Output)
}

// Set pin High
func (g *GPIO) High(pin Pin) {
	g.WritePin(pin, High)
}

// Set pin Low
func (g *GPIO) Low(pin Pin) {
	g.WritePin(pin, Low)
}

// Toggle pin state
func (g *GPIO) Toggle(pin Pin) {
	g.TogglePin(pin)
}

// Set pin Direction
func (g *GPIO) Mode(pin Pin, dir Direction) {
	g.PinMode(pin, dir)
}

// Set pin state (high/low)
func (g *GPIO) Write(pin Pin, state State) {
	g.WritePin(pin, state)
}

// Read pin state (high/low)
func (g *GPIO) Read(pin Pin) State {
	return g.ReadPin(pin)
}

// Set a given pull up/down mode
func (g *GPIO) Pull(pin Pin, pull Pull) {
	g.PullMode(pin, pull)
}

// Pull up pin
func (g *GPIO) PullUp(pin Pin) {
	g.PullMode(pin, PullUp)
}

// Pull down pin
func (g *GPIO) PullDown(pin Pin) {
	g.PullMode(pin, PullDown)
}

// Disable pullup/down on pin
func (g *GPIO) PullOff(pin Pin) {
	g.PullMode(pin, PullOff)
}

// PinMode sets the direction of a given pin (Input or Output)
func (g *GPIO) PinMode(pin Pin, direction Direction) {

	// Pin fsel register, 0 or 1 depending on bank
	fsel := uint8(pin) / 10
	shift := (uint8(pin) % 10) * 3

	g.memlock.Lock()
	defer g.memlock.Unlock()

	if direction == Input {
		g.mem[fsel] = g.mem[fsel] &^ (pinMask << shift)
	} else {
		g.mem[fsel] = (g.mem[fsel] &^ (pinMask << shift)) | (1 << shift)
	}

}

// WritePin sets a given pin High or Low
// by setting the clear or set registers respectively
func (g *GPIO) WritePin(pin Pin, state State) {

	p := uint8(pin)

	// Clear register, 10 / 11 depending on bank
	// Set register, 7 / 8 depending on bank
	clearReg := p/32 + 10
	setReg := p/32 + 7

	g.memlock.Lock()
	defer g.memlock.Unlock()

	if state == Low {
		g.mem[clearReg] = 1 << (p & 31)
	} else {
		g.mem[setReg] = 1 << (p & 31)
	}

}

// Read the state of a pin
func (g *GPIO) ReadPin(pin Pin) State {
	// Input level register offset (13 / 14 depending on bank)
	levelReg := uint8(pin)/32 + 13

	if (g.mem[levelReg] & (1 << uint8(pin))) != 0 {
		return High
	}

	return Low
}

// Toggle a pin state (high -> low -> high)
// TODO: probably possible to do this much faster without read
func (g *GPIO) TogglePin(pin Pin) {
	switch g.ReadPin(pin) {
	case Low:
		g.High(pin)
	case High:
		g.Low(pin)
	}
}

func (g *GPIO) PullMode(pin Pin, pull Pull) {
	// Pull up/down/off register has offset 38 / 39, pull is 37
	pullClkReg := uint8(pin)/32 + 38
	pullReg := 37
	shift := (uint8(pin) % 32)

	g.memlock.Lock()
	defer g.memlock.Unlock()

	switch pull {
	case PullDown, PullUp:
		g.mem[pullReg] = g.mem[pullReg]&^3 | uint32(pull)
	case PullOff:
		g.mem[pullReg] = g.mem[pullReg] &^ 3
	}

	// Wait for value to clock in, this is ugly, sorry :(
	time.Sleep(time.Microsecond)

	g.mem[pullClkReg] = 1 << shift

	// Wait for value to clock in
	time.Sleep(time.Microsecond)

	g.mem[pullReg] = g.mem[pullReg] &^ 3
	g.mem[pullClkReg] = 0

}

// Open and memory map GPIO memory range from /dev/mem .
// Some reflection magic is used to convert it to a unsafe []uint32 pointer
func (g *GPIO) Open() (err error) {
	var file *os.File
	var base int64

	// Open fd for rw mem access; try gpiomem first
	if file, err = os.OpenFile(
		"/dev/gpiomem",
		os.O_RDWR|os.O_SYNC,
		0); os.IsNotExist(err) {
		file, err = os.OpenFile(
			"/dev/mem",
			os.O_RDWR|os.O_SYNC,
			0)
		base = g.getGPIOBase()
	}

	if err != nil {
		return
	}

	// FD can be closed after memory mapping
	defer file.Close()

	g.memlock.Lock()
	defer g.memlock.Unlock()

	// Memory map GPIO registers to byte array
	g.mem8, err = syscall.Mmap(
		int(file.Fd()),
		base,
		memLength,
		syscall.PROT_READ|syscall.PROT_WRITE,
		syscall.MAP_SHARED)

	if err != nil {
		return
	}

	// Convert mapped byte memory to unsafe []uint32 pointer, adjust length as needed
	header := *(*reflect.SliceHeader)(unsafe.Pointer(&g.mem8))
	header.Len /= (32 / 8) // (32 bit = 4 bytes)
	header.Cap /= (32 / 8)

	g.mem = *(*[]uint32)(unsafe.Pointer(&header))

	return nil
}

// Close unmaps GPIO memory
func (g *GPIO) Close() error {
	g.memlock.Lock()
	defer g.memlock.Unlock()
	return syscall.Munmap(g.mem8)
}

// Read /proc/device-tree/soc/ranges and determine the base address.
// Use the default Raspberry Pi 1 base address if this fails.
func (g *GPIO) getGPIOBase() (base int64) {
	base = pi1GPIOBase
	ranges, err := os.Open("/proc/device-tree/soc/ranges")
	defer ranges.Close()
	if err != nil {
		return
	}
	b := make([]byte, 4)
	n, err := ranges.ReadAt(b, 4)
	if n != 4 || err != nil {
		return
	}
	buf := bytes.NewReader(b)
	var out uint32
	err = binary.Read(buf, binary.BigEndian, &out)
	if err != nil {
		return
	}
	return int64(out + 0x200000)
}
