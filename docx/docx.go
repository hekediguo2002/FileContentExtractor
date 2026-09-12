package docx

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

	"github.com/hekediguo2002/FileContentExtractor/layout"
	root "github.com/hekediguo2002/FileContentExtractor/model"
)

type xmlNode struct {
	XMLName xml.Name
	Attrs   []xml.Attr `xml:",any,attr"`
	Nodes   []xmlNode  `xml:",any"`
	Text    string     `xml:",chardata"`
}

type floatingContent struct {
	paragraphs []layout.Paragraph
	images     []root.Image
	pageHint   int
}

func attr(n xmlNode, local string) string {
	for _, a := range n.Attrs {
		if a.Name.Local == local {
			return a.Value
		}
	}
	return ""
}
func find(n xmlNode, local string) []xmlNode {
	var out []xmlNode
	for _, c := range n.Nodes {
		if c.XMLName.Local == local {
			out = append(out, c)
		}
		out = append(out, find(c, local)...)
	}
	return out
}

func bodyParagraphs(n xmlNode) []xmlNode {
	var out []xmlNode
	var walk func(xmlNode)
	walk = func(parent xmlNode) {
		for _, child := range parent.Nodes {
			if child.XMLName.Local == "txbxContent" {
				continue
			}
			if child.XMLName.Local == "p" {
				out = append(out, child)
				continue
			}
			walk(child)
		}
	}
	walk(n)
	return out
}

func paragraphDescendants(n xmlNode, local string) []xmlNode {
	var out []xmlNode
	var walk func(xmlNode)
	walk = func(parent xmlNode) {
		for _, child := range parent.Nodes {
			if child.XMLName.Local == "txbxContent" || child.XMLName.Local == "p" {
				continue
			}
			if child.XMLName.Local == local {
				out = append(out, child)
			}
			walk(child)
		}
	}
	walk(n)
	return out
}
func textOf(n xmlNode) string {
	var b strings.Builder
	switch n.XMLName.Local {
	case "t", "instrText":
		b.WriteString(n.Text)
	case "tab":
		b.WriteByte('\t')
	case "br":
		if attr(n, "type") != "page" {
			b.WriteByte('\n')
		}
	}
	for _, c := range n.Nodes {
		b.WriteString(textOf(c))
	}
	return b.String()
}

