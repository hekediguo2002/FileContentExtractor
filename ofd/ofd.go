// Package ofd 解析 OFD（GB/T 33190 版式文档）中的文本对象及其版式属性，
// 仅依赖 Go 标准库。OFD 是 ZIP 封装的 XML 页面描述，本包提取既有文本，
// 不做渲染或 OCR。参考 SensitiveFile/file_detector 的 ofd_reader.py 移植。
package ofd

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
	"math"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/hekediguo2002/FileContentExtractor/model"
)

const mmToPt = 72.0 / 25.4

const maxXMLBytes = 20 << 20
const maxResourceBytes = 256 << 20
const maxPackageEntries = 20000

type xmlNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xmlNode  `xml:",any"`
	Text    string     `xml:",chardata"`
}

func ParseFile(name string) (*model.Document, error) {
	z, err := zip.OpenReader(name)
	if err != nil {
		return nil, fmt.Errorf("failed to open OFD package: %w", err)
	}
	defer z.Close()
	if len(z.File) > maxPackageEntries {
		return nil, fmt.Errorf("OFD package contains too many entries")
	}

	names := map[string]*zip.File{}
	for _, f := range z.File {
		names[strings.ToLower(f.Name)] = f
	}
	readXML := func(member string) (xmlNode, error) {
		f := names[strings.ToLower(member)]
		if f == nil {
			return xmlNode{}, fmt.Errorf("invalid OFD package: missing %s", member)
		}
		if f.UncompressedSize64 > maxXMLBytes {
			return xmlNode{}, fmt.Errorf("OFD XML entry is too large: %s", member)
		}
		r, err := f.Open()
		if err != nil {
			return xmlNode{}, err
		}
		defer r.Close()
		var n xmlNode
		if err := xml.NewDecoder(r).Decode(&n); err != nil {
			return xmlNode{}, fmt.Errorf("invalid OFD XML in %s: %w", member, err)
		}
		return n, nil
	}
	readResource := func(member string) ([]byte, error) {
		f := names[strings.ToLower(member)]
		if f == nil {
			return nil, fmt.Errorf("invalid OFD package: missing %s", member)
		}
		if f.UncompressedSize64 > maxResourceBytes {
			return nil, fmt.Errorf("OFD resource is too large: %s", member)
		}
		r, err := f.Open()
		if err != nil {
			return nil, err
		}
		defer r.Close()
		return io.ReadAll(io.LimitReader(r, maxResourceBytes+1))
	}

	ofdFile := names["ofd.xml"]
	if ofdFile == nil {
		return nil, fmt.Errorf("invalid OFD package: OFD.xml is missing")
	}
	ofdRoot, err := readXML("OFD.xml")
	if err != nil {
		return nil, err
	}
	docRoot := findFirst(ofdRoot, "DocRoot")
	if docRoot == nil || strings.TrimSpace(docRoot.Text) == "" {
		return nil, fmt.Errorf("invalid OFD package: DocRoot is missing")
	}
	docName, err := resolveMember("OFD.xml", docRoot.Text)
	if err != nil {
		return nil, err
	}
	docXML, err := readXML(docName)
	if err != nil {
		return nil, err
	}

	fonts := loadFonts(names, readXML, docName, docXML)
	media := loadMedia(names, readXML, readResource, docName, docXML)
	pageWidth, pageHeight := pageSize(docXML)

	d := &model.Document{Path: name, Format: "ofd", Pagination: "native"}
	for _, item := range walk(docXML) {
		if item.XMLName.Local != "Page" || attr(item, "BaseLoc") == "" {
			continue
		}
		member, err := resolveMember(docName, attr(item, "BaseLoc"))
		if err != nil {
			return nil, err
		}
		pageXML, err := readXML(member)
		if err != nil {
			return nil, err
		}
		d.Pages = append(d.Pages, parsePage(pageXML, len(d.Pages)+1, fonts, media, pageWidth, pageHeight))
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("invalid OFD package: no pages found")
	}
	return d, nil
}

