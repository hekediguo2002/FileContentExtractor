package pptx

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"path"
	"strconv"
	"strings"

	"github.com/hekediguo2002/FileContentExtractor/model"
)

type node struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []node     `xml:",any"`
	Text    string     `xml:",chardata"`
}

func attr(n node, name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func find(n node, name string) []node {
	var out []node
	for _, c := range n.Nodes {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
		out = append(out, find(c, name)...)
	}
	return out
}
func direct(n node, name string) (node, bool) {
	for _, c := range n.Nodes {
		if c.XMLName.Local == name {
			return c, true
		}
	}
	return node{}, false
}
func parse(data []byte) (node, error) { var n node; e := xml.Unmarshal(data, &n); return n, e }
func archive(name string) (map[string][]byte, error) {
	z, e := zip.OpenReader(name)
	if e != nil {
		return nil, e
	}
	defer z.Close()
	a := map[string][]byte{}
	const maxEntrySize = 512 << 20 // decompressed size limit per entry, guards against zip bombs
	for _, f := range z.File {
		if f.UncompressedSize64 > maxEntrySize {
			return nil, fmt.Errorf("pptx entry %s too large", f.Name)
		}
		r, e := f.Open()
		if e != nil {
			return nil, e
		}
		a[f.Name], e = io.ReadAll(io.LimitReader(r, maxEntrySize))
		r.Close()
		if e != nil {
			return nil, e
		}
	}
	return a, nil
}
func rels(data []byte, base string) map[string]string {
	out := map[string]string{}
	n, e := parse(data)
	if e != nil {
		return out
	}
	for _, r := range find(n, "Relationship") {
		target := attr(r, "Target")
		if strings.HasPrefix(target, "/") {
			out[attr(r, "Id")] = path.Clean(strings.TrimPrefix(target, "/"))
		} else {
			out[attr(r, "Id")] = path.Clean(path.Join(base, target))
		}
	}
	return out
}

func ParseFile(name string) (*model.Document, error) {
	a, e := archive(name)
	if e != nil {
		return nil, e
	}
	presentation, e := parse(a["ppt/presentation.xml"])
	if e != nil {
		return nil, e
	}
	relationships := rels(a["ppt/_rels/presentation.xml.rels"], "ppt")
	width, height := 720.0, 540.0
	if sizes := find(presentation, "sldSz"); len(sizes) > 0 {
		width = emu(attr(sizes[0], "cx"))
		height = emu(attr(sizes[0], "cy"))
	}
	d := &model.Document{Path: name, Format: "pptx", Pagination: "slide"}
	for _, id := range find(presentation, "sldId") {
		slidePath := relationships[relationshipID(id)]
		if slidePath == "" {
			continue
		}
		slide, e := parse(a[slidePath])
		if e != nil {
			return nil, fmt.Errorf("parse %s: %w", slidePath, e)
		}
		page := model.Page{Number: len(d.Pages) + 1, Name: fmt.Sprintf("Slide %d", len(d.Pages)+1), Width: width, Height: height}
		for _, shape := range find(slide, "sp") {
			appendContainerText(&page, shape)
		}
		for _, frame := range find(slide, "graphicFrame") {
			if table, ok := parseTable(frame); ok {
				page.Tables = append(page.Tables, table)
				for _, row := range table.Rows {
					for _, cell := range row.Cells {
						page.Runs = append(page.Runs, cell.Runs...)
					}
				}
				appendPageText(&page, model.TableTSV(table))
			} else {
				appendContainerText(&page, frame)
			}
		}
		slideRels := rels(a[path.Join(path.Dir(slidePath), "_rels", path.Base(slidePath)+".rels")], path.Dir(slidePath))
		for _, pic := range find(slide, "pic") {
			blips := find(pic, "blip")
			if len(blips) == 0 {
				continue
			}
			target := slideRels[attr(blips[0], "embed")]
			data := a[target]
			if len(data) == 0 {
				continue
			}
			im := model.Image{Name: path.Base(target), Format: strings.TrimPrefix(path.Ext(target), "."), Bounds: shapeBounds(pic), Data: data}
			if cfg, _, e := image.DecodeConfig(bytes.NewReader(data)); e == nil {
				im.Width = cfg.Width
				im.Height = cfg.Height
			}
			page.Images = append(page.Images, im)
		}
		d.Pages = append(d.Pages, page)
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("PPTX contains no slides")
	}
	return d, nil
}

func appendContainerText(page *model.Page, container node) {
	bounds := shapeBounds(container)
	text, runs := containerText(container, bounds)
	page.Runs = append(page.Runs, runs...)
	appendPageText(page, text)
}

func containerText(container node, bounds model.Rect) (string, []model.TextRun) {
	var text strings.Builder
	var runs []model.TextRun
	for _, paragraph := range find(container, "p") {
		var line strings.Builder
		for _, runNode := range paragraphRuns(paragraph) {
			value := nodeText(runNode)
			if value == "" {
				continue
			}
			style := runStyle(runNode)
			style.Text = value
			style.Bounds = bounds
			runs = append(runs, style)
			line.WriteString(value)
		}
		if line.Len() > 0 {
			if text.Len() > 0 {
				text.WriteByte('\n')
			}
			text.WriteString(line.String())
		}
	}
	return text.String(), runs
}

func appendPageText(page *model.Page, value string) {
	if value == "" {
		return
	}
	if page.Text != "" {
		page.Text += "\n"
	}
	page.Text += value
}

func parseTable(frame node) (model.Table, bool) {
	tables := find(frame, "tbl")
	if len(tables) == 0 {
		return model.Table{}, false
	}
	tableNode := tables[0]
	table := model.Table{Bounds: shapeBounds(frame)}

	var columnWidths []float64
	if grid, ok := direct(tableNode, "tblGrid"); ok {
		for _, column := range grid.Nodes {
			if column.XMLName.Local == "gridCol" {
				columnWidths = append(columnWidths, emu(attr(column, "w")))
			}
		}
	}
	var rowNodes []node
	for _, child := range tableNode.Nodes {
		if child.XMLName.Local == "tr" {
			rowNodes = append(rowNodes, child)
		}
	}
	rowHeights := make([]float64, len(rowNodes))
	for i, row := range rowNodes {
		rowHeights[i] = emu(attr(row, "h"))
	}
	columnOffsets := scaledOffsets(columnWidths, table.Bounds.Width)
	rowOffsets := scaledOffsets(rowHeights, table.Bounds.Height)

	for rowIndex, rowNode := range rowNodes {
		row := model.TableRow{}
		columnIndex := 0
		for _, cellNode := range rowNode.Nodes {
			if cellNode.XMLName.Local != "tc" {
				continue
			}
			columnSpan := positiveInt(attr(cellNode, "gridSpan"))
			rowSpan := positiveInt(attr(cellNode, "rowSpan"))
			continuation := xmlBool(attr(cellNode, "hMerge")) || xmlBool(attr(cellNode, "vMerge"))
			if !continuation {
				bounds := tableCellBounds(table.Bounds, columnOffsets, rowOffsets, columnIndex, rowIndex, columnSpan, rowSpan)
				text, runs := containerText(cellNode, bounds)
				row.Cells = append(row.Cells, model.TableCell{
					Row: rowIndex, Column: columnIndex, RowSpan: rowSpan, ColSpan: columnSpan,
					Bounds: bounds, Text: text, Runs: runs,
				})
			}
			columnIndex++
		}
		table.Rows = append(table.Rows, row)
	}
	return table, len(table.Rows) > 0
}

func positiveInt(value string) int {
	n, _ := strconv.Atoi(value)
	if n < 1 {
		return 1
	}
	return n
}

func xmlBool(value string) bool {
	return value == "1" || strings.EqualFold(value, "true")
}

func scaledOffsets(sizes []float64, target float64) []float64 {
	offsets := make([]float64, len(sizes)+1)
	var total float64
	for _, size := range sizes {
		total += size
	}
	scale := 1.0
	if total > 0 && target > 0 {
		scale = target / total
	}
	for i, size := range sizes {
		offsets[i+1] = offsets[i] + size*scale
	}
	return offsets
}

func tableCellBounds(table model.Rect, columns, rows []float64, column, row, columnSpan, rowSpan int) model.Rect {
	if column < 0 || row < 0 || column >= len(columns)-1 || row >= len(rows)-1 {
		return model.Rect{}
	}
	columnEnd := column + columnSpan
	if columnEnd >= len(columns) {
		columnEnd = len(columns) - 1
	}
	rowEnd := row + rowSpan
	if rowEnd >= len(rows) {
		rowEnd = len(rows) - 1
	}
	return model.Rect{
		X: table.X + columns[column], Y: table.Y + rows[row],
		Width: columns[columnEnd] - columns[column], Height: rows[rowEnd] - rows[row],
	}
}

func relationshipID(n node) string {
	for _, a := range n.Attrs {
		if a.Name.Local == "id" && strings.Contains(a.Name.Space, "relationships") {
			return a.Value
		}
	}
	return attr(n, "id")
}

func emu(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v / 12700 }
func shapeBounds(n node) model.Rect {
	xfrms := find(n, "xfrm")
	if len(xfrms) == 0 {
		return model.Rect{}
	}
	off, _ := direct(xfrms[0], "off")
	ext, _ := direct(xfrms[0], "ext")
	return model.Rect{X: emu(attr(off, "x")), Y: emu(attr(off, "y")), Width: emu(attr(ext, "cx")), Height: emu(attr(ext, "cy"))}
}
func paragraphRuns(p node) []node {
	var out []node
	for _, c := range p.Nodes {
		if c.XMLName.Local == "r" || c.XMLName.Local == "fld" {
			out = append(out, c)
		}
	}
	return out
}
func nodeText(n node) string {
	var b strings.Builder
	for _, t := range find(n, "t") {
		b.WriteString(t.Text)
	}
	return b.String()
}
func runStyle(n node) model.TextRun {
	r := model.TextRun{Size: 18, Color: "#000000"}
	props, ok := direct(n, "rPr")
	if !ok {
		return r
	}
	if sz := attr(props, "sz"); sz != "" {
		v, _ := strconv.ParseFloat(sz, 64)
		r.Size = v / 100
	}
	if fonts := find(props, "latin"); len(fonts) > 0 {
		r.Font = attr(fonts[0], "typeface")
	}
	if r.Font == "" {
		if fonts := find(props, "ea"); len(fonts) > 0 {
			r.Font = attr(fonts[0], "typeface")
		}
	}
	if colors := find(props, "srgbClr"); len(colors) > 0 {
		r.Color = "#" + strings.ToUpper(attr(colors[0], "val"))
	}
	return r
}
