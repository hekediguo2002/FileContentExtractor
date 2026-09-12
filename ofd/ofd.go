// Package ofd 解析 OFD（GB/T 33190 版式文档）中的文本对象及其版式属性，
// 仅依赖 Go 标准库。OFD 是 ZIP 封装的 XML 页面描述，本包提取既有文本，
// 不做渲染或 OCR。参考 SensitiveFile/file_detector 的 ofd_reader.py 移植。
package ofd

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"path"
	"strconv"
	"strings"

	"filecontentextractor/model"
)

const mmToPt = 72.0 / 25.4

const maxXMLBytes = 20 << 20
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
		d.Pages = append(d.Pages, parsePage(pageXML, len(d.Pages)+1, fonts, pageWidth, pageHeight))
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("invalid OFD package: no pages found")
	}
	return d, nil
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

func parsePage(pageXML xmlNode, number int, fonts map[string]string, defW, defH float64) model.Page {
	page := model.Page{Number: number, Width: defW, Height: defH}
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
			page.Images = append(page.Images, model.Image{
				Name:   attr(obj, "ID"),
				Bounds: model.Rect{X: values[0] * mmToPt, Y: values[1] * mmToPt, Width: values[2] * mmToPt, Height: values[3] * mmToPt},
			})
		}
	}
	page.Text = pageText(page.Runs)
	return page
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
