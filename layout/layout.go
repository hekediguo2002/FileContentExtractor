package layout

import (
	"math"
	"strings"
	"unicode"

	"github.com/hekediguo2002/FileContentExtractor/model"
)

type Paragraph struct {
	Runs           []model.TextRun
	Images         []model.Image
	Before         float64
	After          float64
	LineHeight     float64
	BreakBefore    bool
	BreakAfter     bool
	TableID        int
	TableRow       int
	TableCell      int
	RowSpan        int
	ColSpan        int
	SeparatorAfter string
}

type Options struct {
	Width, Height                                    float64
	MarginTop, MarginRight, MarginBottom, MarginLeft float64
	TargetPages                                      int
}

type line struct {
	runs           []model.TextRun
	images         []model.Image
	height         float64
	breakBefore    bool
	breakAfter     bool
	tableID        int
	tableRow       int
	tableCell      int
	rowSpan        int
	colSpan        int
	separatorAfter string
}

type pageTableState struct {
	index int
	rows  map[int]int
}

func Paginate(paragraphs []Paragraph, options Options) []model.Page {
	options = defaults(options)
	lines := makeLines(paragraphs, options.Width-options.MarginLeft-options.MarginRight)
	if len(lines) == 0 {
		return []model.Page{{Number: 1, Width: options.Width, Height: options.Height}}
	}
	usable := options.Height - options.MarginTop - options.MarginBottom
	total := 0.0
	for _, line := range lines {
		total += line.height
	}
	targetPages := options.TargetPages
	// Some non-Office producers always write Pages=1. Ignore a cached value
	// when the measured content cannot plausibly fit in that page count.
	if targetPages == 1 && total > usable*1.5 {
		targetPages = 0
	}
	if targetPages > 0 {
		candidate := total / float64(options.TargetPages)
		if candidate > 0 {
			usable = candidate
		}
	}
	pages := []model.Page{{Number: 1, Width: options.Width, Height: options.Height}}
	tableStates := map[int]map[int]*pageTableState{}
	y := 0.0
	cumulative := 0.0
	nextTarget := usable
	hardAfter := make([]bool, len(lines))
	remainingHard := 0
	for i := 0; i+1 < len(lines); i++ {
		hardAfter[i] = lines[i].breakAfter || lines[i+1].breakBefore
		if hardAfter[i] {
			remainingHard++
		}
	}
	for index, line := range lines {
		page := &pages[len(pages)-1]
		newPage := false
		incomingContent := lineHasContent(line)
		if targetPages > 0 {
			roomForAutomaticPage := len(pages)+remainingHard < targetPages
			newPage = roomForAutomaticPage && incomingContent && pageHasContent(page) && cumulative+line.height > nextTarget
		} else {
			newPage = (len(page.Runs)+len(page.Images) > 0) && y+line.height > usable
		}
		if newPage {
			pages = append(pages, model.Page{Number: len(pages) + 1, Width: options.Width, Height: options.Height})
			page = &pages[len(pages)-1]
			y = 0
			if targetPages > 0 {
				nextTarget = usable * float64(len(pages))
			}
		}
		lineRuns := make([]model.TextRun, 0, len(line.runs))
		for _, run := range line.runs {
			run.Bounds.Y = options.MarginTop + y
			page.Runs = appendRun(page.Runs, run)
			lineRuns = append(lineRuns, run)
			if run.Text != "" {
				page.Text += run.Text
			}
		}
		for _, im := range line.images {
			im.Bounds.Y = options.MarginTop + y
			page.Images = append(page.Images, im)
		}
		if line.tableID > 0 {
			appendTableLine(page, tableStates, line, lineRuns)
		}
		if line.separatorAfter != "" && !line.breakAfter {
			page.Text += line.separatorAfter
		} else if line.tableID == 0 && len(line.runs) > 0 && index+1 < len(lines) && !line.breakAfter {
			page.Text += "\n"
		}
		y += line.height
		cumulative += line.height
		if hardAfter[index] {
			remainingHard--
			pages = append(pages, model.Page{Number: len(pages) + 1, Width: options.Width, Height: options.Height})
			y = 0
			if targetPages > 0 {
				nextTarget = usable * float64(len(pages))
			}
		}
	}
	for i := range pages {
		pages[i].Text = strings.Trim(pages[i].Text, " \r\n")
	}
	return pages
}