func ParseFile(name string) (*root.Document, error) {
	z, err := zip.OpenReader(name)
	if err != nil {
		return nil, err
	}
	defer z.Close()
	// Decompressed size limit per entry, guards against zip bombs.
	const maxEntrySize = 512 << 20
	read := func(p string) ([]byte, error) {
		for _, f := range z.File {
			if f.Name == p {
				if f.UncompressedSize64 > maxEntrySize {
					return nil, fmt.Errorf("docx entry %s too large", p)
				}
				r, e := f.Open()
				if e != nil {
					return nil, e
				}
				defer r.Close()
				return io.ReadAll(io.LimitReader(r, maxEntrySize))
			}
		}
		return nil, fmt.Errorf("missing %s", p)
	}
	files := map[string][]byte{}
	for _, f := range z.File {
		if strings.HasPrefix(f.Name, "word/media/") && f.UncompressedSize64 <= maxEntrySize {
			r, e := f.Open()
			if e == nil {
				files[f.Name], _ = io.ReadAll(io.LimitReader(r, maxEntrySize))
				r.Close()
			}
		}
	}
	rels := map[string]string{}
	if rb, e := read("word/_rels/document.xml.rels"); e == nil {
		var rn xmlNode
		if xml.Unmarshal(rb, &rn) == nil {
			for _, r := range find(rn, "Relationship") {
				rels[attr(r, "Id")] = path.Clean("word/" + attr(r, "Target"))
			}
		}
	}
	xmlData, err := read("word/document.xml")
	if err != nil {
		return nil, err
	}
	var rootNode xmlNode
	if err := xml.Unmarshal(xmlData, &rootNode); err != nil {
		return nil, err
	}
	d := &root.Document{Path: name, Format: "docx", Pagination: "layout-estimated"}
	var paragraphs []layout.Paragraph
	var floating []floatingContent
	pageHint := 0
	useRenderedBreaks := len(find(rootNode, "lastRenderedPageBreak")) > 0
	body := bodyParagraphs(rootNode)
	for paragraphIndex, p := range body {
		renderedBreaks := len(paragraphDescendants(p, "lastRenderedPageBreak"))
		base := root.TextRun{Size: 11, Color: "#000000"}
		item := layout.Paragraph{}
		sectionBreakAfter := false
		if pp, ok := child(p, "pPr"); ok {
			if rp, ok := child(pp, "rPr"); ok {
				base = runStyle(rp, base)
			}
			if spacing, ok := child(pp, "spacing"); ok {
				item.Before = twips(attr(spacing, "before"))
				item.After = twips(attr(spacing, "after"))
				if v := attr(spacing, "line"); v != "" {
					raw, _ := strconv.ParseFloat(v, 64)
					if attr(spacing, "lineRule") == "auto" {
						item.LineHeight = base.Size * raw / 240
					} else {
						item.LineHeight = raw / 20
					}
				}
			}
			if _, ok := child(pp, "pageBreakBefore"); ok {
				item.BreakBefore = true
			}
			if section, ok := child(pp, "sectPr"); ok {
				kind := "nextPage"
				if typ, ok := child(section, "type"); ok && attr(typ, "val") != "" {
					kind = attr(typ, "val")
				}
				if !useRenderedBreaks && kind != "continuous" {
					sectionBreakAfter = true
				}
			}
		}
		items := []layout.Paragraph{item}
		flowImageCount := 0
		for _, rn := range paragraphDescendants(p, "r") {
			if len(paragraphDescendants(rn, "lastRenderedPageBreak")) > 0 {
				current := &items[len(items)-1]
				if paragraphHasFlowContent(*current) {
					current.BreakAfter = true
					current.After = 0
					items = append(items, layout.Paragraph{LineHeight: item.LineHeight, After: item.After})
				} else {
					current.BreakBefore = true
				}
			}
			txt := textOf(rn)
			if txt != "" {
				style := base
				if rp, ok := child(rn, "rPr"); ok {
					style = runStyle(rp, style)
				}
				style.Text = txt
				current := &items[len(items)-1]
				current.Runs = append(current.Runs, style)
			}
			runInlineNodes := paragraphDescendants(rn, "inline")
			for _, inline := range runInlineNodes {
				current := &items[len(items)-1]
				images := containerImages(inline, rels, files)
				current.Images = append(current.Images, images...)
				flowImageCount += len(images)
			}
			if len(runInlineNodes) == 0 && len(paragraphDescendants(rn, "anchor")) == 0 {
				current := &items[len(items)-1]
				images := containerImages(rn, rels, files)
				current.Images = append(current.Images, images...)
				flowImageCount += len(images)
			}
		}
		inlineNodes := paragraphDescendants(p, "inline")
		anchorNodes := paragraphDescendants(p, "anchor")
		for _, anchor := range anchorNodes {
			floating = append(floating, floatingContent{
				images:   containerImages(anchor, rels, files),
				pageHint: pageHint,
			})
		}
		if flowImageCount == 0 && len(inlineNodes) == 0 && len(anchorNodes) == 0 {
			current := &items[len(items)-1]
			current.Images = append(current.Images, containerImages(p, rels, files)...)
		}
		for _, box := range find(p, "txbxContent") {
			floating = append(floating, floatingContent{
				paragraphs: textBoxParagraphs(box),
				pageHint:   pageHint,
			})
		}
		for _, br := range paragraphDescendants(p, "br") {
			if attr(br, "type") != "page" {
				continue
			}
			// Word commonly writes both an explicit page break and a
			// lastRenderedPageBreak at the start of the following paragraph.
			// They describe one boundary and must not create two pages.
			duplicatedByNext := useRenderedBreaks && paragraphIndex+1 < len(body) &&
				renderedBreakPrecedesContent(body[paragraphIndex+1])
			if !duplicatedByNext {
				items[len(items)-1].BreakAfter = true
			}
		}
		if sectionBreakAfter {
			items[len(items)-1].BreakAfter = true
		}
		paragraphs = append(paragraphs, items...)
		pageHint += renderedBreaks
	}
	options := documentLayout(rootNode)
	if app, e := read("docProps/app.xml"); e == nil {
		var props xmlNode
		if xml.Unmarshal(app, &props) == nil {
			if pages := find(props, "Pages"); len(pages) > 0 {
				options.TargetPages, _ = strconv.Atoi(strings.TrimSpace(pages[0].Text))
			}
		}
	}
	d.Pages = layout.Paginate(paragraphs, options)
	attachFloating(d.Pages, floating)
	return d, nil
}

func paragraphHasFlowContent(paragraph layout.Paragraph) bool {
	return len(paragraph.Runs) > 0 || len(paragraph.Images) > 0
}

func renderedBreakPrecedesContent(paragraph xmlNode) bool {
	foundContent := false
	var walk func(xmlNode) (bool, bool)
	walk = func(parent xmlNode) (foundBreak, stop bool) {
		for _, child := range parent.Nodes {
			switch child.XMLName.Local {
			case "lastRenderedPageBreak":
				return true, true
			case "t", "instrText":
				if strings.TrimSpace(child.Text) != "" {
					foundContent = true
				}
			case "drawing", "pict":
				foundContent = true
			}
			if foundContent {
				return false, true
			}
			if foundBreak, stop := walk(child); stop {
				return foundBreak, true
			}
		}
		return false, false
	}
	foundBreak, _ := walk(paragraph)
	return foundBreak
}

