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
			appendContainerText(&page, frame)
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
			page.Runs = append(page.Runs, style)
			line.WriteString(value)
		}
		if line.Len() > 0 {
			if page.Text != "" {
				page.Text += "\n"
			}
			page.Text += line.String()
		}
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
