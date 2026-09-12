package doc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"html"
	"os"
	"regexp"
	"strings"
	"unicode/utf16"

	"github.com/hekediguo2002/FileContentExtractor/layout"
	"github.com/hekediguo2002/FileContentExtractor/model"
)

func ParseFile(name string) (*model.Document, error) {
	b, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	d := &model.Document{Path: name, Format: "doc", Pagination: "layout-estimated", Pages: []model.Page{{Number: 1}}}
	if bytes.HasPrefix(bytes.TrimSpace(b), []byte("<!DOCTYPE html")) || bytes.HasPrefix(bytes.TrimSpace(b), []byte("<html")) {
		d.Pagination = "single-flow"
		t := htmlText(string(b))
		d.Pages[0].Text = t
		d.Pages[0].Runs = []model.TextRun{{Text: t, Size: 11, Color: "#000000"}}
		return d, nil
	}
	if !bytes.HasPrefix(b, []byte{0xd0, 0xcf, 0x11, 0xe0, 0xa1, 0xb1, 0x1a, 0xe1}) {
		return nil, fmt.Errorf("not an OLE2 DOC or supported HTML DOC")
	}
	streams, err := readCFB(b)
	if err != nil {
		return nil, err
	}
	w := streams["WordDocument"]
	table := streams["0Table"]
	if len(w) > 12 && binary.LittleEndian.Uint16(w[10:12])&0x0200 != 0 {
		table = streams["1Table"]
	}
	if paragraphs, anchored, ok := extractStyledParagraphs(w, table, streams["Data"]); ok {
		target := summaryPageCount(streams["\x05SummaryInformation"])
		d.Pages = layout.Paginate(paragraphs, layout.Options{TargetPages: target})
		attachAnchoredText(d.Pages, anchored, storyLength(w, 76))
		return d, nil
	}
	text := extractPieceTable(w, table)
	if text == "" {
		text = extractWordText(w)
	}
	parts := strings.Split(text, "\f")
	d.Pages = nil
	for i, part := range parts {
		part = strings.TrimSpace(part)
		p := model.Page{Number: i + 1, Text: part}
		if part != "" {
			p.Runs = []model.TextRun{{Text: part, Size: 11, Color: "#000000"}}
		}
		d.Pages = append(d.Pages, p)
	}
	return d, nil
}

func attachAnchoredText(pages []model.Page, anchored []anchoredText, mainLength int) {
	if len(pages) == 0 {
		return
	}
	for _, item := range anchored {
		pageIndex := 0
		if mainLength > 0 {
			pageIndex = item.anchorCP * len(pages) / mainLength
			if pageIndex >= len(pages) {
				pageIndex = len(pages) - 1
			}
		}
		overlay := layout.Paginate(item.paragraphs, layout.Options{TargetPages: 1})
		if len(overlay) == 0 {
			continue
		}
		page := &pages[pageIndex]
		page.Runs = append(overlay[0].Runs, page.Runs...)
		page.Images = append(overlay[0].Images, page.Images...)
		if overlay[0].Text != "" {
			if page.Text != "" {
				page.Text = overlay[0].Text + "\n" + page.Text
			} else {
				page.Text = overlay[0].Text
			}
		}
	}
}

func summaryPageCount(data []byte) int {
	if len(data) < 48 || binary.LittleEndian.Uint16(data[:2]) != 0xfffe {
		return 0
	}
	sections := int(binary.LittleEndian.Uint32(data[24:28]))
	for s := 0; s < sections; s++ {
		entry := 28 + s*20
		if entry+20 > len(data) {
			break
		}
		base := int(binary.LittleEndian.Uint32(data[entry+16 : entry+20]))
		if base < 0 || base+8 > len(data) {
			continue
		}
		count := int(binary.LittleEndian.Uint32(data[base+4 : base+8]))
		for i := 0; i < count; i++ {
			p := base + 8 + i*8
			if p+8 > len(data) {
				break
			}
			id := binary.LittleEndian.Uint32(data[p : p+4])
			off := int(binary.LittleEndian.Uint32(data[p+4 : p+8]))
			v := base + off
			if id == 14 && v+8 <= len(data) && binary.LittleEndian.Uint32(data[v:v+4]) == 3 {
				return int(int32(binary.LittleEndian.Uint32(data[v+4 : v+8])))
			}
		}
	}
	return 0
}

