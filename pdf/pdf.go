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
	"strconv"
	"strings"
	"unicode/utf16"

	"github.com/hekediguo2002/FileContentExtractor/model"
)

type object struct {
	num        int
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
	if regexp.MustCompile(`/Encrypt\s+\d+\s+\d+\s+R`).Match(b) {
		return nil, fmt.Errorf("encrypted PDF is not supported")
	}
	objs := map[int]object{}
	ms := objRe.FindAllSubmatch(b, -1)
	for _, m := range ms {
		n, _ := strconv.Atoi(string(m[1]))
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
		objs[n] = object{n, dict, data}
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
		pg.Runs = parseText(content, fonts)
		pg.Text = pageText(pg.Runs)
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
		for _, id := range resourceRefs(dict, "XObject") {
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
	channels := 0
	if regexp.MustCompile(`/ColorSpace\s*/DeviceRGB\b`).Match(o.dict) {
		channels = 3
	} else if regexp.MustCompile(`/ColorSpace\s*/DeviceGray\b`).Match(o.dict) {
		channels = 1
	} else if regexp.MustCompile(`/ColorSpace\s*/DeviceCMYK\b`).Match(o.dict) {
		channels = 4
	}
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
func resourceRefs(d []byte, kind string) []int {
	m := regexp.MustCompile(`(?s)/` + kind + `\s*<<(.+?)>>`).FindSubmatch(d)
	if len(m) < 2 {
		return nil
	}
	var out []int
	for _, r := range regexp.MustCompile(`/[^\s/<>()\[\]]+\s+(\d+)\s+\d+\s+R`).FindAllSubmatch(m[1], -1) {
		n, _ := strconv.Atoi(string(r[1]))
		out = append(out, n)
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
			} else if r.Bounds.X-lastEnd > math.Max(lastSize, r.Size)*.7 {
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
	name  string
	cmap  map[string]string
	width int
}

func pageFonts(dict []byte, objs map[int]object) map[string]fontInfo {
	res := map[string]fontInfo{}
	re := regexp.MustCompile(`/([^\s/<>()\[\]]+)\s+(\d+)\s+\d+\s+R`)
	fontObject := regexp.MustCompile(`/(?:Type\s*/Font|Subtype\s*/Type)`)
	for _, m := range re.FindAllSubmatch(dict, -1) {
		id, _ := strconv.Atoi(string(m[2]))
		o, ok := objs[id]
		if !ok || !fontObject.Match(o.dict) {
			continue
		}
		fi := fontInfo{}
		if n := regexp.MustCompile(`/BaseFont\s*/([^\s/<>()\[\]]+)`).FindSubmatch(o.dict); len(n) > 1 {
			fi.name = string(n[1])
		}
		if u := regexp.MustCompile(`/ToUnicode\s+(\d+)\s+\d+\s+R`).FindSubmatch(o.dict); len(u) > 1 {
			cid, _ := strconv.Atoi(string(u[1]))
			if co, ok := objs[cid]; ok {
				fi.cmap, fi.width = parseCMap(decodeStream(co.dict, co.data))
			}
		}
		res[string(m[1])] = fi
	}
	return res
}
func parseCMap(data []byte) (map[string]string, int) {
	out := map[string]string{}
	mode := ""
	width := 0
	pair := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
	triple := regexp.MustCompile(`<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>\s*<([0-9A-Fa-f]+)>`)
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "beginbfchar") {
			mode = "char"
			continue
		}
		if strings.Contains(line, "endbfchar") {
			mode = ""
			continue
		}
		if strings.Contains(line, "beginbfrange") {
			mode = "range"
			continue
		}
		if strings.Contains(line, "endbfrange") {
			mode = ""
			continue
		}
		if mode == "char" {
			for _, m := range pair.FindAllStringSubmatch(line, -1) {
				src, _ := hex.DecodeString(m[1])
				dst, _ := hex.DecodeString(m[2])
				out[string(src)] = utf16BE(dst)
				if width == 0 {
					width = len(src)
				}
			}
		}
		if mode == "range" {
			if m := triple.FindStringSubmatch(line); len(m) > 0 {
				a, _ := strconv.ParseUint(m[1], 16, 32)
				b, _ := strconv.ParseUint(m[2], 16, 32)
				base, _ := strconv.ParseUint(m[3], 16, 32)
				w := len(m[1]) / 2
				if width == 0 {
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

func parseText(data []byte, fonts map[string]fontInfo) []model.TextRun {
	s := string(data)
	runs := []model.TextRun{}
	size := 12.0
	scale := 1.0
	font := fontInfo{}
	color := "#000000"
	x, y := 0.0, 0.0
	inText := false
	var clip, pending *model.Rect
	var clips []*model.Rect
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
				txt := decodePDFString(toks[i-1], font)
				if txt != "" {
					eff := size * scale
					w := float64(len([]rune(txt))) * eff * .5
					bounds := model.Rect{X: x, Y: y - eff, Width: w, Height: eff}
					if visible(bounds, clip) {
						runs = append(runs, model.TextRun{Text: txt, Font: font.name, Size: eff, Color: color, Bounds: bounds})
					}
					x += w
				}
			}
		case "TJ":
			if i >= 1 {
				for _, raw := range parseArrayStrings(toks[i-1]) {
					v := decodePDFString(raw, font)
					eff := size * scale
					w := float64(len([]rune(v))) * eff * .5
					bounds := model.Rect{X: x, Y: y - eff, Width: w, Height: eff}
					if visible(bounds, clip) {
						runs = append(runs, model.TextRun{Text: v, Font: font.name, Size: eff, Color: color, Bounds: bounds})
					}
					x += w
				}
			}
		}
	}
	return runs
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
	var raw []byte
	if len(t) >= 2 && t[0] == '(' {
		t = t[1 : len(t)-1]
		var b strings.Builder
		for i := 0; i < len(t); i++ {
			if t[i] == '\\' && i+1 < len(t) {
				i++
				switch t[i] {
				case 'n':
					b.WriteByte('\n')
				case 'r':
					b.WriteByte('\r')
				case 't':
					b.WriteByte('\t')
				default:
					b.WriteByte(t[i])
				}
			} else {
				b.WriteByte(t[i])
			}
		}
		raw = []byte(b.String())
	}
	if strings.HasPrefix(t, "<") {
		h := strings.Trim(t, "<>")
		raw, _ = hex.DecodeString(h)
	}
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
				i++
			}
		}
		return b.String()
	}
	if len(raw) > 1 && len(raw)%2 == 0 {
		return utf16BE(raw)
	}
	return string(raw)
}
func parseArrayStrings(t string) []string {
	var out []string
	for i := 0; i < len(t); {
		if t[i] == '(' {
			j := i + 1
			for j < len(t) && t[j] != ')' {
				if t[j] == '\\' {
					j++
				}
				j++
			}
			if j < len(t) {
				j++
			}
			out = append(out, t[i:j])
			i = j
		} else if t[i] == '<' {
			j := i + 1
			for j < len(t) && t[j] != '>' {
				j++
			}
			if j < len(t) {
				j++
			}
			out = append(out, t[i:j])
			i = j
		} else {
			i++
		}
	}
	return out
}
