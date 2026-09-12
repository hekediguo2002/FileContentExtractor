package doc

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/hekediguo2002/FileContentExtractor/layout"
	"github.com/hekediguo2002/FileContentExtractor/model"
)

type textPiece struct {
	cpStart, cpEnd int
	fc             int
	compressed     bool
}

type charStyle struct {
	font      string
	size      float64
	color     string
	picOffset int
}

type formatRun struct {
	fcStart, fcEnd int
	style          charStyle
}

type styledChar struct {
	r     rune
	style charStyle
}

type anchoredText struct {
	paragraphs []layout.Paragraph
	anchorCP   int
}

func extractStyledParagraphs(word, table, data []byte) ([]layout.Paragraph, []anchoredText, bool) {
	pieces, ok := parsePieceTable(word, table)
	if !ok {
		return nil, nil, false
	}
	fonts := parseFontTable(fibData(word, table, 15))
	formats := parseCharacterFormats(word, fibData(word, table, 12), fonts)
	chars := decodeStyledChars(word, pieces, formats)
	if len(chars) == 0 {
		return nil, nil, false
	}
	mainLength := storyLength(word, 76)
	if mainLength <= 0 || mainLength > len(chars) {
		return buildStyledParagraphs(chars, data), nil, true
	}
	body := buildStyledParagraphs(chars[:mainLength], data)
	textBoxStart := mainLength
	for offset := 80; offset <= 96; offset += 4 {
		textBoxStart += storyLength(word, offset)
	}
	textBoxLength := storyLength(word, 100)
	if textBoxLength <= 0 || textBoxStart < 0 || textBoxStart+textBoxLength > len(chars) {
		return body, nil, true
	}
	paragraphs := buildStyledParagraphs(chars[textBoxStart:textBoxStart+textBoxLength], data)
	if len(paragraphs) == 0 {
		return body, nil, true
	}
	anchor := firstDrawingAnchor(fibData(word, table, 40), mainLength)
	return body, []anchoredText{{paragraphs: paragraphs, anchorCP: anchor}}, true
}

func storyLength(word []byte, offset int) int {
	if offset < 0 || offset+4 > len(word) {
		return 0
	}
	return int(binary.LittleEndian.Uint32(word[offset : offset+4]))
}

func firstDrawingAnchor(plc []byte, mainLength int) int {
	// PlcSpaMom stores (n+1) CP values followed by n 26-byte SPA records.
	count := (len(plc) - 4) / 30
	if count <= 0 || (count+1)*4+count*26 > len(plc) {
		return 0
	}
	anchor := mainLength
	for i := 0; i < count; i++ {
		cp := int(binary.LittleEndian.Uint32(plc[i*4 : i*4+4]))
		if cp >= 0 && cp < anchor {
			anchor = cp
		}
	}
	if anchor >= mainLength {
		return 0
	}
	return anchor
}

func fibData(word, table []byte, index int) []byte {
	off := 154 + index*8
	if off+8 > len(word) {
		return nil
	}
	fc := int(binary.LittleEndian.Uint32(word[off : off+4]))
	lcb := int(binary.LittleEndian.Uint32(word[off+4 : off+8]))
	if fc < 0 || lcb < 0 || fc+lcb > len(table) {
		return nil
	}
	return table[fc : fc+lcb]
}

func parsePieceTable(word, table []byte) ([]textPiece, bool) {
	clx := fibData(word, table, 33)
	i := 0
	for i < len(clx) && clx[i] == 1 {
		if i+3 > len(clx) {
			return nil, false
		}
		size := int(binary.LittleEndian.Uint16(clx[i+1 : i+3]))
		i += 3 + size
	}
	if i+5 > len(clx) || clx[i] != 2 {
		return nil, false
	}
	size := int(binary.LittleEndian.Uint32(clx[i+1 : i+5]))
	i += 5
	if size < 4 || i+size > len(clx) {
		return nil, false
	}
	plc := clx[i : i+size]
	count := (size - 4) / 12
	cpBytes := (count + 1) * 4
	if count <= 0 || cpBytes+count*8 > len(plc) {
		return nil, false
	}
	pieces := make([]textPiece, 0, count)
	for p := 0; p < count; p++ {
		cpStart := int(binary.LittleEndian.Uint32(plc[p*4 : p*4+4]))
		cpEnd := int(binary.LittleEndian.Uint32(plc[(p+1)*4 : (p+1)*4+4]))
		pcd := cpBytes + p*8
		rawFC := binary.LittleEndian.Uint32(plc[pcd+2 : pcd+6])
		compressed := rawFC&0x40000000 != 0
		fc := int(rawFC & 0x3fffffff)
		if compressed {
			fc /= 2
		}
		pieces = append(pieces, textPiece{cpStart: cpStart, cpEnd: cpEnd, fc: fc, compressed: compressed})
	}
	return pieces, true
}

