package xlsx

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
	"regexp"
	"sort"
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
func text(n node) string {
	var b strings.Builder
	if n.XMLName.Local == "t" || n.XMLName.Local == "v" {
		b.WriteString(n.Text)
	}
	for _, c := range n.Nodes {
		b.WriteString(text(c))
	}
	return b.String()
}

type archive map[string][]byte

func openArchive(name string) (archive, error) {
	z, e := zip.OpenReader(name)
	if e != nil {
		return nil, e
	}
	defer z.Close()
	a := archive{}
	const maxEntrySize = 512 << 20 // decompressed size limit per entry, guards against zip bombs
	for _, f := range z.File {
		if f.UncompressedSize64 > maxEntrySize {
			return nil, fmt.Errorf("xlsx entry %s too large", f.Name)
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
func parseNode(data []byte) (node, error) { var n node; e := xml.Unmarshal(data, &n); return n, e }
func relationships(data []byte, base string) map[string]string {
	result := map[string]string{}
	n, e := parseNode(data)
	if e != nil {
		return result
	}
	for _, r := range find(n, "Relationship") {
		target := attr(r, "Target")
		if strings.HasPrefix(target, "/") {
			result[attr(r, "Id")] = path.Clean(strings.TrimPrefix(target, "/"))
		} else {
			result[attr(r, "Id")] = path.Clean(path.Join(base, target))
		}
	}
	return result
}

type fontStyle struct {
	name  string
	size  float64
	color string
}

func ParseFile(name string) (*model.Document, error) {
	a, e := openArchive(name)
	if e != nil {
		return nil, e
	}
	workbook, e := parseNode(a["xl/workbook.xml"])
	if e != nil {
		return nil, e
	}
	rels := relationships(a["xl/_rels/workbook.xml.rels"], "xl")
	shared := sharedStrings(a["xl/sharedStrings.xml"])
	fonts, xfs := styles(a["xl/styles.xml"])
	d := &model.Document{Path: name, Format: "xlsx", Pagination: "worksheet"}
	for _, sheet := range find(workbook, "sheet") {
		target := rels[attr(sheet, "id")]
		if target == "" {
			continue
		}
		root, e := parseNode(a[target])
		if e != nil {
			return nil, fmt.Errorf("parse %s: %w", target, e)
		}
		page := model.Page{Number: len(d.Pages) + 1, Name: attr(sheet, "name")}
		rows := find(root, "row")
		for _, row := range rows {
			var values []string
			for _, cell := range direct(row, "c") {
				value := cellValue(cell, shared)
				col, rowNum := cellPosition(attr(cell, "r"))
				for len(values) < col {
					values = append(values, "")
				}
				values = append(values, value)
				if value != "" {
					style := fontStyle{size: 11, color: "#000000"}
					if len(fonts) > 0 {
						style = fonts[0]
					}
					si, _ := strconv.Atoi(attr(cell, "s"))
					if si >= 0 && si < len(xfs) && xfs[si] >= 0 && xfs[si] < len(fonts) {
						style = fonts[xfs[si]]
					}
					page.Runs = append(page.Runs, model.TextRun{Text: value, Font: style.name, Size: style.size, Color: style.color, Bounds: model.Rect{X: float64(col), Y: float64(rowNum), Width: 1, Height: 1}})
				}
			}
			line := strings.TrimRight(strings.Join(values, "\t"), "\t")
			if line != "" {
				if page.Text != "" {
					page.Text += "\n"
				}
				page.Text += line
			}
		}
		page.Images = sheetImages(a, target, root)
		d.Pages = append(d.Pages, page)
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("XLSX contains no worksheets")
	}
	return d, nil
}

func direct(n node, name string) []node {
	var out []node
	for _, c := range n.Nodes {
		if c.XMLName.Local == name {
			out = append(out, c)
		}
	}
	return out
}
func sharedStrings(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	n, e := parseNode(data)
	if e != nil {
		return nil
	}
	var out []string
	for _, si := range direct(n, "si") {
		out = append(out, displayedText(si))
	}
	return out
}

func displayedText(n node) string {
	if n.XMLName.Local == "rPh" || n.XMLName.Local == "phoneticPr" {
		return ""
	}
	if n.XMLName.Local == "t" {
		return n.Text
	}
	var b strings.Builder
	for _, child := range n.Nodes {
		b.WriteString(displayedText(child))
	}
	return b.String()
}

func cellValue(c node, shared []string) string {
	kind := attr(c, "t")
	if kind == "inlineStr" {
		if is := find(c, "is"); len(is) > 0 {
			return displayedText(is[0])
		}
	}
	v := ""
	if vs := direct(c, "v"); len(vs) > 0 {
		v = vs[0].Text
	}
	if kind == "s" {
		i, _ := strconv.Atoi(v)
		if i >= 0 && i < len(shared) {
			return shared[i]
		}
	}
	if kind == "b" {
		if v == "1" {
			return "TRUE"
		}
		return "FALSE"
	}
	return v
}

var cellRef = regexp.MustCompile(`^([A-Za-z]+)(\d+)$`)

func cellPosition(ref string) (int, int) {
	m := cellRef.FindStringSubmatch(ref)
	if len(m) < 3 {
		return 0, 0
	}
	col := 0
	for _, r := range strings.ToUpper(m[1]) {
		col = col*26 + int(r-'A'+1)
		if col > 16384 { // XFD is the widest real column; clamp bogus refs to avoid a huge row allocation
			col = 16384
			break
		}
	}
	row, _ := strconv.Atoi(m[2])
	return col - 1, row - 1
}
func styles(data []byte) ([]fontStyle, []int) {
	if len(data) == 0 {
		return []fontStyle{{size: 11, color: "#000000"}}, []int{0}
	}
	n, e := parseNode(data)
	if e != nil {
		return nil, nil
	}
	var fonts []fontStyle
	if fs := find(n, "fonts"); len(fs) > 0 {
		for _, f := range direct(fs[0], "font") {
			s := fontStyle{size: 11, color: "#000000"}
			if v := find(f, "name"); len(v) > 0 {
				s.name = attr(v[0], "val")
			}
			if v := find(f, "sz"); len(v) > 0 {
				s.size, _ = strconv.ParseFloat(attr(v[0], "val"), 64)
			}
			if v := find(f, "color"); len(v) > 0 {
				if rgb := attr(v[0], "rgb"); len(rgb) >= 6 {
					s.color = "#" + strings.ToUpper(rgb[len(rgb)-6:])
				}
			}
			fonts = append(fonts, s)
		}
	}
	var xfs []int
	if cs := find(n, "cellXfs"); len(cs) > 0 {
		for _, xf := range direct(cs[0], "xf") {
			id, _ := strconv.Atoi(attr(xf, "fontId"))
			xfs = append(xfs, id)
		}
	}
	return fonts, xfs
}
func sheetImages(a archive, sheetPath string, sheet node) []model.Image {
	relPath := path.Join(path.Dir(sheetPath), "_rels", path.Base(sheetPath)+".rels")
	sheetRels := relationships(a[relPath], path.Dir(sheetPath))
	var drawings []string
	for _, d := range find(sheet, "drawing") {
		if p := sheetRels[attr(d, "id")]; p != "" {
			drawings = append(drawings, p)
		}
	}
	var images []model.Image
	seen := map[string]bool{}
	for _, drawing := range drawings {
		rel := relationships(a[path.Join(path.Dir(drawing), "_rels", path.Base(drawing)+".rels")], path.Dir(drawing))
		for _, target := range rel {
			if !strings.Contains(target, "/media/") || seen[target] {
				continue
			}
			seen[target] = true
			data := a[target]
			im := model.Image{Name: path.Base(target), Format: strings.TrimPrefix(path.Ext(target), "."), Data: data}
			if cfg, _, e := image.DecodeConfig(bytes.NewReader(data)); e == nil {
				im.Width = cfg.Width
				im.Height = cfg.Height
			}
			images = append(images, im)
		}
	}
	sort.Slice(images, func(i, j int) bool { return images[i].Name < images[j].Name })
	return images
}
