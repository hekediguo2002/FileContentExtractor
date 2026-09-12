package ppt

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unicode/utf16"

	"filecontentextractor/internal/officeart"
	"filecontentextractor/internal/ole"
	"filecontentextractor/model"
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
		d.Pages = slides
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
		pages = append(pages, page)
		pos += 7 + size
	}
	return pages
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
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r", "\n"))
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