func appendTableLine(page *model.Page, states map[int]map[int]*pageTableState, source line, runs []model.TextRun) {
	pageStates := states[page.Number]
	if pageStates == nil {
		pageStates = map[int]*pageTableState{}
		states[page.Number] = pageStates
	}
	state := pageStates[source.tableID]
	if state == nil {
		page.Tables = append(page.Tables, model.Table{})
		state = &pageTableState{index: len(page.Tables) - 1, rows: map[int]int{}}
		pageStates[source.tableID] = state
	}
	table := &page.Tables[state.index]
	rowIndex, ok := state.rows[source.tableRow]
	if !ok {
		table.Rows = append(table.Rows, model.TableRow{})
		rowIndex = len(table.Rows) - 1
		state.rows[source.tableRow] = rowIndex
	}
	row := &table.Rows[rowIndex]
	cellIndex := -1
	for i := range row.Cells {
		if row.Cells[i].Column == source.tableCell {
			cellIndex = i
			break
		}
	}
	if cellIndex < 0 {
		row.Cells = append(row.Cells, model.TableCell{
			Row: source.tableRow, Column: source.tableCell,
			RowSpan: maxInt(source.rowSpan, 1), ColSpan: maxInt(source.colSpan, 1),
		})
		cellIndex = len(row.Cells) - 1
	}
	cell := &row.Cells[cellIndex]
	if len(runs) > 0 {
		if cell.Text != "" {
			cell.Text += "\n"
		}
		for _, run := range runs {
			cell.Text += run.Text
			cell.Bounds = unionRect(cell.Bounds, run.Bounds)
			table.Bounds = unionRect(table.Bounds, run.Bounds)
		}
		cell.Runs = append(cell.Runs, runs...)
	}
}

