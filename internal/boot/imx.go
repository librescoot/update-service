package boot

import (
	"encoding/binary"
	"fmt"
)

// imxExtent checks the non-plugin U-Boot imximage v2 layout. It is not a
// signature check or proof that executable code is correct for this board.
func imxExtent(data []byte) (uint64, error) {
	bad := func(s string) (uint64, error) { return 0, fmt.Errorf("invalid i.MX image: %s", s) }
	if len(data) < 44 || data[0] != 0xd1 || binary.BigEndian.Uint16(data[1:3]) != 32 || (data[3] != 0x40 && data[3] != 0x41) {
		return bad("expected complete IVT v2, length 32")
	}
	word := func(off int) uint64 { return uint64(binary.LittleEndian.Uint32(data[off : off+4])) }
	self, entry, boot, dcd, csf := word(20), word(4), word(16), word(12), word(24)
	if self == 0 || self%4 != 0 || word(8) != 0 || word(28) != 0 {
		return bad("invalid self address or reserved fields")
	}
	// All pointers are load addresses relative to self, not device offsets.
	span := func(addr, size uint64) bool {
		return addr >= self && addr%4 == 0 && addr-self <= uint64(len(data)) && size <= uint64(len(data))-(addr-self)
	}
	if boot < self+32 || !span(boot, 12) {
		return bad("boot data pointer outside image")
	}
	bo := int(boot - self)
	start, size, plugin := word(bo), word(bo+4), word(bo+8)
	if start == 0 || start >= self || self-start != 1024 || plugin != 0 || size <= self-start || start+size > 1<<32 {
		return bad("unsupported load prefix, plugin, or boot data extent")
	}
	prefix := self - start
	extent := size - prefix
	if uint64(len(data)) > extent || self+uint64(len(data)) > 1<<32 {
		return bad("source exceeds declared image extent")
	}
	metadataEnd := boot + 12
	if dcd != 0 {
		if dcd < metadataEnd || !span(dcd, 4) {
			return bad("DCD pointer outside header")
		}
		off := int(dcd - self)
		n := uint64(binary.BigEndian.Uint16(data[off+1 : off+3]))
		if data[off] != 0xd2 || data[off+3] != 0x40 || n < 4 || !span(dcd, n) {
			return bad("invalid or truncated DCD")
		}
		metadataEnd = dcd + n
	}
	if entry < metadataEnd || !span(entry, 32) {
		return bad("entry does not point to usable payload")
	}
	payloadEnd := uint64(len(data))
	if csf == 0 {
		// mkimage rounds BootData.size (including the omitted 1KiB prefix)
		// to 4KiB, so up to 4095 bytes of declared tail need not be in the file.
		if size != (uint64(len(data))+prefix+4095)&^uint64(4095) {
			return bad("truncated or inconsistent image extent")
		}
	} else {
		if csf <= entry+32 || csf < self || (csf-start)%4096 != 0 || csf >= start+size {
			return bad("invalid CSF address")
		}
		reserved := start + size - csf
		if reserved > 64*1024 || reserved%4096 != 0 {
			return bad("unsupported CSF reservation")
		}
		csfOff := csf - self
		if csfOff >= uint64(len(data)) {
			// Unsigned u-boot-dtb.imx reserves CSF space without including it.
			if csf-start != (uint64(len(data))+prefix+4095)&^uint64(4095) {
				return bad("truncated payload before CSF")
			}
		} else {
			if !span(csf, 4) {
				return bad("truncated CSF header")
			}
			off := int(csfOff)
			n := uint64(binary.BigEndian.Uint16(data[off+1 : off+3]))
			if data[off] != 0xd4 || (data[off+3] != 0x40 && data[off+3] != 0x41) || n < 4 || n > reserved || !span(csf, n) {
				return bad("invalid or truncated CSF")
			}
			payloadEnd = csfOff
		}
	}
	if entry-self+32 > payloadEnd {
		return bad("no executable payload before CSF")
	}
	// Reject blank/erased entry code even when header pointers look plausible.
	nonzero, nonFF := false, false
	for _, v := range data[entry-self : entry-self+32] {
		nonzero = nonzero || v != 0
		nonFF = nonFF || v != 0xff
	}
	if !nonzero || !nonFF {
		return bad("blank entry payload")
	}
	return extent, nil
}
