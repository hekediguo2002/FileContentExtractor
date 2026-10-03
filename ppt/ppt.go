package ppt

import (
	"encoding/binary"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode/utf16"

	"github.com/hekediguo2002/FileContentExtractor/internal/officeart"
	"github.com/hekediguo2002/FileContentExtractor/internal/ole"
	"github.com/hekediguo2002/FileContentExtractor/model"
)

type record struct {
	instance, version uint16
	kind              uint16
	data              []byte
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
	content := streams["PowerPoint Document"]
	if len(content) == 0 {
		return nil, fmt.Errorf("PowerPoint Document stream not found")
	}
	d := &model.Document{Path: name, Format: "ppt", Pagination: "slide"}
	width, height := 720.0, 540.0
	// Container records nest; cap recursion depth so a crafted file with
	// deeply nested containers cannot exhaust the goroutine stack.
	const maxDepth = 200
	var walk func([]byte, int)
	walk = func(data []byte, depth int) {
		if depth > maxDepth {
			return
		}
		for _, r := range parseRecords(data) {
			if r.kind == 1001 && len(r.data) >= 8 {
				width = float64(int32(binary.LittleEndian.Uint32(r.data[:4]))) / 8
				height = float64(int32(binary.LittleEndian.Uint32(r.data[4:8]))) / 8
			}
			if r.kind == 4080 && r.instance == 0 {
				parseSlideList(r.data, d, width, height)
			} else if r.version == 0xf {
				walk(r.data, depth+1)
			}
		}
	}
	walk(content, 0)
	if slides := scanSlides(content, width, height); len(slides) > 0 {
		if len(d.Pages) == 0 {
			d.Pages = slides
		} else if len(slides) >= len(d.Pages) {
			// SlideListWithText is the authoritative page order for legacy PPT.
			// Slide containers may include an earlier master/deleted slide and
			// usually hold only text local to shapes. Align the current tail and
			// merge that local content instead of replacing the complete list.
			slides = slides[len(slides)-len(d.Pages):]
			for i := range d.Pages {
				mergeSlideContent(&d.Pages[i], slides[i])
			}
		}
	}
	if len(d.Pages) == 0 {
		p := model.Page{Number: 1, Name: "Slide 1", Width: width, Height: height}
		collectText(content, &p)
		if p.Text != "" {
			d.Pages = append(d.Pages, p)
		}
	}
	pictures := officeart.Images(streams["Pictures"])
	if len(d.Pages) > 0 {
		d.Pages[0].Images = append(d.Pages[0].Images, pictures...)
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("PPT contains no slides or text")
	}
	return d, nil
}

func mergeSlideContent(page *model.Page, extra model.Page) {
	for _, run := range extra.Runs {
		value := strings.TrimSpace(run.Text)
		if value == "" || strings.Contains(page.Text, value) {
			continue
		}
		appendText(page, value)
	}
	page.Images = append(page.Images, extra.Images...)
	for _, table := range extra.Tables {
		page.Tables = append(page.Tables, table)
		mergeTableText(page, table)
	}
}

func scanSlides(data []byte, width, height float64) []model.Page {
	var pages []model.Page
	for pos := 0; pos+8 <= len(data); pos++ {
		if binary.LittleEndian.Uint16(data[pos+2:pos+4]) != 1006 {
			continue
		}
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if size <= 0 || pos+8+size > len(data) {
			continue
		}
		payload := data[pos+8 : pos+8+size]
		page := model.Page{Number: len(pages) + 1, Name: fmt.Sprintf("Slide %d", len(pages)+1), Width: width, Height: height}
		collectText(payload, &page)
		page.Images = officeart.Images(payload)
		page.Tables = parseLegacyTables(payload)
		for _, table := range page.Tables {
			mergeTableText(&page, table)
		}
		pages = append(pages, page)
		pos += 7 + size
	}
	return pages
}

func mergeTableText(page *model.Page, table model.Table) {
	structured := model.TableTSV(table)
	if structured == "" {
		return
	}
	var values []string
	for _, row := range table.Rows {
		for _, cell := range row.Cells {
			values = append(values, strings.TrimSpace(cell.Text))
		}
	}
	linear := strings.Join(values, "\n")
	if linear != "" && strings.Contains(page.Text, linear) {
		page.Text = strings.Replace(page.Text, linear, structured, 1)
		return
	}
	if !strings.Contains(page.Text, structured) {
		appendText(page, structured)
	}
}