func parseCharacterFormats(word, plc []byte, fonts []string) []formatRun {
	if len(plc) < 4 || (len(plc)-4)%8 != 0 {
		return nil
	}
	count := (len(plc) - 4) / 8
	fcBytes := (count + 1) * 4
	if fcBytes+count*4 > len(plc) {
		return nil
	}
	var runs []formatRun
	for i := 0; i < count; i++ {
		pn := int(binary.LittleEndian.Uint32(plc[fcBytes+i*4 : fcBytes+i*4+4]))
		off := pn * 512
		if off < 0 || off+512 > len(word) {
			continue
		}
		page := word[off : off+512]
		crun := int(page[511])
		base := (crun + 1) * 4
		if crun == 0 || base+crun > 511 {
			continue
		}
		for j := 0; j < crun; j++ {
			fcStart := int(binary.LittleEndian.Uint32(page[j*4 : j*4+4]))
			fcEnd := int(binary.LittleEndian.Uint32(page[(j+1)*4 : (j+1)*4+4]))
			pos := int(page[base+j]) * 2
			style := defaultCharStyle()
			if pos > 0 && pos < 511 {
				length := int(page[pos])
				if pos+1+length <= 511 {
					style = parseSprms(page[pos+1:pos+1+length], fonts, style)
				}
			}
			runs = append(runs, formatRun{fcStart: fcStart, fcEnd: fcEnd, style: style})
		}
	}
	sort.SliceStable(runs, func(i, j int) bool { return runs[i].fcStart < runs[j].fcStart })
	return runs
}

func parseSprms(data []byte, fonts []string, style charStyle) charStyle {
	fontASCII, fontEastAsia := -1, -1
	for i := 0; i+2 <= len(data); {
		op := binary.LittleEndian.Uint16(data[i : i+2])
		i += 2
		length, prefix := sprmOperandSize(op, data[i:])
		if length < 0 || i+prefix+length > len(data) {
			break
		}
		operand := data[i+prefix : i+prefix+length]
		switch op {
		case 0x4A43: // sprmCHps, half-points
			if len(operand) >= 2 {
				style.size = float64(binary.LittleEndian.Uint16(operand)) / 2
			}
		case 0x4A4F: // sprmCRgFtc0 / ASCII font
			if len(operand) >= 2 {
				fontASCII = int(binary.LittleEndian.Uint16(operand))
			}
		case 0x4A50: // sprmCRgFtc1 / East Asian font
			if len(operand) >= 2 {
				fontEastAsia = int(binary.LittleEndian.Uint16(operand))
			}
		case 0x2A42: // sprmCIco, legacy color index
			if len(operand) >= 1 {
				style.color = indexedColor(operand[0])
			}
		case 0x6870: // sprmCCv, COLORREF (R, G, B, flags)
			if len(operand) >= 4 && binary.LittleEndian.Uint32(operand) != 0xFFFFFFFF {
				style.color = fmt.Sprintf("#%02X%02X%02X", operand[0], operand[1], operand[2])
			}
		case 0x6A03: // sprmCPicLocation, offset in the Data stream
			if len(operand) >= 4 {
				style.picOffset = int(binary.LittleEndian.Uint32(operand))
			}
		}
		i += prefix + length
	}
	fontIndex := fontEastAsia
	if fontIndex < 0 {
		fontIndex = fontASCII
	}
	if fontIndex >= 0 && fontIndex < len(fonts) {
		style.font = fonts[fontIndex]
	}
	return style
}

func sprmOperandSize(op uint16, remaining []byte) (length, prefix int) {
	switch op >> 13 {
	case 0, 1:
		return 1, 0
	case 2, 4, 5:
		return 2, 0
	case 3:
		return 4, 0
	case 7:
		return 3, 0
	case 6:
		if len(remaining) < 1 {
			return -1, 0
		}
		return int(remaining[0]), 1
	default:
		return -1, 0
	}
}

func parseFontTable(data []byte) []string {
	if len(data) < 4 {
		return nil
	}
	count := int(binary.LittleEndian.Uint16(data[0:2]))
	pos := 4
	fonts := make([]string, 0, count)
	for pos < len(data) && len(fonts) < count {
		entryLen := int(data[pos]) + 1
		end := pos + entryLen
		if entryLen < 40 || end > len(data) {
			break
		}
		nameStart := pos + 40
		var units []uint16
		for p := nameStart; p+1 < end; p += 2 {
			u := binary.LittleEndian.Uint16(data[p : p+2])
			if u == 0 {
				break
			}
			units = append(units, u)
		}
		fonts = append(fonts, string(utf16.Decode(units)))
		pos = end
	}
	return fonts
}

