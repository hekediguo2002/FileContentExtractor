package pdf

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"math"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/hekediguo2002/FileContentExtractor/model"
)

type object struct {
	num, gen   int
	dict, data []byte
}

var objRe = regexp.MustCompile(`(?s)(\d+)\s+(\d+)\s+obj\s*(.*?)\s*endobj`)
var pageRe = regexp.MustCompile(`(?s)/Type\s*/Page\b(.*?)(?:endobj)`)

// Untrusted input can declare absurd sizes: cap decompressed streams (a tiny
// Flate stream can expand without bound) and recursive reference walks (a
// pathological reference chain can exhaust the goroutine stack).
const (
	maxDecodedStream = 256 << 20
	maxRefDepth      = 1000
	// 单边像素上限：真实文档图片远小于此；超过即视为畸形并放弃像素合成。
	maxImageDimension = 1 << 15
)

func ParseFile(name string) (*model.Document, error) {
	b, e := os.ReadFile(name)
	if e != nil {
		return nil, e
	}
	return parse(name, b)
}
func parse(name string, b []byte) (*model.Document, error) {
	if !bytes.HasPrefix(bytes.TrimSpace(b), []byte("%PDF-")) {
		return nil, fmt.Errorf("not a PDF")
	}
	objs := map[int]object{}
	ms := objRe.FindAllSubmatch(b, -1)
	for _, m := range ms {
		n, _ := strconv.Atoi(string(m[1]))
		gen, _ := strconv.Atoi(string(m[2]))
		body := m[3]
		dict := body
		var data []byte
		if i := bytes.Index(body, []byte("stream")); i >= 0 {
			dict = bytes.TrimSpace(body[:i])
			s := i + 6
			if s < len(body) && body[s] == '\r' {
				s++
			}
			if s < len(body) && body[s] == '\n' {
				s++
			}
			if j := bytes.Index(body[s:], []byte("endstream")); j >= 0 {
				data = body[s : s+j]
			}
		}
		objs[n] = object{num: n, gen: gen, dict: dict, data: data}
	}
	if err := decryptPDFObjects(b, objs); err != nil {
		return nil, err
	}
	// PDF 1.5 object streams keep page dictionaries (and sometimes content
	// streams) compressed inside an /ObjStm. Expand from a stable snapshot.
	// Some producers embed
	// object-stream-like bytes in extracted objects; ranging over and mutating
	// the same map can otherwise repeatedly expand data and exhaust memory.
	objStreams := make([]object, 0, len(objs))
	for _, o := range objs {
		objStreams = append(objStreams, o)
	}
	for _, o := range objStreams {
		if !regexp.MustCompile(`/Type\s*/ObjStm\b`).Match(o.dict) {
			continue
		}
		n := int(dictNum(o.dict, "N"))
		first := int(dictNum(o.dict, "First"))
		raw := decodeStream(o.dict, o.data)
		// A malformed dictionary must not turn into an unbounded allocation or
		// loop. Object streams in normal files are far smaller than this limit.
		if n <= 0 || n > 100000 || first <= 0 || first > len(raw) {
			continue
		}
		head := strings.Fields(string(raw[:first]))
		if len(head) < n*2 {
			continue
		}
		for i := 0; i < n; i++ {
			num, _ := strconv.Atoi(head[i*2])
			off, _ := strconv.Atoi(head[i*2+1])
			end := len(raw)
			if i+1 < n {
				end, _ = strconv.Atoi(head[(i+1)*2+1])
			}
			// Compare with len(raw)-first instead of first+off: offsets from
			// the file can be near maxInt, and adding first could overflow to
			// a negative value that slips past the bounds check.
			if off < 0 || off > len(raw)-first {
				continue
			}
			if end < off {
				end = off
			}
			if end > len(raw)-first {
				end = len(raw) - first
			}
			body := bytes.TrimSpace(raw[first+off : first+end])
			// Explicit objects take precedence. Incremental PDFs may retain an
			// older compressed object with the same number; overwriting the
			// explicit font dictionary can discard its ToUnicode mapping.
			if _, exists := objs[num]; !exists {
				objs[num] = object{num: num, dict: body, data: body}
			}
		}
	}
	d := &model.Document{Path: name, Format: "pdf", Pagination: "native"}
	pages := orderedPages(objs)
	typePage := regexp.MustCompile(`/Type\s*/Page\b`)
	typePages := regexp.MustCompile(`/Type\s*/Pages\b`)
	if len(pages) == 0 {
		for _, o := range objs {
			if typePage.Match(o.dict) && !typePages.Match(o.dict) {
				pages = append(pages, o)
			}
		}
		sortObjects(pages)
	}
	for i, p := range pages {
		pg := model.Page{Number: i + 1, Width: 595, Height: 842}
		if m := regexp.MustCompile(`/MediaBox\s*\[([^]]+)\]`).FindSubmatch(p.dict); len(m) > 1 {
			v := nums(string(m[1]))
			if len(v) >= 4 {
				pg.Width = v[2] - v[0]
				pg.Height = v[3] - v[1]
			}
		}
		resources := pageResourceData(p, objs)
		fonts := pageFonts(resources, objs)
		var content []byte
		for _, n := range contentRefs(p.dict) {
			if o, ok := objs[n]; ok {
				content = append(content, decodeStream(o.dict, o.data)...)
				content = append(content, '\n')
			}
		}
		runs, textClips := parseText(content, fonts)
		pg.Runs = runs
		segments := parsePathSegments(content)
		tables, tableRuns := detectTables(pg.Runs, textClips, segments)
		if len(tables) == 0 {
			tables, tableRuns = detectClipTables(pg.Runs, textClips, segments, 3)
		}
		pg.Tables = tables
		pg.Text = pageTextWithTables(pg.Runs, pg.Tables, tableRuns)
		pg.Images = pageImages(resources, objs)
		d.Pages = append(d.Pages, pg)
	}
	if len(d.Pages) == 0 {
		return nil, fmt.Errorf("PDF has no page objects")
	}
	return d, nil
}

func pageResourceData(page object, objs map[int]object) []byte {
	var out []byte
	seen := map[int]bool{}
	current := page
	for {
		out = append(out, current.dict...)
		out = append(out, '\n')
		if m := regexp.MustCompile(`/Resources\s+(\d+)\s+\d+\s+R`).FindSubmatch(current.dict); len(m) > 1 {
			id, _ := strconv.Atoi(string(m[1]))
			if resource, ok := objs[id]; ok {
				out = append(out, resource.dict...)
				out = append(out, '\n')
			}
		}
		m := regexp.MustCompile(`/Parent\s+(\d+)\s+\d+\s+R`).FindSubmatch(current.dict)
		if len(m) < 2 {
			break
		}
		id, _ := strconv.Atoi(string(m[1]))
		if seen[id] {
			break
		}
		seen[id] = true
		parent, ok := objs[id]
		if !ok {
			break
		}
		current = parent
	}
	return out
}

func pageImages(resources []byte, objs map[int]object) []model.Image {
	var images []model.Image
	seen := map[int]bool{}
	var visit func([]byte, int)
	visit = func(dict []byte, depth int) {
		if depth > maxRefDepth {
			return
		}
		for _, id := range resourceRefs(dict, "XObject", objs) {
			if seen[id] {
				continue
			}
			seen[id] = true
			o, ok := objs[id]
			if !ok {
				continue
			}
			if regexp.MustCompile(`/Subtype\s*/Image\b`).Match(o.dict) {
				images = append(images, extractImage(o, objs))
				continue
			}
			if regexp.MustCompile(`/Subtype\s*/Form\b`).Match(o.dict) {
				visit(o.dict, depth+1)
			}
		}
	}
	visit(resources, 0)
	return images
}