type legacyTableCell struct {
	bounds model.Rect
	text   string
	runs   []model.TextRun
}

func parseLegacyTables(data []byte) []model.Table {
	var tables []model.Table
	var walk func([]byte, int)
	walk = func(payload []byte, depth int) {
		if depth > 200 {
			return
		}
		for _, item := range parseRecords(payload) {
			if item.kind == 0xF003 {
				if table, ok := legacyTableFromGroup(item.data); ok {
					tables = append(tables, table)
					continue
				}
			}
			if item.version == 0xf {
				walk(item.data, depth+1)
			}
		}
	}
	walk(data, 0)
	return tables
}

func legacyTableFromGroup(data []byte) (model.Table, bool) {
	children := parseRecords(data)
	if len(children) < 2 || children[0].kind != 0xF004 || !legacyTableMarker(children[0].data) {
		return model.Table{}, false
	}
	var cells []legacyTableCell
	for _, child := range children[1:] {
		if child.kind != 0xF004 {
			continue
		}
		anchor, hasAnchor := legacyClientAnchor(child.data)
		if !hasAnchor || !containsRecordKind(child.data, 0xF00D) {
			continue
		}
		page := model.Page{}
		collectText(child.data, &page)
		for i := range page.Runs {
			page.Runs[i].Bounds = anchor
		}
		cells = append(cells, legacyTableCell{bounds: anchor, text: page.Text, runs: page.Runs})
	}
	if len(cells) == 0 {
		return model.Table{}, false
	}

	var columns, rows []float64
	for _, cell := range cells {
		columns = appendUniqueCoordinate(columns, cell.bounds.X)
		rows = appendUniqueCoordinate(rows, cell.bounds.Y)
	}
	sort.Float64s(columns)
	sort.Float64s(rows)
	table := model.Table{}
	for _, cell := range cells {
		column := coordinateIndex(columns, cell.bounds.X)
		row := coordinateIndex(rows, cell.bounds.Y)
		if column < 0 || row < 0 {
			continue
		}
		columnSpan := legacySpan(columns, column, cell.bounds.Width)
		rowSpan := legacySpan(rows, row, cell.bounds.Height)
		for len(table.Rows) <= row {
			table.Rows = append(table.Rows, model.TableRow{})
		}
		table.Rows[row].Cells = append(table.Rows[row].Cells, model.TableCell{
			Row: row, Column: column, RowSpan: rowSpan, ColSpan: columnSpan,
			Bounds: cell.bounds, Text: cell.text, Runs: cell.runs,
		})
		if table.Bounds.Width == 0 && table.Bounds.Height == 0 {
			table.Bounds = cell.bounds
		} else {
			table.Bounds = unionRect(table.Bounds, cell.bounds)
		}
	}
	for row := range table.Rows {
		sort.Slice(table.Rows[row].Cells, func(i, j int) bool {
			return table.Rows[row].Cells[i].Column < table.Rows[row].Cells[j].Column
		})
	}
	return table, len(table.Rows) > 0
}

func legacyTableMarker(data []byte) bool {
	return legacyTableMarkerDepth(data, 0)
}

func legacyTableMarkerDepth(data []byte, depth int) bool {
	if depth > 200 {
		return false
	}
	for _, item := range parseRecords(data) {
		if item.kind == 0xF122 {
			count := int(item.instance)
			for index := 0; index < count && index*6+6 <= len(item.data); index++ {
				offset := index * 6
				property := binary.LittleEndian.Uint16(item.data[offset:offset+2]) & 0x3fff
				value := binary.LittleEndian.Uint32(item.data[offset+2 : offset+6])
				if property == 0x039f && value&1 != 0 {
					return true
				}
			}
		}
		if item.version == 0xf && legacyTableMarkerDepth(item.data, depth+1) {
			return true
		}
	}
	return false
}

func legacyClientAnchor(data []byte) (model.Rect, bool) {
	return legacyClientAnchorDepth(data, 0)
}