func containerImages(container xmlNode, rels map[string]string, files map[string][]byte) []root.Image {
	var images []root.Image
	for _, blip := range find(container, "blip") {
		target := rels[attr(blip, "embed")]
		data := files[target]
		if len(data) == 0 {
			continue
		}
		im := root.Image{Name: path.Base(target), Format: strings.TrimPrefix(path.Ext(target), "."), Data: data}
		if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
			im.Width, im.Height = cfg.Width, cfg.Height
		}
		if extents := find(container, "extent"); len(extents) > 0 {
			cx, _ := strconv.ParseFloat(attr(extents[0], "cx"), 64)
			cy, _ := strconv.ParseFloat(attr(extents[0], "cy"), 64)
			im.Bounds.Width = cx / 12700
			im.Bounds.Height = cy / 12700
		}
		images = append(images, im)
	}
	return images
}

func textBoxParagraphs(box xmlNode) []layout.Paragraph {
	var paragraphs []layout.Paragraph
	for _, p := range find(box, "p") {
		base := root.TextRun{Size: 11, Color: "#000000"}
		if pp, ok := child(p, "pPr"); ok {
			if rp, ok := child(pp, "rPr"); ok {
				base = runStyle(rp, base)
			}
		}
		item := layout.Paragraph{}
		for _, rn := range paragraphDescendants(p, "r") {
			text := textOf(rn)
			if text == "" {
				continue
			}
			style := base
			if rp, ok := child(rn, "rPr"); ok {
				style = runStyle(rp, style)
			}
			style.Text = text
			item.Runs = append(item.Runs, style)
		}
		if len(item.Runs) > 0 {
			paragraphs = append(paragraphs, item)
		}
	}
	return paragraphs
}

func attachFloating(pages []root.Page, floating []floatingContent) {
	for _, item := range floating {
		if len(pages) == 0 {
			return
		}
		pageIndex := item.pageHint
		if pageIndex < 0 {
			pageIndex = 0
		}
		if pageIndex >= len(pages) {
			pageIndex = len(pages) - 1
		}
		page := &pages[pageIndex]
		page.Images = append(page.Images, item.images...)
		if len(item.paragraphs) == 0 {
			continue
		}
		overlay := layout.Paginate(item.paragraphs, layout.Options{Width: page.Width, Height: page.Height, TargetPages: 1})
		if len(overlay) == 0 {
			continue
		}
		page.Runs = append(page.Runs, overlay[0].Runs...)
		if overlay[0].Text != "" {
			if page.Text != "" {
				page.Text += "\n"
			}
			page.Text += overlay[0].Text
		}
	}
}
func twips(s string) float64 { v, _ := strconv.ParseFloat(s, 64); return v / 20 }
func documentLayout(n xmlNode) layout.Options {
	o := layout.Options{}
	sects := find(n, "sectPr")
	if len(sects) == 0 {
		return o
	}
	s := sects[len(sects)-1]
	if size, ok := child(s, "pgSz"); ok {
		o.Width = twips(attr(size, "w"))
		o.Height = twips(attr(size, "h"))
	}
	if margin, ok := child(s, "pgMar"); ok {
		o.MarginTop = twips(attr(margin, "top"))
		o.MarginRight = twips(attr(margin, "right"))
		o.MarginBottom = twips(attr(margin, "bottom"))
		o.MarginLeft = twips(attr(margin, "left"))
	}
	return o
}
func child(n xmlNode, local string) (xmlNode, bool) {
	for _, c := range n.Nodes {
		if c.XMLName.Local == local {
			return c, true
		}
	}
	return xmlNode{}, false
}
func runStyle(rp xmlNode, r root.TextRun) root.TextRun {
	if s := attrFirst(rp, "sz"); s != "" {
		if v, e := strconv.ParseFloat(s, 64); e == nil {
			r.Size = v / 2
		}
	}
	if c := attrFirst(rp, "color"); c != "" && c != "auto" {
		r.Color = "#" + strings.ToUpper(c)
	}
	if f := attrFirst(rp, "rFonts"); f != "" {
		r.Font = f
	}
	return r
}
func attrFirst(n xmlNode, local string) string {
	for _, c := range n.Nodes {
		if c.XMLName.Local == local {
			if v := attr(c, "val"); v != "" {
				return v
			}
			return attr(c, "id")
		}
	}
	return ""
}
