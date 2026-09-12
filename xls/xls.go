package xls

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"filecontentextractor/internal/officeart"
	"filecontentextractor/internal/ole"
	"filecontentextractor/model"
)

type record struct {
	id     uint16
	offset int
	data   []byte
}
type sheetInfo struct {
	name   string
	offset int
}
type font struct {
	name  string
	size  float64
	color string
}
type cell struct {
	row, col, xf int
	value        string
}

func ParseFile(name string) (*model.Document, error) {
	raw, e := os.ReadFile(name)
	if e != nil {
		return nil, e
	}
	streams, e := ole.Read(raw)
	if e != nil {
		return nil, e
	}
	book := streams["Workbook"]
	if len(book) == 0 {
		book = streams["Book"]
	}
	if len(book) == 0 {
		return nil, fmt.Errorf("XLS Workbook stream not found")
	}
	records := records(book)
	var sheets []sheetInfo
	var shared []string
	var fonts []font
	var xfs []int
	for _, r := range records {
		switch r.id {
		case 0x0085:
			if s, ok := boundSheet(r.data); ok {
				sheets = append(sheets, s)
			}
		case 0x00FC:
			shared = parseSST(r.data)
		case 0x0031:
			if len(fonts) == 4 {
				fonts = append(fonts, font{size: 11, color: "#000000"})
			} // BIFF reserves font index 4.
			fonts = append(fonts, parseFont(r.data))
		case 0x00E0:
			if len(r.data) >= 2 {
				xfs = append(xfs, int(binary.LittleEndian.Uint16(r.data[:2])))
			}
		}
	}
	sort.SliceStable(sheets, func(i, j int) bool { return sheets[i].offset < sheets[j].offset })
	d := &model.Document{Path: name, Format: "xls", Pagination: "worksheet"}
	for i, s := range sheets {
		end := len(book)
		if i+1 < len(sheets) {
			end = sheets[i+1].offset
		}
		cells, images := parseSheet(book, s.offset, end, shared)
		p := model.Page{Number: i + 1, Name: s.name}
		maxCol := 0
		byRow := map[int][]cell{}
		var rows []int
		for _, c := range cells {
			if _, ok := byRow[c.row]; !ok {
				rows = append(rows, c.row)
			}
			byRow[c.row] = append(byRow[c.row], c)
			if c.col > maxCol {
				maxCol = c.col
			}
			style := font{size: 11, color: "#000000"}
			if len(fonts) > 0 {
				style = fonts[0]
			}
			if c.xf >= 0 && c.xf < len(xfs) && xfs[c.xf] >= 0 && xfs[c.xf] < len(fonts) {
				style = fonts[xfs[c.xf]]
			}
			p.Runs = append(p.Runs, model.TextRun{Text: c.value, Font: style.name, Size: style.size, Color: style.color, Bounds: model.Rect{X: float64(c.col), Y: float64(c.row), Width: 1, Height: 1}})
		}
		sort.Ints(rows)
		for _, row := range rows {
			values := make([]string, maxCol+1)
			for _, c := range byRow[row] {
				values[c.col] = c.value
			}
			line := strings.TrimRight(strings.Join(values, "\t"), "\t")
			if line != "" {
				if p.Text != "" {
					p.Text += "\n"
				}
				p.Text += line
			}
		}
		p.Images = images
		d.Pages = append(d.Pages, p)
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("XLS contains no worksheets")
	}
	return d, nil
}

