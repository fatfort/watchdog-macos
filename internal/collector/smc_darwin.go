//go:build darwin && cgo

package collector

/*
#cgo LDFLAGS: -framework IOKit -framework CoreFoundation

#include <IOKit/IOKitLib.h>
#include <CoreFoundation/CoreFoundation.h>
#include <string.h>

// SMC user-client interface — selectors 2 (KEY_INFO) and 5 (READ_KEY) are
// the ones we need. Layout below is the AppleSMC keyData struct that's been
// stable since OS X 10.6.
typedef struct {
    char           major;
    char           minor;
    char           build;
    char           reserved[1];
    unsigned short release;
} SMCVersion;

typedef struct {
    unsigned short version;
    unsigned short length;
    unsigned int   cpuPLimit;
    unsigned int   gpuPLimit;
    unsigned int   memPLimit;
} SMCPLimitData;

typedef struct {
    unsigned int dataSize;
    unsigned int dataType;
    char         dataAttributes;
} SMCKeyInfoData;

typedef struct {
    unsigned int   key;
    SMCVersion     vers;
    SMCPLimitData  pLimitData;
    SMCKeyInfoData keyInfo;
    char           result;
    char           status;
    char           data8;
    unsigned int   data32;
    unsigned char  bytes[32];
} SMCKeyData;

static io_connect_t smc_conn = 0;

// smc_open opens a connection to AppleSMC. Returns 0 on success, kIOReturn*
// error on failure.
static int smc_open(void) {
    if (smc_conn != 0) return 0;
    io_iterator_t iterator;
    CFMutableDictionaryRef matching = IOServiceMatching("AppleSMC");
    kern_return_t result = IOServiceGetMatchingServices(kIOMainPortDefault, matching, &iterator);
    if (result != kIOReturnSuccess) return result;
    io_object_t device = IOIteratorNext(iterator);
    IOObjectRelease(iterator);
    if (device == 0) return -1;
    result = IOServiceOpen(device, mach_task_self(), 0, &smc_conn);
    IOObjectRelease(device);
    return result;
}

// smc_call invokes a method on the AppleSMC user-client. selector 2 = KEY_INFO
// (returns dataSize/dataType for a key), 5 = READ_KEY (returns key bytes).
static int smc_call(int index, SMCKeyData *in, SMCKeyData *out) {
    size_t inSize = sizeof(SMCKeyData);
    size_t outSize = sizeof(SMCKeyData);
    return IOConnectCallStructMethod(smc_conn, index, in, inSize, out, &outSize);
}

// smc_read_key reads `key` (a 4-char FourCC packed big-endian) into outBuf,
// up to outBufLen bytes. Returns the number of bytes actually read, or
// -errno on failure.
static int smc_read_key(unsigned int key, unsigned char *outBuf, int outBufLen, unsigned int *outType) {
    SMCKeyData in;
    SMCKeyData out;
    memset(&in, 0, sizeof(in));
    memset(&out, 0, sizeof(out));
    in.key = key;
    in.data8 = 9; // SMC_CMD_READ_KEYINFO
    int r = smc_call(2, &in, &out);
    if (r != kIOReturnSuccess) return -1;
    unsigned int sz = out.keyInfo.dataSize;
    if (sz > (unsigned int)outBufLen) sz = outBufLen;
    if (outType) *outType = out.keyInfo.dataType;

    memset(&in, 0, sizeof(in));
    memset(&out, 0, sizeof(out));
    in.key = key;
    in.keyInfo.dataSize = sz;
    in.data8 = 5; // SMC_CMD_READ_BYTES
    r = smc_call(5, &in, &out);
    if (r != kIOReturnSuccess) return -2;
    memcpy(outBuf, out.bytes, sz);
    return (int)sz;
}

// Helpers exposed to cgo callers.
int smc_init(void) { return smc_open(); }

int smc_read(unsigned int key, unsigned char *outBuf, int outBufLen, unsigned int *outType) {
    return smc_read_key(key, outBuf, outBufLen, outType);
}
*/
import "C"

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"unsafe"
)

var (
	smcInitOnce sync.Once
	smcInitErr  error
)

// fourcc packs a 4-character SMC key into a big-endian uint32. The IOKit
// interface expects keys as a literal 32-bit integer with the ASCII bytes
// in MSB→LSB order (e.g. "TC0P" → 0x54433050).
func fourcc(s string) uint32 {
	if len(s) != 4 {
		return 0
	}
	return uint32(s[0])<<24 | uint32(s[1])<<16 | uint32(s[2])<<8 | uint32(s[3])
}