func unionRect(a, b model.Rect) model.Rect {
	if a.Width == 0 && a.Height == 0 {
		return b
	}
	if b.Width == 0 && b.Height == 0 {
		return a
	}
	x := math.Min(a.X, b.X)
	y := math.Min(a.Y, b.Y)
	right := math.Max(a.X+a.Width, b.X+b.Width)
	bottom := math.Max(a.Y+a.Height, b.Y+b.Height)
	return model.Rect{X: x, Y: y, Width: right - x, Height: bottom - y}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func lineHasContent(line line) bool {
	if len(line.images) > 0 {
		return true
	}
	for _, run := range line.runs {
		if strings.TrimSpace(run.Text) != "" {
			return true
		}
	}
	return false
}
func pageHasContent(page *model.Page) bool {
	return len(page.Images) > 0 || strings.TrimSpace(page.Text) != ""
}

func defaults(o Options) Options {
	if o.Width <= 0 {
		o.Width = 595.28
	}
	if o.Height <= 0 {
		o.Height = 841.89
	}
	if o.MarginTop < 0 {
		o.MarginTop = 0
	}
	if o.MarginRight < 0 {
		o.MarginRight = 0
	}
	if o.MarginBottom < 0 {
		o.MarginBottom = 0
	}
	if o.MarginLeft < 0 {
		o.MarginLeft = 0
	}
	if o.MarginTop == 0 {
		o.MarginTop = 72
	}
	if o.MarginRight == 0 {
		o.MarginRight = 72
	}
	if o.MarginBottom == 0 {
		o.MarginBottom = 72
	}
	if o.MarginLeft == 0 {
		o.MarginLeft = 72
	}
	return o
}

func makeLines(paragraphs []Paragraph, width float64) []line {
	var lines []line
	for _, p := range paragraphs {
		firstLine := len(lines)
		if p.Before > 0 {
			lines = append(lines, line{height: p.Before})
		}
		newLine := func() line {
			return line{tableID: p.TableID, tableRow: p.TableRow, tableCell: p.TableCell, rowSpan: p.RowSpan, colSpan: p.ColSpan}
		}
		current := newLine()
		x := 0.0
		flush := func(force bool) {
			if len(current.runs) > 0 || force {
				if current.height <= 0 {
					current.height = defaultLineHeight(p)
				}
				lines = append(lines, current)
			}
			current = newLine()
			x = 0
		}
		for _, source := range p.Runs {
			style := source
			if style.Size <= 0 {
				style.Size = 11
			}
			for _, r := range source.Text {
				if r == '\n' || r == '\r' {
					flush(true)
					continue
				}
				w := runeWidth(r, style.Size)
				if x > 0 && x+w > width {
					flush(false)
				}
				fragment := style
				fragment.Text = string(r)
				fragment.Bounds = model.Rect{X: x, Y: 0, Width: w, Height: style.Size}
				current.runs = appendRun(current.runs, fragment)
				x += w
				lh := math.Max(style.Size*1.25, p.LineHeight)
				if lh > current.height {
					current.height = lh
				}
			}
		}
		if len(current.runs) > 0 || len(p.Runs) == 0 {
			flush(true)
		}
		for _, source := range p.Images {
			im := source
			if im.Bounds.Width <= 0 {
				im.Bounds.Width = float64(im.Width) * .75
			}
			if im.Bounds.Height <= 0 {
				im.Bounds.Height = float64(im.Height) * .75
			}
			if im.Bounds.Width > width && im.Bounds.Width > 0 {
				scale := width / im.Bounds.Width
				im.Bounds.Width = width
				im.Bounds.Height *= scale
			}
			im.Bounds.X = 0
			imageLine := newLine()
			imageLine.images = []model.Image{im}
			imageLine.height = math.Max(im.Bounds.Height, 1)
			lines = append(lines, imageLine)
		}
		lastContentLine := len(lines) - 1
		if p.After > 0 {
			lines = append(lines, line{height: p.After})
		}
		if p.SeparatorAfter != "" && lastContentLine >= firstLine {
			lines[lastContentLine].separatorAfter = p.SeparatorAfter
		}
		if p.BreakAfter {
			if len(lines) == 0 {
				lines = append(lines, line{height: 1})
			}
			lines[len(lines)-1].breakAfter = true
		}
		if p.BreakBefore && firstLine < len(lines) {
			lines[firstLine].breakBefore = true
		}
	}
	return lines
}

func defaultLineHeight(p Paragraph) float64 {
	if p.LineHeight > 0 {
		return p.LineHeight
	}
	size := 11.0
	for _, r := range p.Runs {
		if r.Size > size {
			size = r.Size
		}
	}
	return size * 1.25
}
func runeWidth(r rune, size float64) float64 {
	switch {
	case r == '\t':
		return size * 2
	case unicode.IsSpace(r):
		return size * .5
	case r < 128:
		return size * .55
	case unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul):
		return size
	default:
		return size * .8
	}
}
func appendRun(runs []model.TextRun, run model.TextRun) []model.TextRun {
	if run.Text == "" {
		return runs
	}
	if len(runs) > 0 {
		last := &runs[len(runs)-1]
		if last.Font == run.Font && last.Size == run.Size && last.Color == run.Color && math.Abs(last.Bounds.Y-run.Bounds.Y) < .01 && math.Abs(last.Bounds.X+last.Bounds.Width-run.Bounds.X) < .01 {
			last.Text += run.Text
			last.Bounds.Width += run.Bounds.Width
			if run.Bounds.Height > last.Bounds.Height {
				last.Bounds.Height = run.Bounds.Height
			}
			return runs
		}
	}
	return append(runs, run)
}