func extractImage(o object, objs map[int]object) model.Image {
	w := int(dictNum(o.dict, "Width"))
	h := int(dictNum(o.dict, "Height"))
	format := imageFormat(o.dict)
	data := decodeStream(o.dict, o.data)
	result := model.Image{Name: fmt.Sprintf("%d", o.num), Format: format, Width: w, Height: h, Data: data}
	if strings.EqualFold(format, "DCTDecode") {
		return applyJPEGSoftMask(result, o, objs)
	}
	if !strings.EqualFold(format, "FlateDecode") || w <= 0 || h <= 0 || int(dictNum(o.dict, "BitsPerComponent")) != 8 {
		return result
	}
	channels := imageChannels(o.dict, objs)
	// Untrusted Width/Height can be near maxInt: cap each dimension so the
	// int64 product below cannot overflow and image.NewNRGBA cannot panic.
	if channels == 0 || w > maxImageDimension || h > maxImageDimension || int64(w)*int64(h)*int64(channels) > int64(len(data)) {
		return result
	}
	var alpha []byte
	if m := regexp.MustCompile(`/SMask\s+(\d+)\s+\d+\s+R`).FindSubmatch(o.dict); len(m) > 1 {
		n, _ := strconv.Atoi(string(m[1]))
		if mask, ok := objs[n]; ok {
			alpha = decodeStream(mask.dict, mask.data)
		}
	}
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	for i := 0; i < w*h; i++ {
		src := i * channels
		dst := i * 4
		switch channels {
		case 1:
			im.Pix[dst], im.Pix[dst+1], im.Pix[dst+2] = data[src], data[src], data[src]
		case 3:
			copy(im.Pix[dst:dst+3], data[src:src+3])
		case 4:
			r, g, b := color.CMYKToRGB(data[src], data[src+1], data[src+2], data[src+3])
			im.Pix[dst], im.Pix[dst+1], im.Pix[dst+2] = r, g, b
		}
		im.Pix[dst+3] = 255
		if len(alpha) >= w*h {
			im.Pix[dst+3] = alpha[i]
		}
	}
	var encoded bytes.Buffer
	if png.Encode(&encoded, im) == nil {
		result.Format = "png"
		result.Data = encoded.Bytes()
	}
	return result
}

func imageChannels(dict []byte, objs map[int]object) int {
	switch {
	case regexp.MustCompile(`/ColorSpace\s*/DeviceRGB\b`).Match(dict):
		return 3
	case regexp.MustCompile(`/ColorSpace\s*/DeviceGray\b`).Match(dict):
		return 1
	case regexp.MustCompile(`/ColorSpace\s*/DeviceCMYK\b`).Match(dict):
		return 4
	}
	match := regexp.MustCompile(`/ColorSpace\s*\[\s*/ICCBased\s+(\d+)\s+\d+\s+R`).FindSubmatch(dict)
	if len(match) < 2 {
		return 0
	}
	id, _ := strconv.Atoi(string(match[1]))
	profile, ok := objs[id]
	if !ok {
		return 0
	}
	channels := int(dictNum(profile.dict, "N"))
	if channels == 1 || channels == 3 || channels == 4 {
		return channels
	}
	return 0
}

func applyJPEGSoftMask(result model.Image, o object, objs map[int]object) model.Image {
	m := regexp.MustCompile(`/SMask\s+(\d+)\s+\d+\s+R`).FindSubmatch(o.dict)
	if len(m) < 2 {
		return result
	}
	id, _ := strconv.Atoi(string(m[1]))
	mask, ok := objs[id]
	if !ok || int(dictNum(mask.dict, "BitsPerComponent")) != 8 {
		return result
	}
	alpha := decodeStream(mask.dict, mask.data)
	mw := int(dictNum(mask.dict, "Width"))
	mh := int(dictNum(mask.dict, "Height"))
	base, err := jpeg.Decode(bytes.NewReader(o.data))
	if err != nil || mw <= 0 || mh <= 0 || mw > maxImageDimension || mh > maxImageDimension || int64(mw)*int64(mh) > int64(len(alpha)) {
		return result
	}
	bounds := base.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return result
	}
	im := image.NewNRGBA(image.Rect(0, 0, w, h))
	matteR, matteG, matteB, hasMatte := matteRGB(mask.dict)
	for y := 0; y < h; y++ {
		my := y * mh / h
		for x := 0; x < w; x++ {
			mx := x * mw / w
			c, ok := color.NRGBAModel.Convert(base.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			if !ok {
				continue
			}
			a := int(alpha[my*mw+mx])
			if hasMatte {
				// Undo the declared premultiplication while compositing onto
				// white: C' + (1-alpha)*(white-matte).
				c.R = byte(minInt(255, int(c.R)+(255-a)*(255-matteR)/255))
				c.G = byte(minInt(255, int(c.G)+(255-a)*(255-matteG)/255))
				c.B = byte(minInt(255, int(c.B)+(255-a)*(255-matteB)/255))
			} else {
				c.R = byte((int(c.R)*a + 255*(255-a) + 127) / 255)
				c.G = byte((int(c.G)*a + 255*(255-a) + 127) / 255)
				c.B = byte((int(c.B)*a + 255*(255-a) + 127) / 255)
			}
			c.A = 255
			im.SetNRGBA(x, y, c)
		}
	}
	var encoded bytes.Buffer
	if png.Encode(&encoded, im) != nil {
		return result
	}
	result.Format = "png"
	result.Width = w
	result.Height = h
	result.Data = encoded.Bytes()
	return result
}

