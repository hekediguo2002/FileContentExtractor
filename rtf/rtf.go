// Package rtf 解析 RTF 文档的文本与常见格式（字体、字号、颜色），
// 仅依赖 Go 标准库。参考 SensitiveFile/file_detector 的 rtf_reader.py 移植。
package rtf

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/hekediguo2002/FileContentExtractor/layout"
	"github.com/hekediguo2002/FileContentExtractor/model"
)

var tokenRe = regexp.MustCompile(`\\([A-Za-z]+)(-?\d+)? ?|\\'([0-9A-Fa-f]{2})|\\([^A-Za-z])|([{}])|([^\\{}]+)`)

// destinations 为可忽略的目标组（字体表、颜色表、样式表、域、对象等）。
var destinations = map[string]bool{
	"aftncn": true, "aftnsep": true, "aftnsepc": true, "annotation": true,
	"atnauthor": true, "atndate": true, "atnicn": true, "atnid": true,
	"atnparent": true, "atnref": true, "atntime": true, "atrfend": true,
	"atrfstart": true, "author": true, "background": true, "bkmkend": true,
	"bkmkstart": true, "blipuid": true, "buptim": true, "category": true,
	"colorschememapping": true, "colortbl": true, "comment": true,
	"company": true, "creatim": true, "datafield": true, "datastore": true,
	"defchp": true, "defpap": true, "do": true, "doccomm": true, "docvar": true,
	"dptxbxtext": true, "ebcend": true, "ebcstart": true, "factoidname": true,
	"falt": true, "fchars": true, "ffdeftext": true, "ffentrymcr": true,
	"ffexitmcr": true, "ffformat": true, "ffhelptext": true, "ffl": true,
	"ffname": true, "ffstattext": true, "field": true, "file": true,
	"filetbl": true, "fldinst": true, "fldrslt": true, "fontemb": true,
	"fontfile": true, "fonttbl": true, "footer": true, "footerf": true,
	"footerl": true, "footerr": true, "footnote": true, "formfield": true,
	"ftncn": true, "ftnsep": true, "ftnsepc": true, "generator": true,
	"header": true, "headerf": true, "headerl": true, "headerr": true,
	"hl": true, "hlfr": true, "hlinkbase": true, "hlloc": true, "hlsrc": true,
	"hsv": true, "htmltag": true, "info": true, "keycode": true,
	"keywords": true, "latentstyles": true, "lchars": true, "levelnumbers": true,
	"leveltext": true, "lfolevel": true, "linkval": true, "list": true,
	"listlevel": true, "listname": true, "listoverride": true,
	"listoverridetable": true, "listpicture": true, "liststylename": true,
	"listtable": true, "manager": true, "mhtmltag": true, "mmath": true,
	"nesttableprops": true, "nextfile": true, "nonesttables": true,
	"objalias": true, "objclass": true, "objdata": true, "object": true,
	"objname": true, "objsect": true, "objtime": true, "oldcprops": true,
	"oldpprops": true, "oldsprops": true, "oldtprops": true, "operator": true,
	"panose": true, "password": true, "passwordhash": true, "pgp": true,
	"pgptbl": true, "picprop": true, "pict": true, "pn": true, "pnseclvl": true,
	"pntext": true, "printim": true, "private": true, "propname": true,
	"protend": true, "protstart": true, "protusertbl": true, "pxe": true,
	"result": true, "revtbl": true, "revtim": true, "rsidtbl": true,
	"rxe": true, "shp": true, "shpgrp": true, "shpinst": true, "shppict": true,
	"shprslt": true, "shptxt": true, "sn": true, "sp": true, "staticval": true,
	"stylesheet": true, "subject": true, "sv": true, "svb": true, "tc": true,
	"template": true, "themedata": true, "title": true, "txe": true,
	"ud": true, "upr": true, "userprops": true, "wgrffmtfilter": true,
	"windowcaption": true, "writereservation": true, "writereservhash": true,
	"xe": true, "xmlattrname": true, "xmlattrvalue": true, "xmlclose": true,
	"xmlname": true, "xmlnstbl": true, "xmlopen": true,
}

type state struct {
	ignorable bool
	font      int
	fontSize  float64
	color     int
	alignment string
	ucSkip    int
}

func defaultState() state { return state{fontSize: 12, ucSkip: 1} }