// readSMCKey reads up to 32 bytes for the given SMC key. The returned slice
// is sized to the key's actual data length. dataType is the FourCC of the
// SMC type (e.g. "sp78", "flt ", "ui8 ", "fpe2").
func readSMCKey(key string) ([]byte, string, error) {
	smcInitOnce.Do(func() {
		if rc := C.smc_init(); rc != 0 {
			smcInitErr = errors.New("smc_init failed")
		}
	})
	if smcInitErr != nil {
		return nil, "", smcInitErr
	}
	var buf [32]C.uchar
	var dtype C.uint
	n := C.smc_read(C.uint(fourcc(key)), &buf[0], C.int(len(buf)), &dtype)
	if n < 0 {
		return nil, "", errors.New("smc_read failed")
	}
	out := make([]byte, int(n))
	for i := 0; i < int(n); i++ {
		out[i] = byte(buf[i])
	}
	// Unpack dataType uint32 → 4-byte ASCII tag.
	t := uint32(dtype)
	tag := []byte{byte(t >> 24), byte(t >> 16), byte(t >> 8), byte(t)}
	_ = unsafe.Sizeof(buf) // keep cgo happy about buf address
	return out, string(tag), nil
}

// decodeSMCFloat converts an SMC value to a float using its type tag. Handles
// the formats we actually encounter on macOS: sp78 (signed 8.8 fixed-point,
// classic temp/fan), flt (IEEE 754 little-endian, Apple Silicon temps), and
// fpe2 (unsigned 14.2 fixed-point, classic fan RPM).
func decodeSMCFloat(data []byte, tag string) (float64, bool) {
	switch tag {
	case "sp78":
		if len(data) < 2 {
			return 0, false
		}
		// Big-endian signed 8.8 fixed-point.
		raw := int16(uint16(data[0])<<8 | uint16(data[1]))
		return float64(raw) / 256.0, true
	case "flt ":
		if len(data) < 4 {
			return 0, false
		}
		bits := binary.LittleEndian.Uint32(data[:4])
		v := math.Float32frombits(bits)
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return 0, false
		}
		return float64(v), true
	case "fpe2":
		if len(data) < 2 {
			return 0, false
		}
		// Big-endian unsigned 14.2 fixed-point.
		raw := uint16(data[0])<<8 | uint16(data[1])
		return float64(raw) / 4.0, true
	case "ui8 ", "ui16", "ui32":
		var v uint64
		for _, b := range data {
			v = v<<8 | uint64(b)
		}
		return float64(v), true
	}
	return 0, false
}

// readSMCTemp tries an ordered list of likely CPU-temperature keys, returning
// the first that produces a plausible value. Apple Silicon has no single
// "CPU die temp" key — Tp09 is a performance core that's almost always live.
// Tc0c/Tc0d are firstgen M1 fallbacks, Tp0T is an M1/M2 thermal pressure key.
// TC0P/TC0H are Intel's "CPU proximity" and "CPU heatsink" keys.
func readSMCTemp() float64 {
	candidates := []string{
		// Apple Silicon (M1/M2/M3/M4) — performance-core die temps.
		"Tp09", "Tp01", "Tp05", "Tp0D", "Tp0H", "Tp0L", "Tp0a", "Tp0b",
		// Apple Silicon — efficient-core, used on fanless Airs.
		"Te05", "Te0L", "Te0P", "Te0S",
		// Apple Silicon — thermal pressure / package die.
		"Tp0T",
		// Intel.
		"TC0P", "TC0H", "TC0D", "TC0E",
	}
	var best float64
	for _, k := range candidates {
		data, tag, err := readSMCKey(k)
		if err != nil {
			continue
		}
		v, ok := decodeSMCFloat(data, tag)
		if !ok {
			continue
		}
		if v < 5 || v > 125 { // implausible — skip
			continue
		}
		if v > best {
			best = v
		}
	}
	return best
}

// readSMCFan reads the maximum currently-reporting fan RPM across F0Ac..F3Ac.
// Returns 0 on fanless Macs (Air, mini-M1 in some configs) or when no key
// reports a positive value. Fan count lives in FNum but we don't bother — the
// missing keys just fail and contribute 0.
func readSMCFan() int {
	var max float64
	for _, k := range []string{"F0Ac", "F1Ac", "F2Ac", "F3Ac"} {
		data, tag, err := readSMCKey(k)
		if err != nil {
			continue
		}
		v, ok := decodeSMCFloat(data, tag)
		if !ok {
			continue
		}
		if v > max {
			max = v
		}
	}
	return int(max)
}

// readSMCThermal is the entry point used by collectThermal.
func readSMCThermal() (float64, int, error) {
	smcInitOnce.Do(func() {
		if rc := C.smc_init(); rc != 0 {
			smcInitErr = errors.New("smc_init failed")
		}
	})
	if smcInitErr != nil {
		return 0, 0, smcInitErr
	}
	return readSMCTemp(), readSMCFan(), nil
}
