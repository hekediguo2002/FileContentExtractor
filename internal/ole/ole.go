package ole

import (
	"encoding/binary"
	"fmt"
	"unicode/utf16"
)

func Read(data []byte) (map[string][]byte, error) {
	if len(data) < 512 {
		return nil, fmt.Errorf("truncated OLE2 file")
	}
	if string(data[:8]) != "\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1" {
		return nil, fmt.Errorf("not an OLE2 file")
	}
	secShift := binary.LittleEndian.Uint16(data[30:32])
	if secShift < 9 || secShift > 12 {
		return nil, fmt.Errorf("unsupported sector size shift %d", secShift)
	}
	miniShift := binary.LittleEndian.Uint16(data[32:34])
	if miniShift < 6 || miniShift > secShift {
		return nil, fmt.Errorf("unsupported mini sector size shift %d", miniShift)
	}
	sectorSize := 1 << secShift
	miniSize := 1 << miniShift
	nFAT := int(binary.LittleEndian.Uint32(data[44:48]))
	firstDir := int32(binary.LittleEndian.Uint32(data[48:52]))
	cutoff := int64(binary.LittleEndian.Uint32(data[56:60]))
	firstMiniFAT := int32(binary.LittleEndian.Uint32(data[60:64]))
	nMiniFAT := int(binary.LittleEndian.Uint32(data[64:68]))
	firstDIFAT := int32(binary.LittleEndian.Uint32(data[68:72]))
	nDIFAT := int(binary.LittleEndian.Uint32(data[72:76]))
	// Clamp sector counts to what the file can physically contain.
	if maxSectors := len(data) / sectorSize; true {
		if nFAT > maxSectors {
			nFAT = maxSectors
		}
		if nMiniFAT > maxSectors {
			nMiniFAT = maxSectors
		}
		if nDIFAT > maxSectors {
			nDIFAT = maxSectors
		}
	}
	sector := func(id int32) []byte {
		if id < 0 {
			return nil
		}
		off := (int(id) + 1) * sectorSize
		if off < 0 || off+sectorSize > len(data) {
			return nil
		}
		return data[off : off+sectorSize]
	}
	var fatIDs []int32
	for i := 0; i < 109 && len(fatIDs) < nFAT; i++ {
		v := int32(binary.LittleEndian.Uint32(data[76+i*4 : 80+i*4]))
		if v >= 0 {
			fatIDs = append(fatIDs, v)
		}
	}
	for id, n := firstDIFAT, 0; id >= 0 && n < nDIFAT && len(fatIDs) < nFAT; n++ {
		s := sector(id)
		if s == nil {
			break
		}
		for i := 0; i < sectorSize-4 && len(fatIDs) < nFAT; i += 4 {
			v := int32(binary.LittleEndian.Uint32(s[i : i+4]))
			if v >= 0 {
				fatIDs = append(fatIDs, v)
			}
		}
		id = int32(binary.LittleEndian.Uint32(s[sectorSize-4:]))
	}
	var fat []int32
	for _, id := range fatIDs {
		s := sector(id)
		if s == nil {
			continue
		}
		for i := 0; i < sectorSize; i += 4 {
			fat = append(fat, int32(binary.LittleEndian.Uint32(s[i:i+4])))
		}
	}
	chain := func(start int32, size int64) []byte {
		var out []byte
		seen := map[int32]bool{}
		for start >= 0 && !seen[start] && int64(len(out)) < size {
			seen[start] = true
			s := sector(start)
			if s == nil {
				break
			}
			out = append(out, s...)
			if int(start) >= len(fat) {
				break
			}
			start = fat[start]
		}
		if int64(len(out)) > size {
			out = out[:size]
		}
		return out
	}
	directory := chain(firstDir, 1<<31)
	type entry struct {
		name  string
		start int32
		size  int64
		mini  bool
	}
	var entries []entry
	rootStart := int32(-1)
	var rootSize int64
	for off := 0; off+128 <= len(directory); off += 128 {
		n := int(binary.LittleEndian.Uint16(directory[off+64 : off+66]))
		if n < 2 || n > 64 {
			continue
		}
		units := make([]uint16, 0, n/2-1)
		for i := 0; i < n-2; i += 2 {
			units = append(units, binary.LittleEndian.Uint16(directory[off+i:off+i+2]))
		}
		name := string(utf16.Decode(units))
		typ := directory[off+66]
		start := int32(binary.LittleEndian.Uint32(directory[off+116 : off+120]))
		size := int64(binary.LittleEndian.Uint64(directory[off+120 : off+128]))
		if typ == 5 {
			rootStart, rootSize = start, size
		} else if typ == 2 {
			entries = append(entries, entry{name, start, size, size < cutoff})
		}
	}
	miniStream := chain(rootStart, rootSize)
	miniFATData := chain(firstMiniFAT, int64(nMiniFAT*sectorSize))
	miniFAT := make([]int32, len(miniFATData)/4)
	for i := range miniFAT {
		miniFAT[i] = int32(binary.LittleEndian.Uint32(miniFATData[i*4 : i*4+4]))
	}
	readMini := func(start int32, size int64) []byte {
		var out []byte
		seen := map[int32]bool{}
		for start >= 0 && !seen[start] && int64(len(out)) < size {
			seen[start] = true
			off := int(start) * miniSize
			if off < 0 || off+miniSize > len(miniStream) {
				break
			}
			out = append(out, miniStream[off:off+miniSize]...)
			if int(start) >= len(miniFAT) {
				break
			}
			start = miniFAT[start]
		}
		if int64(len(out)) > size {
			out = out[:size]
		}
		return out
	}
	result := map[string][]byte{}
	for _, e := range entries {
		if e.mini {
			result[e.name] = readMini(e.start, e.size)
		} else {
			result[e.name] = chain(e.start, e.size)
		}
	}
	return result, nil
}