func decodeStyledChars(word []byte, pieces []textPiece, formats []formatRun) []styledChar {
	var result []styledChar
	formatIndex := 0
	for _, piece := range pieces {
		count := piece.cpEnd - piece.cpStart
		for i := 0; i < count; i++ {
			fc := piece.fc + i
			var r rune
			if piece.compressed {
				if fc >= len(word) {
					break
				}
				r = rune(word[fc])
			} else {
				fc = piece.fc + i*2
				if fc+2 > len(word) {
					break
				}
				r = rune(binary.LittleEndian.Uint16(word[fc : fc+2]))
			}
			for formatIndex+1 < len(formats) && fc >= formats[formatIndex].fcEnd {
				formatIndex++
			}
			style := defaultCharStyle()
			if formatIndex < len(formats) && fc >= formats[formatIndex].fcStart && fc < formats[formatIndex].fcEnd {
				style = formats[formatIndex].style
			}
			result = append(result, styledChar{r: r, style: style})
		}
	}
	return result
}

func buildStyledParagraphs(chars []styledChar, data []byte) []layout.Paragraph {
	var paragraphs []layout.Paragraph
	currentParagraph := layout.Paragraph{}
	var run strings.Builder
	current := charStyle{}
	haveStyle := false
	flush := func() {
		if run.Len() == 0 {
			return
		}
		text := run.String()
		currentParagraph.Runs = append(currentParagraph.Runs, model.TextRun{Text: text, Font: current.font, Size: current.size, Color: current.color})
		run.Reset()
	}
	flushParagraph := func() {
		flush()
		paragraphs = append(paragraphs, currentParagraph)
		currentParagraph = layout.Paragraph{}
		haveStyle = false
	}
	for _, ch := range chars {
		if ch.r == '\f' {
			flushParagraph()
			paragraphs[len(paragraphs)-1].BreakAfter = true
			continue
		}
		if ch.r == '\r' || ch.r == '\v' || ch.r == 0x07 {
			flushParagraph()
			continue
		}
		if ch.r == 1 {
			flush()
			if picture, ok := extractPictureAt(data, ch.style.picOffset); ok {
				currentParagraph.Images = append(currentParagraph.Images, picture)
			}
			continue
		}
		r, keep := normalizedDocRune(ch.r)
		if !keep {
			continue
		}
		if !haveStyle || ch.style != current {
			flush()
			current = ch.style
			haveStyle = true
		}
		run.WriteRune(r)
	}
	flush()
	if len(currentParagraph.Runs) > 0 || len(currentParagraph.Images) > 0 {
		paragraphs = append(paragraphs, currentParagraph)
	}
	return paragraphs
}

func normalizedDocRune(r rune) (rune, bool) {
	switch r {
	case '\r', '\v', 0x07:
		return '\n', true
	case '\t':
		return '\t', true
	default:
		return r, r >= 0x20
	}
}

func indexedColor(index byte) string {
	colors := [...]string{"#000000", "#000000", "#0000FF", "#00FFFF", "#00FF00", "#FF00FF", "#FF0000", "#FFFF00", "#FFFFFF", "#000080", "#008080", "#008000", "#800080", "#800000", "#808000", "#808080", "#C0C0C0"}
	if int(index) < len(colors) {
		return colors[index]
	}
	return "#000000"
}

func defaultCharStyle() charStyle {
	return charStyle{size: 11, color: "#000000", picOffset: -1}
}

func extractPictureAt(data []byte, offset int) (model.Image, bool) {
	if offset < 0 || offset+4 > len(data) {
		return model.Image{}, false
	}
	end := len(data)
	if size := int(binary.LittleEndian.Uint32(data[offset : offset+4])); size > 0 && offset+size <= len(data) {
		end = offset + size
	}
	for pos := offset; pos+25 <= end; pos++ {
		id := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
		format := ""
		switch id {
		case 0xF01D:
			format = "jpg"
		case 0xF01E:
			format = "png"
		case 0xF01F:
			format = "dib"
		case 0xF029:
			format = "tiff"
		default:
			continue
		}
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		start, stop := pos+25, pos+8+size
		if size < 17 || stop > end || start > stop {
			continue
		}
		raw := append([]byte(nil), data[start:stop]...)
		picture := model.Image{Name: fmt.Sprintf("doc-image-%d", offset), Format: format, Data: raw}
		if config, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil {
			picture.Width, picture.Height = config.Width, config.Height
		}
		return picture, true
	}
	return model.Image{}, false
}