func htmlText(s string) string {
	s = regexp.MustCompile(`(?is)<(?:head|style|script|noscript)\b[^>]*>.*?</(?:head|style|script|noscript)\s*>`).ReplaceAllString(s, "")
	s = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|tr|h[1-6])\s*>`).ReplaceAllString(s, "\n")
	s = regexp.MustCompile(`(?i)</(td|th)\s*>`).ReplaceAllString(s, "\t")
	s = regexp.MustCompile(`(?s)<[^>]+>`).ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	lines := strings.Split(s, "\n")
	out := lines[:0]
	blank := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			if !blank && len(out) > 0 {
				out = append(out, "")
			}
			blank = true
			continue
		}
		out = append(out, line)
		blank = false
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

func extractPieceTable(word, table []byte) string {
	const fcLcbStart = 154
	const clxIndex = 33
	off := fcLcbStart + clxIndex*8
	if len(word) < off+8 {
		return ""
	}
	fc := int(binary.LittleEndian.Uint32(word[off : off+4]))
	l := int(binary.LittleEndian.Uint32(word[off+4 : off+8]))
	if fc < 0 || l < 5 || fc+l > len(table) {
		return ""
	}
	clx := table[fc : fc+l]
	i := 0
	for i < len(clx) && clx[i] == 1 {
		if i+3 > len(clx) {
			return ""
		}
		n := int(binary.LittleEndian.Uint16(clx[i+1 : i+3]))
		i += 3 + n
	}
	if i+5 > len(clx) || clx[i] != 2 {
		return ""
	}
	size := int(binary.LittleEndian.Uint32(clx[i+1 : i+5]))
	i += 5
	if i+size > len(clx) || size < 4 {
		return ""
	}
	plc := clx[i : i+size]
	pieces := (size - 4) / 12
	if pieces <= 0 {
		return ""
	}
	cpBytes := (pieces + 1) * 4
	if cpBytes+pieces*8 > len(plc) {
		return ""
	}
	var out strings.Builder
	for p := 0; p < pieces; p++ {
		cp0 := int(binary.LittleEndian.Uint32(plc[p*4 : p*4+4]))
		cp1 := int(binary.LittleEndian.Uint32(plc[(p+1)*4 : (p+1)*4+4]))
		count := cp1 - cp0
		pcd := cpBytes + p*8
		rawFC := binary.LittleEndian.Uint32(plc[pcd+2 : pcd+6])
		compressed := rawFC&0x40000000 != 0
		pos := int(rawFC & 0x3fffffff)
		if compressed {
			pos /= 2
		}
		if count <= 0 || pos < 0 || pos >= len(word) {
			continue
		}
		if compressed {
			end := pos + count
			if end > len(word) {
				end = len(word)
			}
			for _, c := range word[pos:end] {
				writeDocChar(&out, rune(c))
			}
		} else {
			end := pos + count*2
			if end > len(word) {
				end = len(word)
			}
			var u []uint16
			for j := pos; j+1 < end; j += 2 {
				u = append(u, binary.LittleEndian.Uint16(word[j:j+2]))
			}
			for _, r := range utf16.Decode(u) {
				writeDocChar(&out, r)
			}
		}
	}
	return strings.TrimSpace(out.String())
}
func writeDocChar(out *strings.Builder, r rune) {
	switch r {
	case '\r', '\v':
		out.WriteByte('\n')
	case '\t':
		out.WriteByte('\t')
	case '\f':
		out.WriteByte('\f')
	case 0x07:
		out.WriteByte('\n')
	default:
		if r >= 0x20 {
			out.WriteRune(r)
		}
	}
}

func extractWordText(b []byte) string {
	var out strings.Builder
	// Unicode text runs are common in modern Word documents.
	for i := 0; i+1 < len(b); {
		j := i
		for j+1 < len(b) {
			u := binary.LittleEndian.Uint16(b[j : j+2])
			if !((u >= 0x20 && u <= 0x7e) || u >= 0x300 || u == '\t' || u == '\r' || u == '\n') {
				break
			}
			j += 2
		}
		if j-i >= 8 {
			u := make([]uint16, 0, (j-i)/2)
			for k := i; k < j; k += 2 {
				u = append(u, binary.LittleEndian.Uint16(b[k:k+2]))
			}
			s := string(utf16.Decode(u))
			if strings.TrimSpace(s) != "" {
				out.WriteString(s)
				out.WriteByte('\n')
			}
			i = j
		} else {
			i += 2
		}
	}
	if out.Len() > 0 {
		return strings.TrimSpace(out.String())
	}
	// Fallback for uncompressed ANSI pieces: retain printable ASCII and control breaks.
	for _, c := range b {
		if c == '\r' || c == '\n' {
			out.WriteByte('\n')
		} else if c >= 0x20 && c < 0x7f {
			out.WriteByte(c)
		}
	}
	return strings.TrimSpace(out.String())
}

const (
	freeSect = int32(-1)
	noStream = int32(-2)
	endSect  = int32(-2)
	fatSect  = int32(-3)
	difSect  = int32(-4)
)

func readCFB(b []byte) (map[string][]byte, error) {
	if len(b) < 512 {
		return nil, fmt.Errorf("truncated OLE2")
	}
	secShift := binary.LittleEndian.Uint16(b[30:32])
	if secShift < 9 || secShift > 12 {
		return nil, fmt.Errorf("unsupported sector size shift %d", secShift)
	}
	sectorSize := 1 << secShift
	nFAT := binary.LittleEndian.Uint32(b[44:48])
	firstDir := int32(binary.LittleEndian.Uint32(b[48:52]))
	firstMiniFAT := int32(binary.LittleEndian.Uint32(b[60:64]))
	nMiniFAT := binary.LittleEndian.Uint32(b[64:68])
	firstDIFAT := int32(binary.LittleEndian.Uint32(b[68:72]))
	nDIFAT := binary.LittleEndian.Uint32(b[72:76])
	// Clamp sector counts to what the file can physically contain,
	// so crafted headers cannot cause huge allocations or long loops.
	if maxSectors := uint32(len(b) / sectorSize); true {
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
	getSec := func(id int32) []byte {
		if id < 0 {
			return nil
		}
		off := (int(id) + 1) * sectorSize
		if off < 0 || off+sectorSize > len(b) {
			return nil
		}
		return b[off : off+sectorSize]
	}
	var fatIDs []int32
	for i := 0; i < 109 && len(fatIDs) < int(nFAT); i++ {
		id := int32(binary.LittleEndian.Uint32(b[76+i*4 : 80+i*4]))
		if id >= 0 {
			fatIDs = append(fatIDs, id)
		}
	}
	for id, count := firstDIFAT, uint32(0); id >= 0 && count < nDIFAT && len(fatIDs) < int(nFAT); count++ {
		s := getSec(id)
		if s == nil {
			break
		}
		for i := 0; i < sectorSize-4 && len(fatIDs) < int(nFAT); i += 4 {
			v := int32(binary.LittleEndian.Uint32(s[i : i+4]))
			if v >= 0 {
				fatIDs = append(fatIDs, v)
			}
		}
		id = int32(binary.LittleEndian.Uint32(s[sectorSize-4:]))
	}
	var fat []int32
	for _, id := range fatIDs {
		s := getSec(id)
		if s == nil {
			continue
		}
		for j := 0; j < sectorSize; j += 4 {
			fat = append(fat, int32(binary.LittleEndian.Uint32(s[j:j+4])))
		}
	}
	chain := func(start int32, size int64) []byte {
		var out []byte
		seen := map[int32]bool{}
		for start >= 0 && !seen[start] && int64(len(out)) < size {
			seen[start] = true
			s := getSec(start)
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
	dir := chain(firstDir, 1<<30)
	type ent struct {
		name  string
		start int32
		size  int64
		mini  bool
	}
	var ents []ent
	var rootStart int32 = -1
	var rootSize int64
	for off := 0; off+128 <= len(dir); off += 128 {
		nlen := int(binary.LittleEndian.Uint16(dir[off+64 : off+66]))
		if nlen < 2 {
			continue
		}
		u := make([]uint16, 0, nlen/2-1)
		for i := 0; i < nlen-2 && off+2+i+1 < len(dir); i += 2 {
			u = append(u, binary.LittleEndian.Uint16(dir[off+i:off+i+2]))
		}
		name := string(utf16.Decode(u))
		st := int32(binary.LittleEndian.Uint32(dir[off+116 : off+120]))
		sz := int64(binary.LittleEndian.Uint64(dir[off+120 : off+128]))
		typ := dir[off+66]
		if typ == 5 {
			rootStart = st
			rootSize = sz
			continue
		}
		if typ != 2 {
			continue
		}
		ents = append(ents, ent{name, st, sz, sz < 4096})
	}
	miniStream := chain(rootStart, rootSize)
	miniFat := chain(firstMiniFAT, int64(nMiniFAT)*int64(sectorSize))
	mf := make([]int32, len(miniFat)/4)
	for i := range mf {
		mf[i] = int32(binary.LittleEndian.Uint32(miniFat[i*4 : i*4+4]))
	}
	readMini := func(start int32, size int64) []byte {
		var out []byte
		seen := map[int32]bool{}
		for start >= 0 && !seen[start] && int64(len(out)) < size {
			seen[start] = true
			off := int(start) * 64
			if off+64 > len(miniStream) {
				break
			}
			out = append(out, miniStream[off:off+64]...)
			if int(start) >= len(mf) {
				break
			}
			start = mf[start]
		}
		if int64(len(out)) > size {
			out = out[:size]
		}
		return out
	}
	res := map[string][]byte{}
	for _, e := range ents {
		if e.mini {
			res[e.name] = readMini(e.start, e.size)
		} else {
			res[e.name] = chain(e.start, e.size)
		}
	}
	return res, nil
}