// ParseFile 解析 RTF 文件。中文内容依赖 ＼uN Unicode 转义（Word 默认行为）；
// \'xx 字节按 CP1252 解码，GBK 双字节序列不在支持范围内。
func ParseFile(name string) (*model.Document, error) {
	raw, err := os.ReadFile(name)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(strings.TrimLeft(string(raw[:minInt(len(raw), 16)]), " \t\r\n"), `{\rtf`) {
		return nil, fmt.Errorf("not an RTF file: %s", name)
	}
	source := string(raw) // latin-1 语义：字节即码点
	fonts := fontTable(source)
	colors := colorTable(source)
	paragraphs, width, height := parse(source, fonts, colors)

	d := &model.Document{Path: name, Format: "rtf", Pagination: "layout-estimated"}
	d.Pages = layout.Paginate(paragraphs, layout.Options{Width: width, Height: height})
	return d, nil
}

// extractGroup 提取 {\control ...} 整组（含嵌套）。
func extractGroup(source, control string) string {
	start := strings.Index(source, `{\`+control)
	if start < 0 {
		return ""
	}
	depth, escaped := 0, false
	for i := start; i < len(source); i++ {
		c := source[i]
		if escaped {
			escaped = false
			continue
		}
		switch c {
		case '\\':
			escaped = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return source[start : i+1]
			}
		}
	}
	return ""
}

var fontEntryRe = regexp.MustCompile(`(?s)\{\\f(\d+)(.*?);\}`)
var controlWordRe = regexp.MustCompile(`\\[A-Za-z]+-?\d* ?`)

func fontTable(source string) map[int]string {
	fonts := map[int]string{}
	for _, m := range fontEntryRe.FindAllStringSubmatch(extractGroup(source, "fonttbl"), -1) {
		n, _ := strconv.Atoi(m[1])
		body := controlWordRe.ReplaceAllString(m[2], "")
		name := strings.TrimSpace(strings.NewReplacer("{", "", "}", "").Replace(body))
		if name != "" {
			fonts[n] = name
		}
	}
	return fonts
}

var colorChannelRe = regexp.MustCompile(`\\(red|green|blue)(\d+)`)

func colorTable(source string) []string {
	group := extractGroup(source, "colortbl")
	colors := []string{""}
	if group == "" {
		return colors
	}
	body := strings.TrimRight(group[len(`{\colortbl`):], "}")
	entries := strings.Split(body, ";")
	var out []string
	for _, entry := range entries[:maxInt(0, len(entries)-1)] {
		channels := map[string]int{}
		for _, m := range colorChannelRe.FindAllStringSubmatch(entry, -1) {
			v, _ := strconv.Atoi(m[2])
			channels[m[1]] = minInt(255, v)
		}
		r, okR := channels["red"]
		g, okG := channels["green"]
		b, okB := channels["blue"]
		if okR && okG && okB {
			out = append(out, fmt.Sprintf("#%02X%02X%02X", r, g, b))
		} else {
			out = append(out, "")
		}
	}
	if len(out) == 0 {
		return colors
	}
	return out
}

func parse(source string, fonts map[int]string, colors []string) ([]layout.Paragraph, float64, float64) {
	st := defaultState()
	stack := []state{}
	var spans []model.TextRun
	var paragraphs []layout.Paragraph
	unicodeFallback := 0
	pendingIgnorable := false
	width, height := 612.0, 792.0
	var ansiBytes []byte

	appendText := func(text string) {}
	flushAnsi := func() {}
	appendText = func(text string) {
		if st.ignorable || text == "" {
			return
		}
		color := "#000000"
		if st.color >= 0 && st.color < len(colors) && colors[st.color] != "" {
			color = colors[st.color]
		}
		run := model.TextRun{Text: text, Font: fonts[st.font], Size: st.fontSize, Color: color}
		if n := len(spans); n > 0 {
			last := &spans[n-1]
			if last.Font == run.Font && last.Size == run.Size && last.Color == run.Color {
				last.Text += text
				return
			}
		}
		spans = append(spans, run)
	}
	flushAnsi = func() {
		if len(ansiBytes) == 0 {
			return
		}
		text := decodeCP1252(ansiBytes)
		ansiBytes = ansiBytes[:0]
		appendText(text)
	}
	flushParagraph := func() {
		if len(spans) > 0 {
			hasText := false
			for _, s := range spans {
				if strings.TrimSpace(s.Text) != "" {
					hasText = true
					break
				}
			}
			if hasText {
				paragraphs = append(paragraphs, layout.Paragraph{Runs: spans})
			}
			spans = nil
		}
	}
	markPageBreak := func() {
		flushParagraph()
		if len(paragraphs) > 0 {
			paragraphs[len(paragraphs)-1].BreakAfter = true
		}
	}

	for _, m := range tokenRe.FindAllStringSubmatch(source, -1) {
		word, param, hexByte, symbol, brace, plain := m[1], m[2], m[3], m[4], m[5], m[6]
		if brace == "{" {
			flushAnsi()
			stack = append(stack, st)
			continue
		}
		if brace == "}" {
			flushAnsi()
			if len(stack) > 0 {
				st = stack[len(stack)-1]
				stack = stack[:len(stack)-1]
			}
			pendingIgnorable = false
			continue
		}
		if symbol != "" {
			flushAnsi()
			switch symbol {
			case "*":
				pendingIgnorable = true
			case "{", "}", "\\":
				appendText(symbol)
			case "~":
				appendText(" ")
			case "_":
				appendText("‑")
			case "-":
				appendText("­")
			}
			continue
		}
		if hexByte != "" {
			if unicodeFallback > 0 {
				unicodeFallback--
				continue
			}
			v, _ := strconv.ParseUint(hexByte, 16, 8)
			ansiBytes = append(ansiBytes, byte(v))
			continue
		}
		if word != "" {
			flushAnsi()
			value, hasValue := 0, false
			if param != "" {
				value, _ = strconv.Atoi(param)
				hasValue = true
			}
			if pendingIgnorable || destinations[word] {
				st.ignorable = true
				pendingIgnorable = false
				continue
			}
			if st.ignorable {
				continue
			}
			switch word {
			case "uc":
				if hasValue && value >= 0 {
					st.ucSkip = value
				}
			case "u":
				if hasValue {
					if value < 0 {
						value += 65536
					}
					appendText(string(rune(value)))
					unicodeFallback = st.ucSkip
				}
			case "par", "row", "cell":
				flushParagraph()
			case "line":
				appendText("\n")
			case "tab":
				appendText("\t")
			case "page":
				markPageBreak()
			case "f":
				if hasValue {
					st.font = value
				}
			case "fs":
				if hasValue {
					st.fontSize = math2(float64(value) / 2)
				}
			case "cf":
				if hasValue {
					st.color = value
				}
			case "qc":
				st.alignment = "center"
			case "qr":
				st.alignment = "right"
			case "ql", "qj":
				st.alignment = "left"
			case "pard":
				st.font, st.fontSize, st.color = 0, 12, 0
				st.alignment = ""
			case "plain":
				st.font, st.fontSize, st.color = 0, 12, 0
			case "paperw":
				if hasValue {
					width = float64(value) / 20
				}
			case "paperh":
				if hasValue {
					height = float64(value) / 20
				}
			}
			continue
		}
		if plain != "" && !st.ignorable {
			text := strings.NewReplacer("\r", "", "\n", "").Replace(plain)
			if unicodeFallback > 0 {
				skipped := minInt(unicodeFallback, len(text))
				text = text[skipped:]
				unicodeFallback -= skipped
			}
			appendText(text)
		}
	}
	flushAnsi()
	flushParagraph()
	return paragraphs, width, height
}

func math2(v float64) float64 {
	if v < 0 {
		return 0
	}
	return v
}

// decodeCP1252 把 \'xx 累积的字节按 CP1252 解码（高位字节映射到对应 Unicode）。
func decodeCP1252(b []byte) string {
	runes := make([]rune, 0, len(b))
	for _, c := range b {
		if c < 0x80 {
			runes = append(runes, rune(c))
		} else if r, ok := cp1252High[c]; ok {
			runes = append(runes, r)
		} else {
			runes = append(runes, rune(c))
		}
	}
	return string(runes)
}

var cp1252High = map[byte]rune{
	0x80: '€', 0x82: '‚', 0x83: 'ƒ', 0x84: '„', 0x85: '…',
	0x86: '†', 0x87: '‡', 0x88: 'ˆ', 0x89: '‰', 0x8A: 'Š',
	0x8B: '‹', 0x8C: 'Œ', 0x8E: 'Ž', 0x91: '‘', 0x92: '’',
	0x93: '“', 0x94: '”', 0x95: '•', 0x96: '–', 0x97: '—',
	0x98: '˜', 0x99: '™', 0x9A: 'š', 0x9B: '›', 0x9C: 'œ',
	0x9E: 'ž', 0x9F: 'Ÿ',
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