func loadMedia(names map[string]*zip.File, readXML func(string) (xmlNode, error), readResource func(string) ([]byte, error), docName string, docXML xmlNode) map[string]model.Image {
	media := map[string]model.Image{}
	for _, item := range walk(docXML) {
		if item.XMLName.Local != "PublicRes" && item.XMLName.Local != "DocumentRes" {
			continue
		}
		resourceFile := strings.TrimSpace(item.Text)
		if resourceFile == "" {
			continue
		}
		member, err := resolveMember(docName, resourceFile)
		if err != nil {
			continue
		}
		resourceXML, err := readXML(member)
		if err != nil {
			continue
		}
		baseLoc := strings.TrimSpace(attr(resourceXML, "BaseLoc"))
		for _, resource := range walk(resourceXML) {
			if resource.XMLName.Local != "MultiMedia" || attr(resource, "ID") == "" || !strings.EqualFold(attr(resource, "Type"), "Image") {
				continue
			}
			mediaFile := findFirst(resource, "MediaFile")
			if mediaFile == nil || strings.TrimSpace(mediaFile.Text) == "" {
				continue
			}
			imageMember, err := resolveMember(member, path.Join(baseLoc, strings.TrimSpace(mediaFile.Text)))
			if err != nil {
				continue
			}
			if names[strings.ToLower(imageMember)] == nil {
				continue
			}
			data, err := readResource(imageMember)
			if err != nil || len(data) == 0 {
				continue
			}
			format := strings.ToLower(attr(resource, "Format"))
			if format == "jpeg" {
				format = "jpg"
			}
			value := model.Image{Name: path.Base(imageMember), Format: format, Data: data}
			if config, detected, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
				value.Width, value.Height = config.Width, config.Height
				if value.Format == "" {
					value.Format = detected
				}
			}
			media[attr(resource, "ID")] = value
		}
	}
	return media
}

// resolveMember 把 OFD 内的相对路径解析为包内成员名，拒绝越界路径。
func resolveMember(baseFile, relativePath string) (string, error) {
	relativePath = strings.ReplaceAll(strings.TrimSpace(relativePath), "\\", "/")
	if strings.HasPrefix(relativePath, "/") {
		relativePath = strings.TrimLeft(relativePath, "/")
		baseFile = ""
	}
	resolved := path.Clean(path.Join(path.Dir(baseFile), relativePath))
	if resolved == ".." || strings.HasPrefix(resolved, "../") {
		return "", fmt.Errorf("unsafe path in OFD package: %s", relativePath)
	}
	return resolved, nil
}

func walk(n xmlNode) []xmlNode {
	out := []xmlNode{n}
	for _, c := range n.Nodes {
		out = append(out, walk(c)...)
	}
	return out
}

func findFirst(n xmlNode, local string) *xmlNode {
	for _, item := range walk(n) {
		if item.XMLName.Local == local {
			return &item
		}
	}
	return nil
}