func legacyClientAnchorDepth(data []byte, depth int) (model.Rect, bool) {
	if depth > 200 {
		return model.Rect{}, false
	}
	for _, item := range parseRecords(data) {
		if item.kind == 0xF00F && len(item.data) >= 16 {
			left := float64(int32(binary.LittleEndian.Uint32(item.data[0:4]))) / 8
			top := float64(int32(binary.LittleEndian.Uint32(item.data[4:8]))) / 8
			right := float64(int32(binary.LittleEndian.Uint32(item.data[8:12]))) / 8
			bottom := float64(int32(binary.LittleEndian.Uint32(item.data[12:16]))) / 8
			return model.Rect{X: left, Y: top, Width: right - left, Height: bottom - top}, right > left && bottom > top
		}
		if item.version == 0xf {
			if anchor, ok := legacyClientAnchorDepth(item.data, depth+1); ok {
				return anchor, true
			}
		}
	}
	return model.Rect{}, false
}

func containsRecordKind(data []byte, kind uint16) bool {
	return containsRecordKindDepth(data, kind, 0)
}

func containsRecordKindDepth(data []byte, kind uint16, depth int) bool {
	if depth > 200 {
		return false
	}
	for _, item := range parseRecords(data) {
		if item.kind == kind {
			return true
		}
		if item.version == 0xf && containsRecordKindDepth(item.data, kind, depth+1) {
			return true
		}
	}
	return false
}

func appendUniqueCoordinate(values []float64, value float64) []float64 {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func coordinateIndex(values []float64, value float64) int {
	for index, existing := range values {
		if existing == value {
			return index
		}
	}
	return -1
}

func legacySpan(starts []float64, index int, size float64) int {
	span := 1
	end := starts[index] + size
	for next := index + 1; next < len(starts) && starts[next] < end; next++ {
		span++
	}
	return span
}

func unionRect(a, b model.Rect) model.Rect {
	left := a.X
	if b.X < left {
		left = b.X
	}
	top := a.Y
	if b.Y < top {
		top = b.Y
	}
	right := a.X + a.Width
	if candidate := b.X + b.Width; candidate > right {
		right = candidate
	}
	bottom := a.Y + a.Height
	if candidate := b.Y + b.Height; candidate > bottom {
		bottom = candidate
	}
	return model.Rect{X: left, Y: top, Width: right - left, Height: bottom - top}
}

func parseRecords(data []byte) []record {
	var out []record
	for pos := 0; pos+8 <= len(data); {
		options := binary.LittleEndian.Uint16(data[pos : pos+2])
		kind := binary.LittleEndian.Uint16(data[pos+2 : pos+4])
		size := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		if size < 0 || pos+8+size > len(data) {
			break
		}
		r := record{instance: options >> 4, version: options & 0xf, kind: kind, data: data[pos+8 : pos+8+size]}
		out = append(out, r)
		pos += 8 + size
	}
	return out
}
func parseSlideList(data []byte, d *model.Document, width, height float64) {
	var page *model.Page
	for _, r := range parseRecords(data) {
		if r.kind == 1011 {
			d.Pages = append(d.Pages, model.Page{Number: len(d.Pages) + 1, Name: fmt.Sprintf("Slide %d", len(d.Pages)+1), Width: width, Height: height})
			page = &d.Pages[len(d.Pages)-1]
			continue
		}
		if page == nil {
			continue
		}
		value := ""
		switch r.kind {
		case 4000:
			value = utf16Text(r.data)
		case 4008:
			value = string(r.data)
		}
		appendText(page, value)
	}
}
func collectText(data []byte, page *model.Page) {
	collectTextDepth(data, page, 0)
}

func collectTextDepth(data []byte, page *model.Page, depth int) {
	if depth > 200 {
		return
	}
	for _, r := range parseRecords(data) {
		switch r.kind {
		case 4000:
			appendText(page, utf16Text(r.data))
		case 4008:
			appendText(page, string(r.data))
		}
		if r.version == 0xf {
			collectTextDepth(r.data, page, depth+1)
		}
	}
}
func appendText(page *model.Page, value string) {
	value = strings.TrimSpace(strings.NewReplacer("\r", "\n", "\v", "\n").Replace(value))
	if value == "" {
		return
	}
	if page.Text != "" {
		page.Text += "\n"
	}
	page.Text += value
	page.Runs = append(page.Runs, model.TextRun{Text: value, Size: 18, Color: "#000000"})
}
func utf16Text(data []byte) string {
	units := make([]uint16, 0, len(data)/2)
	for i := 0; i+1 < len(data); i += 2 {
		units = append(units, binary.LittleEndian.Uint16(data[i:i+2]))
	}
	return string(utf16.Decode(units))
}