func records(data []byte) []record {
	var out []record
	for pos := 0; pos+4 <= len(data); {
		id := binary.LittleEndian.Uint16(data[pos : pos+2])
		n := int(binary.LittleEndian.Uint16(data[pos+2 : pos+4]))
		if pos+4+n > len(data) {
			break
		}
		out = append(out, record{id: id, offset: pos, data: data[pos+4 : pos+4+n]})
		pos += 4 + n
	}
	return out
}
func boundSheet(b []byte) (sheetInfo, bool) {
	if len(b) < 8 {
		return sheetInfo{}, false
	}
	s := sheetInfo{offset: int(binary.LittleEndian.Uint32(b[:4]))}
	n := int(b[6])
	wide := b[7]&1 != 0
	if wide {
		if 8+n*2 > len(b) {
			return sheetInfo{}, false
		}
		u := make([]uint16, n)
		for i := range u {
			u[i] = binary.LittleEndian.Uint16(b[8+i*2:])
		}
		s.name = string(utf16.Decode(u))
	} else {
		if 8+n > len(b) {
			return sheetInfo{}, false
		}
		s.name = string(b[8 : 8+n])
	}
	return s, true
}
func parseSST(b []byte) []string {
	if len(b) < 8 {
		return nil
	}
	unique := int(binary.LittleEndian.Uint32(b[4:8]))
	// Each string header costs at least 3 bytes; clamp the declared
	// count so a corrupt SST cannot trigger a huge allocation.
	if max := (len(b) - 8) / 3; unique > max {
		unique = max
	}
	pos := 8
	out := make([]string, 0, unique)
	for len(out) < unique && pos+3 <= len(b) {
		n := int(binary.LittleEndian.Uint16(b[pos : pos+2]))
		flags := b[pos+2]
		pos += 3
		rich, ext := 0, 0
		if flags&8 != 0 {
			if pos+2 > len(b) {
				break
			}
			rich = int(binary.LittleEndian.Uint16(b[pos : pos+2]))
			pos += 2
		}
		if flags&4 != 0 {
			if pos+4 > len(b) {
				break
			}
			ext = int(binary.LittleEndian.Uint32(b[pos : pos+4]))
			pos += 4
		}
		wide := flags&1 != 0
		bytesN := n
		if wide {
			bytesN *= 2
		}
		if pos+bytesN > len(b) {
			break
		}
		out = append(out, decodeXLString(b[pos:pos+bytesN], wide))
		pos += bytesN + rich*4 + ext
	}
	return out
}
func decodeXLString(b []byte, wide bool) string {
	if !wide {
		return string(b)
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, binary.LittleEndian.Uint16(b[i:i+2]))
	}
	return string(utf16.Decode(u))
}
func parseFont(b []byte) font {
	f := font{size: 11, color: "#000000"}
	if len(b) < 16 {
		return f
	}
	f.size = float64(binary.LittleEndian.Uint16(b[:2])) / 20
	f.color = xlsColor(int(binary.LittleEndian.Uint16(b[4:6])))
	n := int(b[14])
	wide := b[15]&1 != 0
	start := 16
	bytesN := n
	if wide {
		bytesN *= 2
	}
	if start+bytesN <= len(b) {
		f.name = decodeXLString(b[start:start+bytesN], wide)
	}
	return f
}
func parseSheet(book []byte, start, end int, shared []string) ([]cell, []model.Image) {
	if start < 0 || end < start || start >= len(book) || end > len(book) {
		return nil, nil
	}
	var cells []cell
	var drawing []byte
	for _, r := range records(book[start:end]) {
		b := r.data
		switch r.id {
		case 0x00FD:
			if len(b) >= 10 {
				idx := int(binary.LittleEndian.Uint32(b[6:10]))
				v := ""
				if idx >= 0 && idx < len(shared) {
					v = shared[idx]
				}
				cells = append(cells, newCell(b, v))
			}
		case 0x0204:
			if len(b) >= 9 {
				n := int(binary.LittleEndian.Uint16(b[6:8]))
				flags := b[8]
				wide := flags&1 != 0
				size := n
				if wide {
					size *= 2
				}
				if 9+size <= len(b) {
					cells = append(cells, newCell(b, decodeXLString(b[9:9+size], wide)))
				}
			}
		case 0x0203:
			if len(b) >= 14 {
				cells = append(cells, newCell(b, strconv.FormatFloat(math.Float64frombits(binary.LittleEndian.Uint64(b[6:14])), 'g', -1, 64)))
			}
		case 0x027E:
			if len(b) >= 10 {
				cells = append(cells, newCell(b, strconv.FormatFloat(decodeRK(binary.LittleEndian.Uint32(b[6:10])), 'g', -1, 64)))
			}
		case 0x0205:
			if len(b) >= 8 {
				v := "FALSE"
				if b[6] != 0 {
					v = "TRUE"
				}
				cells = append(cells, newCell(b, v))
			}
		case 0x00EC, 0x00EB:
			drawing = append(drawing, b...)
		}
	}
	return cells, officeart.Images(drawing)
}
func newCell(b []byte, value string) cell {
	return cell{row: int(binary.LittleEndian.Uint16(b[:2])), col: int(binary.LittleEndian.Uint16(b[2:4])), xf: int(binary.LittleEndian.Uint16(b[4:6])), value: value}
}
func decodeRK(v uint32) float64 {
	var n float64
	if v&2 != 0 {
		n = float64(int32(v) >> 2)
	} else {
		n = math.Float64frombits(uint64(v&^3) << 32)
	}
	if v&1 != 0 {
		n /= 100
	}
	return n
}
func xlsColor(i int) string {
	colors := []string{"#000000", "#FFFFFF", "#FF0000", "#00FF00", "#0000FF", "#FFFF00", "#FF00FF", "#00FFFF"}
	if i >= 8 && i < 16 {
		return colors[i-8]
	}
	return "#000000"
}