func matteRGB(dict []byte) (int, int, int, bool) {
	m := regexp.MustCompile(`/Matte\s*\[([^]]+)\]`).FindSubmatch(dict)
	if len(m) < 2 {
		return 0, 0, 0, false
	}
	values := nums(string(m[1]))
	if len(values) == 1 {
		value := minInt(255, int(values[0]*255+.5))
		return value, value, value, true
	}
	if len(values) < 3 {
		return 0, 0, 0, false
	}
	return minInt(255, int(values[0]*255+.5)),
		minInt(255, int(values[1]*255+.5)),
		minInt(255, int(values[2]*255+.5)), true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func orderedPages(objs map[int]object) []object {
	var root int
	cat := regexp.MustCompile(`/Type\s*/Catalog\b`)
	pagesRef := regexp.MustCompile(`/Pages\s+(\d+)\s+\d+\s+R`)
	for _, o := range objs {
		if cat.Match(o.dict) {
			if m := pagesRef.FindSubmatch(o.dict); len(m) > 1 {
				root, _ = strconv.Atoi(string(m[1]))
				break
			}
		}
	}
	if root == 0 {
		return nil
	}
	var out []object
	seen := map[int]bool{}
	var walk func(int, int)
	walk = func(id, depth int) {
		if depth > maxRefDepth || seen[id] {
			return
		}
		seen[id] = true
		o, ok := objs[id]
		if !ok {
			return
		}
		if regexp.MustCompile(`/Type\s*/Page\b`).Match(o.dict) {
			out = append(out, o)
			return
		}
		m := regexp.MustCompile(`(?s)/Kids\s*\[(.*?)\]`).FindSubmatch(o.dict)
		if len(m) < 2 {
			return
		}
		for _, r := range regexp.MustCompile(`(\d+)\s+\d+\s+R`).FindAllSubmatch(m[1], -1) {
			n, _ := strconv.Atoi(string(r[1]))
			walk(n, depth+1)
		}
	}
	walk(root, 0)
	return out
}

func contentRefs(d []byte) []int {
	var out []int
	single := regexp.MustCompile(`/Contents\s+(\d+)\s+\d+\s+R`)
	if m := single.FindSubmatch(d); len(m) > 1 {
		n, _ := strconv.Atoi(string(m[1]))
		return []int{n}
	}
	array := regexp.MustCompile(`(?s)/Contents\s*\[(.*?)\]`).FindSubmatch(d)
	if len(array) > 1 {
		re := regexp.MustCompile(`(\d+)\s+\d+\s+R`)
		for _, m := range re.FindAllSubmatch(array[1], -1) {
			n, _ := strconv.Atoi(string(m[1]))
			out = append(out, n)
		}
	}
	return out
}
func resourceRefs(d []byte, kind string, objs map[int]object) []int {
	var dictionaries [][]byte
	if direct := regexp.MustCompile(`(?s)/` + kind + `\s*<<(.+?)>>`).FindSubmatch(d); len(direct) > 1 {
		dictionaries = append(dictionaries, direct[1])
	}
	// Scanner-generated PDFs commonly store /XObject as a reference to a
	// separate resource dictionary rather than an inline <<...>> dictionary.
	indirect := regexp.MustCompile(`/` + kind + `\s+(\d+)\s+\d+\s+R`)
	for _, match := range indirect.FindAllSubmatch(d, -1) {
		id, _ := strconv.Atoi(string(match[1]))
		if resource, ok := objs[id]; ok {
			dictionaries = append(dictionaries, resource.dict)
		}
	}
	ref := regexp.MustCompile(`/[^\s/<>()\[\]]+\s+(\d+)\s+\d+\s+R`)
	seen := map[int]bool{}
	var out []int
	for _, dictionary := range dictionaries {
		for _, match := range ref.FindAllSubmatch(dictionary, -1) {
			id, _ := strconv.Atoi(string(match[1]))
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}
func pageText(runs []model.TextRun) string {
	var b strings.Builder
	lastY := math.NaN()
	lastEnd := 0.0
	lastSize := 0.0
	for _, r := range runs {
		if r.Text == "" {
			continue
		}
		if !math.IsNaN(lastY) {
			if math.Abs(r.Bounds.Y-lastY) > math.Max(lastSize, r.Size)*.55 {
				b.WriteByte('\n')
			} else if r.Bounds.X-lastEnd > math.Max(lastSize, r.Size)*.2 {
				b.WriteByte(' ')
			}
		}
		b.WriteString(r.Text)
		lastY = r.Bounds.Y
		lastEnd = r.Bounds.X + r.Bounds.Width
		lastSize = r.Size
	}
	return strings.TrimSpace(b.String())
}

type pathSegment struct {
	x1, y1, x2, y2 float64
}

func parsePathSegments(data []byte) []pathSegment {
	tokens := tokenize(string(data))
	var path, rectangles []pathSegment
	var result []pathSegment
	var currentX, currentY, startX, startY float64
	haveCurrent := false
	number := func(index int) float64 {
		if index < 0 || index >= len(tokens) {
			return 0
		}
		value, _ := strconv.ParseFloat(tokens[index], 64)
		return value
	}
	for i, token := range tokens {
		switch token {
		case "cm":
			// parseText currently reports coordinates in the content stream's
			// local coordinate system. Keep ruling lines in that same system;
			// applying the page CTM here would vertically mirror one side only.
			continue
		case "m":
			if i >= 2 {
				currentX, currentY = number(i-2), number(i-1)
				startX, startY = currentX, currentY
				haveCurrent = true
			}
		case "l":
			if i >= 2 && haveCurrent {
				x, y := number(i-2), number(i-1)
				path = append(path, pathSegment{currentX, currentY, x, y})
				currentX, currentY = x, y
			}
		case "h":
			if haveCurrent {
				path = append(path, pathSegment{currentX, currentY, startX, startY})
				currentX, currentY = startX, startY
			}
		case "re":
			if i >= 4 {
				x, y := number(i-4), number(i-3)
				w, h := number(i-2), number(i-1)
				x1, y1 := x, y
				x2, y2 := x+w, y
				x3, y3 := x+w, y+h
				x4, y4 := x, y+h
				rectangles = []pathSegment{{x1, y1, x2, y2}, {x2, y2, x3, y3}, {x3, y3, x4, y4}, {x4, y4, x1, y1}}
				path = append(path, rectangles...)
			}
		case "S", "s", "B", "B*", "b", "b*":
			result = append(result, path...)
			path = nil
			rectangles = nil
			haveCurrent = false
		case "f", "f*", "F":
			// Thin filled rectangles are commonly used instead of stroked
			// paths for table rules.
			if len(rectangles) == 4 {
				width := math.Hypot(rectangles[0].x2-rectangles[0].x1, rectangles[0].y2-rectangles[0].y1)
				height := math.Hypot(rectangles[1].x2-rectangles[1].x1, rectangles[1].y2-rectangles[1].y1)
				if width <= 3 || height <= 3 {
					result = append(result, rectangles...)
				}
			}
			path = nil
			rectangles = nil
			haveCurrent = false
		case "n":
			path = nil
			rectangles = nil
			haveCurrent = false
		}
	}
	return result
}

type ruledLine struct {
	coordinate float64
	from, to   float64
}

func detectTables(runs []model.TextRun, clips []*model.Rect, segments []pathSegment) ([]model.Table, map[int]int) {
	const (
		axisTolerance = 3.0
		maxRuleCount  = 10000
		maxGridLevels = 512
		maxTableCells = 4096
	)
	var horizontal, vertical []ruledLine
	for _, segment := range segments {
		dx, dy := math.Abs(segment.x2-segment.x1), math.Abs(segment.y2-segment.y1)
		switch {
		case dy <= axisTolerance && dx >= 4:
			horizontal = append(horizontal, ruledLine{coordinate: (segment.y1 + segment.y2) / 2, from: math.Min(segment.x1, segment.x2), to: math.Max(segment.x1, segment.x2)})
		case dx <= axisTolerance && dy >= 4:
			vertical = append(vertical, ruledLine{coordinate: (segment.x1 + segment.x2) / 2, from: math.Min(segment.y1, segment.y2), to: math.Max(segment.y1, segment.y2)})
		}
	}
	horizontal = mergeRuledLines(horizontal, axisTolerance)
	vertical = mergeRuledLines(vertical, axisTolerance)
	if len(horizontal) < 2 || len(vertical) < 2 || len(horizontal) > maxRuleCount || len(vertical) > maxRuleCount {
		return nil, map[int]int{}
	}
	yLevels := uniqueCoordinates(horizontal, axisTolerance)
	if len(yLevels) > maxGridLevels {
		return nil, map[int]int{}
	}
	var cells []model.TableCell
	for bottomIndex := 0; bottomIndex+1 < len(yLevels); bottomIndex++ {
		for topIndex := bottomIndex + 1; topIndex < len(yLevels); topIndex++ {
			bottom, top := yLevels[bottomIndex], yLevels[topIndex]
			if top-bottom < 3 {
				continue
			}
			var xs []float64
			for _, line := range vertical {
				if covers(line.from, line.to, bottom, top, axisTolerance) {
					xs = append(xs, line.coordinate)
				}
			}
			xs = uniqueFloat64s(xs, axisTolerance)
			// Word commonly draws table rows with horizontal rules and internal
			// column dividers while omitting the outer left/right borders. Infer
			// those two boundaries from matching top and bottom rule endpoints.
			if len(xs) > 0 {
				bottomLeft, bottomRight, bottomOK := enclosingHorizontalSpan(horizontal, bottom, xs, axisTolerance)
				topLeft, topRight, topOK := enclosingHorizontalSpan(horizontal, top, xs, axisTolerance)
				if bottomOK && topOK {
					left := math.Max(bottomLeft, topLeft)
					right := math.Min(bottomRight, topRight)
					if right-left >= 4 {
						xs = uniqueFloat64s(append(xs, left, right), axisTolerance)
					}
				}
			}
			for xi := 0; xi+1 < len(xs); xi++ {
				left, right := xs[xi], xs[xi+1]
				if right-left < 3 ||
					!hasCoverage(horizontal, bottom, left, right, axisTolerance) ||
					!hasCoverage(horizontal, top, left, right, axisTolerance) ||
					hasInternalCoverage(horizontal, yLevels, bottomIndex, topIndex, left, right, axisTolerance) {
					continue
				}
				cells = append(cells, model.TableCell{
					Bounds:  model.Rect{X: left, Y: bottom, Width: right - left, Height: top - bottom},
					RowSpan: topIndex - bottomIndex, ColSpan: 1,
				})
				if len(cells) > maxTableCells {
					return nil, map[int]int{}
				}
			}
		}
	}
	components := cellComponents(cells, axisTolerance)
	tableRuns := map[int]int{}
	var tables []model.Table
	for _, component := range components {
		if len(component) < 2 {
			continue
		}
		table, assigned := buildPDFTable(component, runs, clips, axisTolerance)
		if len(table.Rows) >= 2 || tableColumnCount(table) >= 2 {
			tableIndex := len(tables)
			tables = append(tables, table)
			for runIndex := range assigned {
				tableRuns[runIndex] = tableIndex
			}
		}
	}
	return tables, tableRuns
}

func enclosingHorizontalSpan(lines []ruledLine, coordinate float64, dividers []float64, tolerance float64) (float64, float64, bool) {
	if len(dividers) == 0 {
		return 0, 0, false
	}
	leftDivider, rightDivider := dividers[0], dividers[len(dividers)-1]
	bestLeft, bestRight := 0.0, 0.0
	found := false
	for _, line := range lines {
		if math.Abs(line.coordinate-coordinate) > tolerance ||
			line.from > leftDivider+tolerance || line.to < rightDivider-tolerance {
			continue
		}
		if !found || line.to-line.from > bestRight-bestLeft {
			bestLeft, bestRight, found = line.from, line.to, true
		}
	}
	return bestLeft, bestRight, found
}

func detectClipTables(runs []model.TextRun, clips []*model.Rect, segments []pathSegment, tolerance float64) ([]model.Table, map[int]int) {
	var candidates []model.TableCell
	for runIndex, clip := range clips {
		if clip == nil || clip.Width < 8 || clip.Height < 6 || runIndex >= len(runs) {
			continue
		}
		run := runs[runIndex]
		x := run.Bounds.X + run.Bounds.Width/2
		y := run.Bounds.Y + run.Bounds.Height/2
		if x < clip.X-tolerance || x > clip.X+clip.Width+tolerance || y < clip.Y-tolerance || y > clip.Y+clip.Height+tolerance {
			continue
		}
		duplicate := false
		for _, existing := range candidates {
			if rectNearlyEqual(existing.Bounds, *clip, tolerance) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			candidates = append(candidates, model.TableCell{Bounds: *clip, RowSpan: 1, ColSpan: 1})
		}
	}
	filtered := candidates[:0]
	for index, candidate := range candidates {
		container := false
		candidateArea := candidate.Bounds.Width * candidate.Bounds.Height
		for otherIndex, other := range candidates {
			if index == otherIndex {
				continue
			}
			otherArea := other.Bounds.Width * other.Bounds.Height
			if candidateArea > otherArea*1.5 && rectContains(candidate.Bounds, other.Bounds, tolerance) {
				container = true
				break
			}
		}
		if !container {
			filtered = append(filtered, candidate)
		}
	}

	components := cellComponents(filtered, tolerance)
	assignedTables := map[int]int{}
	var tables []model.Table
	for _, component := range components {
		if len(component) < 4 {
			continue
		}
		table, assigned := buildPDFTable(component, runs, clips, tolerance)
		columns := tableColumnCount(table)
		rowsWithMultipleCells := 0
		for _, row := range table.Rows {
			if len(row.Cells) >= 2 {
				rowsWithMultipleCells++
			}
		}
		if len(table.Rows) < 2 || columns < 2 || columns > 20 || rowsWithMultipleCells < 2 ||
			!clipTableHasRules(table.Bounds, segments, tolerance) {
			continue
		}
		tableIndex := len(tables)
		tables = append(tables, table)
		for runIndex := range assigned {
			assignedTables[runIndex] = tableIndex
		}
	}
	return tables, assignedTables
}

func clipTableHasRules(bounds model.Rect, segments []pathSegment, tolerance float64) bool {
	var horizontal, vertical []ruledLine
	for _, segment := range segments {
		dx, dy := math.Abs(segment.x2-segment.x1), math.Abs(segment.y2-segment.y1)
		switch {
		case dy <= tolerance && dx >= 4:
			horizontal = append(horizontal, ruledLine{coordinate: (segment.y1 + segment.y2) / 2, from: math.Min(segment.x1, segment.x2), to: math.Max(segment.x1, segment.x2)})
		case dx <= tolerance && dy >= 4:
			vertical = append(vertical, ruledLine{coordinate: (segment.x1 + segment.x2) / 2, from: math.Min(segment.y1, segment.y2), to: math.Max(segment.y1, segment.y2)})
		}
	}
	horizontal = mergeRuledLines(horizontal, tolerance)
	vertical = mergeRuledLines(vertical, tolerance)
	horizontalCount := 0
	for _, line := range horizontal {
		if line.coordinate >= bounds.Y-tolerance && line.coordinate <= bounds.Y+bounds.Height+tolerance &&
			line.from <= bounds.X+tolerance && line.to >= bounds.X+bounds.Width-tolerance {
			horizontalCount++
		}
	}
	if horizontalCount < 2 {
		return false
	}
	for _, line := range vertical {
		inside := line.coordinate > bounds.X+tolerance && line.coordinate < bounds.X+bounds.Width-tolerance
		overlap := math.Min(line.to, bounds.Y+bounds.Height) - math.Max(line.from, bounds.Y)
		if inside && overlap >= math.Min(bounds.Height*.25, 12) {
			return true
		}
	}
	return false
}

func rectNearlyEqual(a, b model.Rect, tolerance float64) bool {
	return math.Abs(a.X-b.X) <= tolerance && math.Abs(a.Y-b.Y) <= tolerance &&
		math.Abs(a.Width-b.Width) <= tolerance && math.Abs(a.Height-b.Height) <= tolerance
}

func rectContains(outer, inner model.Rect, tolerance float64) bool {
	return outer.X <= inner.X+tolerance && outer.Y <= inner.Y+tolerance &&
		outer.X+outer.Width >= inner.X+inner.Width-tolerance &&
		outer.Y+outer.Height >= inner.Y+inner.Height-tolerance
}

func mergeRuledLines(lines []ruledLine, tolerance float64) []ruledLine {
	sort.Slice(lines, func(i, j int) bool {
		if math.Abs(lines[i].coordinate-lines[j].coordinate) > tolerance {
			return lines[i].coordinate < lines[j].coordinate
		}
		return lines[i].from < lines[j].from
	})
	var merged []ruledLine
	for _, line := range lines {
		if line.from > line.to {
			line.from, line.to = line.to, line.from
		}
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			if math.Abs(last.coordinate-line.coordinate) <= tolerance && line.from <= last.to+tolerance {
				last.coordinate = (last.coordinate + line.coordinate) / 2
				last.to = math.Max(last.to, line.to)
				continue
			}
		}
		merged = append(merged, line)
	}
	return merged
}

func uniqueCoordinates(lines []ruledLine, tolerance float64) []float64 {
	values := make([]float64, 0, len(lines))
	for _, line := range lines {
		values = append(values, line.coordinate)
	}
	return uniqueFloat64s(values, tolerance)
}

func uniqueFloat64s(values []float64, tolerance float64) []float64 {
	sort.Float64s(values)
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || math.Abs(value-result[len(result)-1]) > tolerance {
			result = append(result, value)
		} else {
			result[len(result)-1] = (result[len(result)-1] + value) / 2
		}
	}
	return result
}

func covers(from, to, wantedFrom, wantedTo, tolerance float64) bool {
	return from <= wantedFrom+tolerance && to >= wantedTo-tolerance
}

func hasCoverage(lines []ruledLine, coordinate, from, to, tolerance float64) bool {
	for _, line := range lines {
		if math.Abs(line.coordinate-coordinate) <= tolerance && covers(line.from, line.to, from, to, tolerance) {
			return true
		}
	}
	return false
}

func hasInternalCoverage(lines []ruledLine, levels []float64, bottomIndex, topIndex int, from, to, tolerance float64) bool {
	for index := bottomIndex + 1; index < topIndex; index++ {
		if hasCoverage(lines, levels[index], from, to, tolerance) {
			return true
		}
	}
	return false
}

func cellComponents(cells []model.TableCell, tolerance float64) [][]model.TableCell {
	visited := make([]bool, len(cells))
	var components [][]model.TableCell
	for start := range cells {
		if visited[start] {
			continue
		}
		visited[start] = true
		queue := []int{start}
		var component []model.TableCell
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			component = append(component, cells[current])
			for next := range cells {
				if !visited[next] && cellsTouch(cells[current].Bounds, cells[next].Bounds, tolerance) {
					visited[next] = true
					queue = append(queue, next)
				}
			}
		}
		components = append(components, component)
	}
	return components
}