func attr(n xmlNode, name string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

func numbers(s string) []float64 {
	var out []float64
	for _, part := range strings.Fields(strings.ReplaceAll(s, ",", " ")) {
		if v, err := strconv.ParseFloat(part, 64); err == nil {
			out = append(out, v)
		}
	}
	return out
}

func loadFonts(names map[string]*zip.File, readXML func(string) (xmlNode, error), docName string, docXML xmlNode) map[string]string {
	fonts := map[string]string{}
	for _, item := range walk(docXML) {
		if item.XMLName.Local != "PublicRes" && item.XMLName.Local != "DocumentRes" {
			continue
		}
		resFile := strings.TrimSpace(item.Text)
		if resFile == "" {
			continue
		}
		member, err := resolveMember(docName, resFile)
		if err != nil {
			continue
		}
		resXML, err := readXML(member)
		if err != nil {
			continue
		}
		for _, f := range walk(resXML) {
			if f.XMLName.Local != "Font" || attr(f, "ID") == "" {
				continue
			}
			name := attr(f, "FamilyName")
			if name == "" {
				name = attr(f, "FontName")
			}
			fonts[attr(f, "ID")] = name
		}
	}
	return fonts
}

func pageSize(docXML xmlNode) (float64, float64) {
	if box := findFirst(docXML, "PhysicalBox"); box != nil {
		if values := numbers(box.Text); len(values) >= 4 {
			return values[2] * mmToPt, values[3] * mmToPt
		}
	}
	return 210 * mmToPt, 297 * mmToPt
}

func parsePage(pageXML xmlNode, number int, fonts map[string]string, media map[string]model.Image, defW, defH float64) model.Page {
	page := model.Page{Number: number, Width: defW, Height: defH}
	var segments []ofdSegment
	if box := findFirst(pageXML, "PhysicalBox"); box != nil {
		if values := numbers(box.Text); len(values) >= 4 {
			page.Width, page.Height = values[2]*mmToPt, values[3]*mmToPt
		}
	}

	for _, obj := range walk(pageXML) {
		switch obj.XMLName.Local {
		case "TextObject":
			if run, ok := textRun(obj, fonts); ok {
				page.Runs = append(page.Runs, run)
			}
		case "ImageObject":
			values := numbers(attr(obj, "Boundary"))
			if len(values) < 4 {
				continue
			}
			imageValue := media[attr(obj, "ResourceID")]
			if imageValue.Name == "" {
				imageValue.Name = attr(obj, "ID")
			}
			imageValue.Bounds = model.Rect{X: values[0] * mmToPt, Y: values[1] * mmToPt, Width: values[2] * mmToPt, Height: values[3] * mmToPt}
			page.Images = append(page.Images, imageValue)
		case "PathObject":
			segments = append(segments, pathSegments(obj)...)
		}
	}
	tables, tableRuns := detectTables(page.Runs, segments)
	page.Tables = tables
	page.Text = pageTextWithTables(page.Runs, page.Tables, tableRuns)
	return page
}

type ofdSegment struct {
	x1, y1, x2, y2 float64
}

func pathSegments(object xmlNode) []ofdSegment {
	boundary := numbers(attr(object, "Boundary"))
	if len(boundary) < 2 {
		return nil
	}
	offsetX, offsetY := boundary[0]*mmToPt, boundary[1]*mmToPt
	var result []ofdSegment
	for _, node := range object.Nodes {
		if node.XMLName.Local != "AbbreviatedData" {
			continue
		}
		tokens := strings.Fields(strings.ReplaceAll(node.Text, ",", " "))
		currentX, currentY := 0.0, 0.0
		haveCurrent := false
		for index := 0; index < len(tokens); {
			switch strings.ToUpper(tokens[index]) {
			case "M":
				if index+2 >= len(tokens) {
					index = len(tokens)
					continue
				}
				x, xErr := strconv.ParseFloat(tokens[index+1], 64)
				y, yErr := strconv.ParseFloat(tokens[index+2], 64)
				if xErr == nil && yErr == nil {
					currentX, currentY = offsetX+x*mmToPt, offsetY+y*mmToPt
					haveCurrent = true
				}
				index += 3
			case "L":
				if index+2 >= len(tokens) {
					index = len(tokens)
					continue
				}
				x, xErr := strconv.ParseFloat(tokens[index+1], 64)
				y, yErr := strconv.ParseFloat(tokens[index+2], 64)
				if xErr == nil && yErr == nil && haveCurrent {
					nextX, nextY := offsetX+x*mmToPt, offsetY+y*mmToPt
					result = append(result, ofdSegment{currentX, currentY, nextX, nextY})
					currentX, currentY = nextX, nextY
				}
				index += 3
			default:
				index++
			}
		}
	}
	return result
}

type ruledLine struct {
	coordinate float64
	from, to   float64
}

func detectTables(runs []model.TextRun, segments []ofdSegment) ([]model.Table, map[int]int) {
	const tolerance = 1.5
	var horizontal, vertical []ruledLine
	for _, segment := range segments {
		dx, dy := math.Abs(segment.x2-segment.x1), math.Abs(segment.y2-segment.y1)
		if dy <= tolerance && dx >= 4 {
			horizontal = append(horizontal, ruledLine{(segment.y1 + segment.y2) / 2, math.Min(segment.x1, segment.x2), math.Max(segment.x1, segment.x2)})
		} else if dx <= tolerance && dy >= 4 {
			vertical = append(vertical, ruledLine{(segment.x1 + segment.x2) / 2, math.Min(segment.y1, segment.y2), math.Max(segment.y1, segment.y2)})
		}
	}
	horizontal = mergeLines(horizontal, tolerance)
	vertical = mergeLines(vertical, tolerance)
	if len(horizontal) < 2 || len(vertical) < 2 || len(horizontal) > 10000 || len(vertical) > 10000 {
		return nil, map[int]int{}
	}
	yLevels := lineCoordinates(horizontal, tolerance)
	if len(yLevels) > 512 {
		return nil, map[int]int{}
	}
	var cells []model.TableCell
	for start := 0; start+1 < len(yLevels); start++ {
		for end := start + 1; end < len(yLevels); end++ {
			top, bottom := yLevels[start], yLevels[end]
			var xs []float64
			for _, line := range vertical {
				if line.from <= top+tolerance && line.to >= bottom-tolerance {
					xs = append(xs, line.coordinate)
				}
			}
			xs = uniqueValues(xs, tolerance)
			for column := 0; column+1 < len(xs); column++ {
				left, right := xs[column], xs[column+1]
				if right-left < 3 || !lineCovers(horizontal, top, left, right, tolerance) || !lineCovers(horizontal, bottom, left, right, tolerance) || internalLineCovers(horizontal, yLevels, start, end, left, right, tolerance) {
					continue
				}
				cells = append(cells, model.TableCell{Bounds: model.Rect{X: left, Y: top, Width: right - left, Height: bottom - top}, RowSpan: end - start, ColSpan: 1})
				if len(cells) > 4096 {
					return nil, map[int]int{}
				}
			}
		}
	}
	components := cellGroups(cells, tolerance)
	var tables []model.Table
	tableRuns := map[int]int{}
	for _, component := range components {
		if len(component) < 2 {
			continue
		}
		table, assigned := buildTable(component, runs, tolerance)
		if len(table.Rows) < 2 && tableColumnCount(table) < 2 {
			continue
		}
		tableIndex := len(tables)
		tables = append(tables, table)
		for runIndex := range assigned {
			tableRuns[runIndex] = tableIndex
		}
	}
	return tables, tableRuns
}

func mergeLines(lines []ruledLine, tolerance float64) []ruledLine {
	sort.Slice(lines, func(i, j int) bool {
		if math.Abs(lines[i].coordinate-lines[j].coordinate) > tolerance {
			return lines[i].coordinate < lines[j].coordinate
		}
		return lines[i].from < lines[j].from
	})
	var result []ruledLine
	for _, line := range lines {
		if len(result) > 0 {
			last := &result[len(result)-1]
			if math.Abs(last.coordinate-line.coordinate) <= tolerance && line.from <= last.to+tolerance {
				last.to = math.Max(last.to, line.to)
				continue
			}
		}
		result = append(result, line)
	}
	return result
}

func lineCoordinates(lines []ruledLine, tolerance float64) []float64 {
	values := make([]float64, 0, len(lines))
	for _, line := range lines {
		values = append(values, line.coordinate)
	}
	return uniqueValues(values, tolerance)
}

func uniqueValues(values []float64, tolerance float64) []float64 {
	sort.Float64s(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || math.Abs(result[len(result)-1]-value) > tolerance {
			result = append(result, value)
		}
	}
	return result
}

func lineCovers(lines []ruledLine, coordinate, from, to, tolerance float64) bool {
	for _, line := range lines {
		if math.Abs(line.coordinate-coordinate) <= tolerance && line.from <= from+tolerance && line.to >= to-tolerance {
			return true
		}
	}
	return false
}

func internalLineCovers(lines []ruledLine, levels []float64, start, end int, from, to, tolerance float64) bool {
	for index := start + 1; index < end; index++ {
		if lineCovers(lines, levels[index], from, to, tolerance) {
			return true
		}
	}
	return false
}

func cellGroups(cells []model.TableCell, tolerance float64) [][]model.TableCell {
	visited := make([]bool, len(cells))
	var result [][]model.TableCell
	for start := range cells {
		if visited[start] {
			continue
		}
		visited[start] = true
		queue := []int{start}
		var group []model.TableCell
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			group = append(group, cells[current])
			for next := range cells {
				if !visited[next] && rectanglesTouch(cells[current].Bounds, cells[next].Bounds, tolerance) {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}
		result = append(result, group)
	}
	return result
}

func rectanglesTouch(a, b model.Rect, tolerance float64) bool {
	return math.Min(a.X+a.Width, b.X+b.Width)-math.Max(a.X, b.X) >= -tolerance && math.Min(a.Y+a.Height, b.Y+b.Height)-math.Max(a.Y, b.Y) >= -tolerance
}

func buildTable(cells []model.TableCell, runs []model.TextRun, tolerance float64) (model.Table, map[int]bool) {
	sort.Slice(cells, func(i, j int) bool {
		if math.Abs(cells[i].Bounds.Y-cells[j].Bounds.Y) > tolerance {
			return cells[i].Bounds.Y < cells[j].Bounds.Y
		}
		return cells[i].Bounds.X < cells[j].Bounds.X
	})
	var table model.Table
	for _, cell := range cells {
		rowIndex := -1
		for index := range table.Rows {
			if len(table.Rows[index].Cells) > 0 && math.Abs(table.Rows[index].Cells[0].Bounds.Y-cell.Bounds.Y) <= tolerance {
				rowIndex = index
				break
			}
		}
		if rowIndex < 0 {
			table.Rows = append(table.Rows, model.TableRow{})
			rowIndex = len(table.Rows) - 1
		}
		cell.Row = rowIndex
		table.Rows[rowIndex].Cells = append(table.Rows[rowIndex].Cells, cell)
		table.Bounds = unionRect(table.Bounds, cell.Bounds)
	}
	var columns []float64
	for rowIndex := range table.Rows {
		sort.Slice(table.Rows[rowIndex].Cells, func(i, j int) bool {
			return table.Rows[rowIndex].Cells[i].Bounds.X < table.Rows[rowIndex].Cells[j].Bounds.X
		})
		for _, cell := range table.Rows[rowIndex].Cells {
			columns = append(columns, cell.Bounds.X, cell.Bounds.X+cell.Bounds.Width)
		}
	}
	columns = uniqueValues(columns, tolerance)
	for rowIndex := range table.Rows {
		for cellIndex := range table.Rows[rowIndex].Cells {
			cell := &table.Rows[rowIndex].Cells[cellIndex]
			cell.Column = nearestValue(columns, cell.Bounds.X)
			cell.ColSpan = maxInt(nearestValue(columns, cell.Bounds.X+cell.Bounds.Width)-cell.Column, 1)
		}
	}
	assigned := map[int]bool{}
	for runIndex, run := range runs {
		x, y := run.Bounds.X+math.Min(run.Bounds.Width/2, math.Max(run.Size*.2, 1)), run.Bounds.Y+run.Bounds.Height/2
		for rowIndex := range table.Rows {
			matched := false
			for cellIndex := range table.Rows[rowIndex].Cells {
				cell := &table.Rows[rowIndex].Cells[cellIndex]
				if x >= cell.Bounds.X-tolerance && x <= cell.Bounds.X+cell.Bounds.Width+tolerance && y >= cell.Bounds.Y-tolerance && y <= cell.Bounds.Y+cell.Bounds.Height+tolerance {
					cell.Runs = append(cell.Runs, run)
					assigned[runIndex] = true
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
	}
	for rowIndex := range table.Rows {
		for cellIndex := range table.Rows[rowIndex].Cells {
			cell := &table.Rows[rowIndex].Cells[cellIndex]
			cell.Text = pageText(cell.Runs)
		}
	}
	return table, assigned
}

func nearestValue(values []float64, wanted float64) int {
	best := 0
	for index := 1; index < len(values); index++ {
		if math.Abs(values[index]-wanted) < math.Abs(values[best]-wanted) {
			best = index
		}
	}
	return best
}

func tableColumnCount(table model.Table) int {
	count := 0
	for _, row := range table.Rows {
		for _, cell := range row.Cells {
			count = maxInt(count, cell.Column+cell.ColSpan)
		}
	}
	return count
}

func unionRect(a, b model.Rect) model.Rect {
	if a.Width == 0 && a.Height == 0 {
		return b
	}
	x, y := math.Min(a.X, b.X), math.Min(a.Y, b.Y)
	right, bottom := math.Max(a.X+a.Width, b.X+b.Width), math.Max(a.Y+a.Height, b.Y+b.Height)
	return model.Rect{X: x, Y: y, Width: right - x, Height: bottom - y}
}

func pageTextWithTables(runs []model.TextRun, tables []model.Table, tableRuns map[int]int) string {
	var values []string
	var plain []model.TextRun
	emitted := map[int]bool{}
	flush := func() {
		if text := pageText(plain); text != "" {
			values = append(values, text)
		}
		plain = nil
	}
	for runIndex, run := range runs {
		tableIndex, ok := tableRuns[runIndex]
		if !ok {
			plain = append(plain, run)
			continue
		}
		flush()
		if !emitted[tableIndex] {
			values = append(values, model.TableTSV(tables[tableIndex]))
			emitted[tableIndex] = true
		}
	}
	flush()
	return strings.Trim(strings.Join(values, "\n"), " \r\n")
}

func textRun(obj xmlNode, fonts map[string]string) (model.TextRun, bool) {
	boundary := numbers(attr(obj, "Boundary"))
	if len(boundary) < 4 {
		return model.TextRun{}, false
	}
	var sb strings.Builder
	for _, node := range walk(obj) {
		if node.XMLName.Local == "TextCode" {
			sb.WriteString(node.Text)
		}
	}
	text := sb.String()
	if text == "" {
		return model.TextRun{}, false
	}
	size := 0.0
	if values := numbers(attr(obj, "Size")); len(values) > 0 {
		size = values[0] * mmToPt
	}
	return model.TextRun{
		Text:  text,
		Font:  fonts[attr(obj, "Font")],
		Size:  size,
		Color: fillColor(obj),
		Bounds: model.Rect{
			X: boundary[0] * mmToPt, Y: boundary[1] * mmToPt,
			Width: boundary[2] * mmToPt, Height: boundary[3] * mmToPt,
		},
	}, true
}

func fillColor(obj xmlNode) string {
	for _, child := range obj.Nodes {
		if child.XMLName.Local != "FillColor" {
			continue
		}
		values := numbers(attr(child, "Value"))
		if len(values) < 3 {
			return ""
		}
		clamp := func(v float64) int {
			return maxInt(0, minInt(255, int(v+0.5)))
		}
		return fmt.Sprintf("#%02X%02X%02X", clamp(values[0]), clamp(values[1]), clamp(values[2]))
	}
	return ""
}

// pageText 按行聚合文本：同一视觉行（Y 接近）的文本对象连成一行。
func pageText(runs []model.TextRun) string {
	if len(runs) == 0 {
		return ""
	}
	ordered := append([]model.TextRun(nil), runs...)
	for i := 0; i < len(ordered); i++ {
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j].Bounds.Y < ordered[i].Bounds.Y ||
				(ordered[j].Bounds.Y == ordered[i].Bounds.Y && ordered[j].Bounds.X < ordered[i].Bounds.X) {
				ordered[i], ordered[j] = ordered[j], ordered[i]
			}
		}
	}
	var lines []string
	var cur strings.Builder
	curY := 0.0
	flush := func() {
		if cur.Len() > 0 {
			lines = append(lines, cur.String())
			cur.Reset()
		}
	}
	for _, r := range ordered {
		size := r.Size
		if size <= 0 {
			size = 10
		}
		if cur.Len() > 0 && abs(r.Bounds.Y-curY) > maxF(2, size*0.55) {
			flush()
		}
		if cur.Len() == 0 {
			curY = r.Bounds.Y
		}
		cur.WriteString(r.Text)
	}
	flush()
	return strings.Join(lines, "\n")
}

func abs(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func maxF(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