func cellsTouch(a, b model.Rect, tolerance float64) bool {
	xOverlap := math.Min(a.X+a.Width, b.X+b.Width)-math.Max(a.X, b.X) >= -tolerance
	yOverlap := math.Min(a.Y+a.Height, b.Y+b.Height)-math.Max(a.Y, b.Y) >= -tolerance
	return xOverlap && yOverlap
}

func buildPDFTable(cells []model.TableCell, runs []model.TextRun, clips []*model.Rect, tolerance float64) (model.Table, map[int]bool) {
	ascendingY := tableTextRunsAscend(cells, runs, tolerance)
	sort.Slice(cells, func(i, j int) bool {
		if math.Abs(cells[i].Bounds.Y-cells[j].Bounds.Y) > tolerance {
			if ascendingY {
				return cells[i].Bounds.Y < cells[j].Bounds.Y
			}
			return cells[i].Bounds.Y > cells[j].Bounds.Y
		}
		return cells[i].Bounds.X < cells[j].Bounds.X
	})
	var table model.Table
	for _, source := range cells {
		rowIndex := -1
		top := source.Bounds.Y
		for i := range table.Rows {
			if len(table.Rows[i].Cells) > 0 {
				existing := table.Rows[i].Cells[0].Bounds
				if math.Abs(existing.Y-top) <= tolerance {
					rowIndex = i
					break
				}
			}
		}
		if rowIndex < 0 {
			table.Rows = append(table.Rows, model.TableRow{})
			rowIndex = len(table.Rows) - 1
		}
		source.Row = rowIndex
		table.Rows[rowIndex].Cells = append(table.Rows[rowIndex].Cells, source)
		table.Bounds = unionPDFRect(table.Bounds, source.Bounds)
	}
	var columns []float64
	for rowIndex := range table.Rows {
		sort.Slice(table.Rows[rowIndex].Cells, func(i, j int) bool {
			return table.Rows[rowIndex].Cells[i].Bounds.X < table.Rows[rowIndex].Cells[j].Bounds.X
		})
		for _, cell := range table.Rows[rowIndex].Cells {
			columns = append(columns, cell.Bounds.X)
			columns = append(columns, cell.Bounds.X+cell.Bounds.Width)
		}
	}
	columns = uniqueFloat64s(columns, tolerance)
	assigned := map[int]bool{}
	for rowIndex := range table.Rows {
		for cellIndex := range table.Rows[rowIndex].Cells {
			cell := &table.Rows[rowIndex].Cells[cellIndex]
			cell.Column = nearestCoordinate(columns, cell.Bounds.X)
			right := nearestCoordinate(columns, cell.Bounds.X+cell.Bounds.Width)
			cell.ColSpan = maxInt(right-cell.Column, 1)
		}
	}
	for runIndex, run := range runs {
		bestRow, bestCell := -1, -1
		bestScore := 0.0
		var clip *model.Rect
		if runIndex < len(clips) {
			clip = clips[runIndex]
		}
		for rowIndex := range table.Rows {
			for cellIndex := range table.Rows[rowIndex].Cells {
				score := runCellScore(run, clip, table.Rows[rowIndex].Cells[cellIndex].Bounds, tolerance)
				if score > bestScore {
					bestScore, bestRow, bestCell = score, rowIndex, cellIndex
				}
			}
		}
		if bestRow >= 0 {
			cell := &table.Rows[bestRow].Cells[bestCell]
			cell.Runs = append(cell.Runs, run)
			assigned[runIndex] = true
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

func tableTextRunsAscend(cells []model.TableCell, runs []model.TextRun, tolerance float64) bool {
	var bounds model.Rect
	for _, cell := range cells {
		bounds = unionPDFRect(bounds, cell.Bounds)
	}
	firstY, lastY := 0.0, 0.0
	found := false
	for _, run := range runs {
		x := run.Bounds.X + math.Min(run.Bounds.Width/2, math.Max(run.Size*.25, 1))
		y := run.Bounds.Y + run.Bounds.Height/2
		if x < bounds.X-tolerance || x > bounds.X+bounds.Width+tolerance || y < bounds.Y-tolerance || y > bounds.Y+bounds.Height+tolerance {
			continue
		}
		if !found {
			firstY = y
			found = true
		}
		lastY = y
	}
	return !found || lastY >= firstY
}

func nearestCoordinate(values []float64, wanted float64) int {
	best := 0
	for i := 1; i < len(values); i++ {
		if math.Abs(values[i]-wanted) < math.Abs(values[best]-wanted) {
			best = i
		}
	}
	return best
}

func runCellScore(run model.TextRun, clip *model.Rect, cell model.Rect, tolerance float64) float64 {
	x := run.Bounds.X + math.Min(run.Bounds.Width/2, math.Max(run.Size*.25, 1))
	y := run.Bounds.Y + run.Bounds.Height/2
	if clip != nil && clip.Width > 0 && clip.Height > 0 {
		overlap := intersectionArea(*clip, cell)
		clipArea := clip.Width * clip.Height
		cellArea := cell.Width * cell.Height
		if overlap > 0 && clipArea > 0 && cellArea > 0 {
			clipCoverage := overlap / clipArea
			cellCoverage := overlap / cellArea
			// Some PDF generators position glyphs in a scaled local coordinate
			// system but clip the text to the table cell in page coordinates.
			// A clip that substantially matches one cell is therefore stronger
			// evidence than a glyph position that appears in an adjacent cell.
			if clipCoverage >= .8 && cellCoverage >= .5 {
				return 3 + clipCoverage + cellCoverage
			}
		}
	}
	textTolerance := math.Min(tolerance, math.Max(run.Size*.05, .25))
	if x < cell.X-textTolerance || x > cell.X+cell.Width+textTolerance || y < cell.Y-textTolerance || y > cell.Y+cell.Height+textTolerance {
		return 0
	}
	score := 1.0
	if clip != nil && clip.Width > 0 && clip.Height > 0 {
		overlap := intersectionArea(*clip, cell)
		clipArea := clip.Width * clip.Height
		if overlap > 0 && clipArea > 0 {
			score += overlap / clipArea
		}
	}
	// Prefer the smallest enclosing rectangle at merged-cell boundaries.
	score += 1 / math.Max(cell.Width*cell.Height, 1)
	return score
}

func intersectionArea(a, b model.Rect) float64 {
	width := math.Min(a.X+a.Width, b.X+b.Width) - math.Max(a.X, b.X)
	height := math.Min(a.Y+a.Height, b.Y+b.Height) - math.Max(a.Y, b.Y)
	if width <= 0 || height <= 0 {
		return 0
	}
	return width * height
}

func tableColumnCount(table model.Table) int {
	maximum := 0
	for _, row := range table.Rows {
		for _, cell := range row.Cells {
			maximum = maxInt(maximum, cell.Column+cell.ColSpan)
		}
	}
	return maximum
}

func pageTextWithTables(runs []model.TextRun, tables []model.Table, tableRuns map[int]int) string {
	var values []string
	var plain []model.TextRun
	emitted := map[int]bool{}
	flushPlain := func() {
		if text := pageText(plain); text != "" {
			values = append(values, text)
		}
		plain = nil
	}
	for runIndex, run := range runs {
		tableIndex, inTable := tableRuns[runIndex]
		if !inTable {
			plain = append(plain, run)
			continue
		}
		flushPlain()
		if !emitted[tableIndex] && tableIndex >= 0 && tableIndex < len(tables) {
			values = append(values, tableText(tables[tableIndex]))
			emitted[tableIndex] = true
		}
	}
	flushPlain()
	return strings.Trim(strings.Join(values, "\n"), " \r\n")
}

func tableText(table model.Table) string {
	return model.TableTSV(table)
}

func unionPDFRect(a, b model.Rect) model.Rect {
	if a.Width == 0 && a.Height == 0 {
		return b
	}
	x := math.Min(a.X, b.X)
	y := math.Min(a.Y, b.Y)
	right := math.Max(a.X+a.Width, b.X+b.Width)
	top := math.Max(a.Y+a.Height, b.Y+b.Height)
	return model.Rect{X: x, Y: y, Width: right - x, Height: top - y}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func sortObjects(a []object) {
	for i := range a {
		for j := i + 1; j < len(a); j++ {
			if a[j].num < a[i].num {
				a[i], a[j] = a[j], a[i]
			}
		}
	}
}
func nums(s string) []float64 {
	var r []float64
	for _, x := range strings.Fields(s) {
		v, _ := strconv.ParseFloat(x, 64)
		r = append(r, v)
	}
	return r
}
func dictNum(d []byte, key string) float64 {
	m := regexp.MustCompile(`/` + key + `\s+(\d+(?:\.\d+)?)`).FindSubmatch(d)
	if len(m) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(string(m[1]), 64)
	return v
}
func imageFormat(d []byte) string {
	m := regexp.MustCompile(`/Filter\s*/(\w+)`).FindSubmatch(d)
	if len(m) > 1 {
		return string(m[1])
	}
	return "raw"
}
func decodeStream(dict, data []byte) []byte {
	if bytes.Contains(dict, []byte("/FlateDecode")) {
		r, e := zlib.NewReader(bytes.NewReader(data))
		if e == nil {
			var b bytes.Buffer
			b.ReadFrom(io.LimitReader(r, maxDecodedStream))
			r.Close()
			return b.Bytes()
		}
	}
	return data
}

type fontInfo struct {
	name          string
	encoding      string
	cidCollection string
	cmap          map[string]string
	width         int
}

func pageFonts(dict []byte, objs map[int]object) map[string]fontInfo {
	res := map[string]fontInfo{}
	re := regexp.MustCompile(`/([^\s/<>()\[\]]+)\s+(\d+)\s+\d+\s+R`)
	fontObject := regexp.MustCompile(`/(?:Type\s*/Font|Subtype\s*/Type)`)
	addFont := func(name []byte, id int) {
		o, ok := objs[id]
		if !ok || !fontObject.Match(o.dict) {
			return
		}
		fi := fontInfo{}
		if n := regexp.MustCompile(`/BaseFont\s*/([^\s/<>()\[\]]+)`).FindSubmatch(o.dict); len(n) > 1 {
			fi.name = string(n[1])
		}
		if e := regexp.MustCompile(`/Encoding\s*/([^\s/<>()\[\]]+)`).FindSubmatch(o.dict); len(e) > 1 {
			fi.encoding = string(e[1])
		}
		fi.cidCollection = fontCIDCollection(o, objs)
		if u := regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`).FindSubmatch(o.dict); len(u) > 1 {
			cid, _ := strconv.Atoi(string(u[1]))
			if co, ok := objs[cid]; ok {
				fi.cmap, fi.width = parseCMap(decodeStream(co.dict, co.data))
			}
		}
		if len(fi.cmap) == 0 && fi.encoding == "GBK-EUC-H" && fi.cidCollection == "Adobe-GB1" {
			fi.cmap, fi.width = adobeGBKEUCCMap, adobeGBKEUCWidth
		}
		res[string(name)] = fi
	}
	for _, m := range re.FindAllSubmatch(dict, -1) {
		id, _ := strconv.Atoi(string(m[2]))
		addFont(m[1], id)
	}
	// Resource dictionaries may keep /Font itself in an indirect object:
	// /Resources << /Font 12 0 R >>, with /F1, /F2 ... inside object 12.
	// Resolve that extra level so the page's font aliases retain ToUnicode.
	fontDictRef := regexp.MustCompile(`/Font\s+(\d+)\s+\d+\s+R`)
	for _, m := range fontDictRef.FindAllSubmatch(dict, -1) {
		id, _ := strconv.Atoi(string(m[1]))
		fontDict, ok := objs[id]
		if !ok {
			continue
		}
		for _, entry := range re.FindAllSubmatch(fontDict.dict, -1) {
			fontID, _ := strconv.Atoi(string(entry[2]))
			addFont(entry[1], fontID)
		}
	}
	return res
}

func fontCIDCollection(font object, objs map[int]object) string {
	descendant := regexp.MustCompile(`/DescendantFonts\s*(?:\[\s*)?(\d+)\s+\d+\s+R`).FindSubmatch(font.dict)
	if len(descendant) < 2 {
		return ""
	}
	id, _ := strconv.Atoi(string(descendant[1]))
	queue := []int{id}
	seen := map[int]bool{}
	refRe := regexp.MustCompile(`(\d+)\s+\d+\s+R`)
	for len(queue) > 0 && len(seen) < 32 {
		id = queue[0]
		queue = queue[1:]
		if seen[id] {
			continue
		}
		seen[id] = true
		o, ok := objs[id]
		if !ok {
			continue
		}
		if orderingBytes := dictionaryString(o.dict, "Ordering"); len(orderingBytes) > 0 {
			ordering := string(orderingBytes)
			registry := string(dictionaryString(o.dict, "Registry"))
			if registry != "" {
				return registry + "-" + ordering
			}
			return ordering
		}
		for _, ref := range refRe.FindAllSubmatch(o.dict, -1) {
			next, _ := strconv.Atoi(string(ref[1]))
			if !seen[next] {
				queue = append(queue, next)
			}
		}
	}
	return ""
}

func parseCMap(data []byte) (map[string]string, int) {
	out := map[string]string{}
	width := 0
	pair := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
	triple := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
	source := string(data)
	charSections := regexp.MustCompile(`(?s)beginbfchar(.*?)endbfchar`).FindAllStringSubmatch(source, -1)
	for _, section := range charSections {
		for _, m := range pair.FindAllStringSubmatch(section[1], -1) {
			src, _ := hex.DecodeString(m[1])
			dst, _ := hex.DecodeString(m[2])
			out[string(src)] = utf16BE(dst)
			if len(src) > width {
				width = len(src)
			}
		}
	}
	rangeSections := regexp.MustCompile(`(?s)beginbfrange(.*?)endbfrange`).FindAllStringSubmatch(source, -1)
	arrayRange := regexp.MustCompile(`(?s)<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>\s*\[(.*?)\]`)
	hexValue := regexp.MustCompile(`<([0-9A-Fa-f]+)>`)
	for _, section := range rangeSections {
		body := section[1]
		for _, m := range arrayRange.FindAllStringSubmatch(body, -1) {
			a, _ := strconv.ParseUint(m[1], 16, 32)
			b, _ := strconv.ParseUint(m[2], 16, 32)
			values := hexValue.FindAllStringSubmatch(m[3], -1)
			w := len(m[1]) / 2
			if w > width {
				width = w
			}
			for code := a; code <= b && int(code-a) < len(values); code++ {
				src := make([]byte, w)
				value := code
				for i := w - 1; i >= 0; i-- {
					src[i] = byte(value)
					value >>= 8
				}
				dst, _ := hex.DecodeString(values[code-a][1])
				out[string(src)] = utf16BE(dst)
			}
		}
		body = arrayRange.ReplaceAllString(body, "")
		for _, m := range triple.FindAllStringSubmatch(body, -1) {
			a, _ := strconv.ParseUint(m[1], 16, 32)
			b, _ := strconv.ParseUint(m[2], 16, 32)
			base, _ := strconv.ParseUint(m[3], 16, 32)
			w := len(m[1]) / 2
			if w > width {
				width = w
			}
			for code := a; code <= b && code-a < 65536; code++ {
				src := make([]byte, w)
				value := code
				for i := w - 1; i >= 0; i-- {
					src[i] = byte(value)
					value >>= 8
				}
				dst := fmt.Sprintf("%0*X", len(m[3]), base+(code-a))
				db, _ := hex.DecodeString(dst)
				out[string(src)] = utf16BE(db)
			}
		}
	}
	return out, width
}
func utf16BE(b []byte) string {
	if len(b)%2 != 0 {
		return string(b)
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
	}
	return string(utf16.Decode(u))
}

func parseText(data []byte, fonts map[string]fontInfo) ([]model.TextRun, []*model.Rect) {
	s := string(data)
	runs := []model.TextRun{}
	var runClips []*model.Rect
	size := 12.0
	scale := 1.0
	font := fontInfo{}
	color := "#000000"
	x, y := 0.0, 0.0
	inText := false
	var clip, pending *model.Rect
	var clips []*model.Rect
	var cidFallback *fontInfo
	for _, candidate := range fonts {
		if candidate.cidCollection == "Adobe-CNS1" {
			copy := candidate
			cidFallback = &copy
			break
		}
	}
	decodeText := func(token string) string {
		value := decodePDFString(token, font)
		if cidFallback == nil || !hasBinaryControls(value) {
			return value
		}
		raw := pdfStringBytes(token)
		if len(raw) == 0 || len(raw)%2 != 0 {
			return value
		}
		candidate := decodeAdobeCNS1(raw)
		if candidate != "" && !hasBinaryControls(candidate) {
			return candidate
		}
		return value
	}
	// Tokenizer handles literal strings, hex strings, names, numbers and operators.
	toks := tokenize(s)
	for i := 0; i < len(toks); i++ {
		t := toks[i]
		if t == "q" {
			clips = append(clips, cloneRect(clip))
			continue
		}
		if t == "Q" {
			if len(clips) > 0 {
				clip = clips[len(clips)-1]
				clips = clips[:len(clips)-1]
			}
			continue
		}
		if t == "re" && i >= 4 {
			a, _ := strconv.ParseFloat(toks[i-4], 64)
			b, _ := strconv.ParseFloat(toks[i-3], 64)
			c, _ := strconv.ParseFloat(toks[i-2], 64)
			d, _ := strconv.ParseFloat(toks[i-1], 64)
			pending = &model.Rect{X: a, Y: b, Width: c, Height: d}
			continue
		}
		if (t == "W" || t == "W*") && pending != nil {
			r := intersectRect(clip, pending)
			clip = &r
			pending = nil
			continue
		}
		if t == "BT" {
			inText = true
			x = 0
			y = 0
			scale = 1
			continue
		}
		if t == "ET" {
			inText = false
			continue
		}
		if (t == "rg" || t == "RG") && i >= 3 {
			r, _ := strconv.ParseFloat(toks[i-3], 64)
			g, _ := strconv.ParseFloat(toks[i-2], 64)
			b, _ := strconv.ParseFloat(toks[i-1], 64)
			color = fmt.Sprintf("#%02X%02X%02X", int(r*255), int(g*255), int(b*255))
		}
		// Some real-world generators emit Tf immediately before BT. Although
		// the PDF grammar normally places text-state operators inside a text
		// object, readers preserve this state and use it for the following BT.
		if t == "Tf" && i >= 2 {
			size, _ = strconv.ParseFloat(toks[i-1], 64)
			font = fonts[strings.TrimPrefix(toks[i-2], "/")]
			continue
		}
		if !inText {
			continue
		}
		switch t {
		case "Tm":
			if i >= 6 {
				a, _ := strconv.ParseFloat(toks[i-6], 64)
				d, _ := strconv.ParseFloat(toks[i-3], 64)
				scale = math.Max(math.Abs(a), math.Abs(d))
				if scale == 0 {
					scale = 1
				}
				x, _ = strconv.ParseFloat(toks[i-2], 64)
				y, _ = strconv.ParseFloat(toks[i-1], 64)
			}
		case "Td", "TD":
			if i >= 2 {
				dx, _ := strconv.ParseFloat(toks[i-2], 64)
				dy, _ := strconv.ParseFloat(toks[i-1], 64)
				x += dx * scale
				y += dy * scale
			}
		case "Tj":
			if i >= 1 {
				txt := decodeText(toks[i-1])
				if txt != "" {
					eff := size * scale
					w := float64(len([]rune(txt))) * eff * .5
					bounds := model.Rect{X: x, Y: y - eff, Width: w, Height: eff}
					if visible(bounds, clip) {
						runs = append(runs, model.TextRun{Text: txt, Font: font.name, Size: eff, Color: color, Bounds: bounds})
						runClips = append(runClips, cloneRect(clip))
					}
					x += w
				}
			}
		case "TJ":
			if i >= 1 {
				for _, raw := range parseArrayItems(toks[i-1]) {
					if adjustment, err := strconv.ParseFloat(raw, 64); err == nil {
						x -= adjustment / 1000 * size * scale
						continue
					}
					v := decodeText(raw)
					eff := size * scale
					w := float64(len([]rune(v))) * eff * .5
					bounds := model.Rect{X: x, Y: y - eff, Width: w, Height: eff}
					if visible(bounds, clip) {
						runs = append(runs, model.TextRun{Text: v, Font: font.name, Size: eff, Color: color, Bounds: bounds})
						runClips = append(runClips, cloneRect(clip))
					}
					x += w
				}
			}
		}
	}
	return runs, runClips
}
func cloneRect(r *model.Rect) *model.Rect {
	if r == nil {
		return nil
	}
	c := *r
	return &c
}
func intersectRect(a, b *model.Rect) model.Rect {
	if a == nil {
		return *b
	}
	x := math.Max(a.X, b.X)
	y := math.Max(a.Y, b.Y)
	r := math.Min(a.X+a.Width, b.X+b.Width)
	top := math.Min(a.Y+a.Height, b.Y+b.Height)
	return model.Rect{X: x, Y: y, Width: math.Max(0, r-x), Height: math.Max(0, top-y)}
}
func visible(r model.Rect, clip *model.Rect) bool {
	if clip == nil || (clip.Width >= 1 && clip.Height >= 1) {
		return true
	}
	return r.X+r.Width >= clip.X && r.X <= clip.X+clip.Width && r.Y+r.Height >= clip.Y && r.Y <= clip.Y+clip.Height
}
func tokenize(s string) []string {
	var out []string
	for i := 0; i < len(s); {
		if strings.ContainsRune(" \t\r\n", rune(s[i])) {
			i++
			continue
		}
		if s[i] == '(' {
			j := i + 1
			dep := 1
			for j < len(s) && dep > 0 {
				if s[j] == '\\' {
					j += 2
					continue
				}
				if s[j] == '(' {
					dep++
				}
				if s[j] == ')' {
					dep--
				}
				j++
			}
			out = append(out, s[i:j])
			i = j
			continue
		}
		if s[i] == '[' {
			j := i + 1
			for j < len(s) && s[j] != ']' {
				j++
			}
			if j < len(s) {
				j++
			}
			out = append(out, s[i:j])
			i = j
			continue
		}
		if s[i] == '<' && i+1 < len(s) && s[i+1] != '<' {
			j := strings.IndexByte(s[i+1:], '>')
			if j >= 0 {
				j += i + 2
				out = append(out, s[i:j])
				i = j
				continue
			}
		}
		if i+1 < len(s) && ((s[i] == '<' && s[i+1] == '<') || (s[i] == '>' && s[i+1] == '>')) {
			out = append(out, s[i:i+2])
			i += 2
			continue
		}
		// Standalone delimiters can occur in malformed or operator-heavy
		// content streams. Treat them as one-byte tokens so the scanner always
		// makes progress (otherwise a closing delimiter would loop forever).
		if strings.ContainsRune(")]><", rune(s[i])) {
			out = append(out, s[i:i+1])
			i++
			continue
		}
		j := i
		for j < len(s) && !strings.ContainsRune(" \t\r\n[]()<>", rune(s[j])) {
			j++
		}
		out = append(out, s[i:j])
		i = j
	}
	return out
}
func decodePDFString(t string, font fontInfo) string {
	raw := pdfStringBytes(t)
	if len(raw) == 0 {
		return ""
	}
	if len(font.cmap) > 0 {
		w := font.width
		if w <= 0 {
			w = 2
		}
		var b strings.Builder
		for i := 0; i < len(raw); {
			matched := false
			for n := w; n >= 1; n-- {
				if i+n <= len(raw) {
					if v, ok := font.cmap[string(raw[i:i+n])]; ok {
						b.WriteString(v)
						i += n
						matched = true
						break
					}
				}
			}
			if !matched {
				// A ToUnicode map is allowed to omit codes. Simple fonts can still
				// recover those bytes through their declared base encoding.
				if isSingleByteFontEncoding(font.encoding) {
					b.WriteRune(decodeSimpleFontByte(raw[i], font.encoding))
				}
				i++
			}
		}
		return b.String()
	}
	if font.encoding == "Identity-H" && font.cidCollection == "Adobe-CNS1" {
		return decodeAdobeCNS1(raw)
	}
	if len(raw) >= 2 && raw[0] == 0xfe && raw[1] == 0xff {
		return utf16BE(raw[2:])
	}
	var b strings.Builder
	for _, c := range raw {
		b.WriteRune(decodeSimpleFontByte(c, font.encoding))
	}
	return b.String()
}

func pdfStringBytes(t string) []byte {
	var raw []byte
	if len(t) >= 2 && t[0] == '(' {
		raw = decodePDFLiteral([]byte(t[1 : len(t)-1]))
	}
	if strings.HasPrefix(t, "<") {
		h := strings.Trim(t, "<>")
		raw, _ = hex.DecodeString(h)
	}
	return raw
}

func hasBinaryControls(text string) bool {
	for _, r := range text {
		if r == 0 || (r < 0x20 && r != '\t' && r != '\n' && r != '\r') || (r >= 0x7f && r <= 0x9f) {
			return true
		}
	}
	return false
}

func decodePDFLiteral(src []byte) []byte {
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		if src[i] != '\\' {
			out = append(out, src[i])
			continue
		}
		if i+1 >= len(src) {
			break
		}
		i++
		switch src[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case '\n':
			// A backslash followed by an end-of-line is a continuation.
		case '\r':
			if i+1 < len(src) && src[i+1] == '\n' {
				i++
			}
		default:
			if src[i] >= '0' && src[i] <= '7' {
				value := int(src[i] - '0')
				for count := 1; count < 3 && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '7'; count++ {
					i++
					value = value*8 + int(src[i]-'0')
				}
				out = append(out, byte(value))
			} else {
				// Escaped parentheses and backslashes represent themselves.
				out = append(out, src[i])
			}
		}
	}
	return out
}

func decodeSimpleFontByte(c byte, encoding string) rune {
	if encoding == "WinAnsiEncoding" {
		if r, ok := winAnsiRunes[c]; ok {
			return r
		}
	}
	return rune(c)
}

func isSingleByteFontEncoding(encoding string) bool {
	switch encoding {
	case "WinAnsiEncoding", "MacRomanEncoding", "MacExpertEncoding", "StandardEncoding":
		return true
	default:
		return false
	}
}

var winAnsiRunes = map[byte]rune{
	0x80: '\u20ac', 0x81: '\u2022', 0x82: '\u201a', 0x83: '\u0192',
	0x84: '\u201e', 0x85: '\u2026', 0x86: '\u2020', 0x87: '\u2021',
	0x88: '\u02c6', 0x89: '\u2030', 0x8a: '\u0160', 0x8b: '\u2039',
	0x8c: '\u0152', 0x8d: '\u2022', 0x8e: '\u017d', 0x8f: '\u2022',
	0x90: '\u2022', 0x91: '\u2018', 0x92: '\u2019', 0x93: '\u201c',
	0x94: '\u201d', 0x95: '\u2022', 0x96: '\u2013', 0x97: '\u2014',
	0x98: '\u02dc', 0x99: '\u2122', 0x9a: '\u0161', 0x9b: '\u203a',
	0x9c: '\u0153', 0x9d: '\u2022', 0x9e: '\u017e', 0x9f: '\u0178',
}

func parseArrayItems(t string) []string {
	if len(t) >= 2 && t[0] == '[' && t[len(t)-1] == ']' {
		t = t[1 : len(t)-1]
	}
	return tokenize(t)
}
