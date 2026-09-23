package app

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/image/draw"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

const (
	tableImageMaxColumns           = 10
	tableImageMaxRows              = 60
	tableImageMaxCellRunes         = 120
	tableImageMaxPNGBytes          = 8 * 1024 * 1024
	tableImageMaxFallbackFaces     = 32
	tableImageFontSize             = 15
	tableImageHeaderFontSize       = 15
	tableImageLineHeightMultiplier = 1.35
	tableImageCellPaddingX         = 14
	tableImageCellPaddingY         = 10
	tableImageMinColumnWidth       = 60
	tableImageMaxColumnWidth       = 520
	tableImageMaxRenderWidth       = 1600
	tableImageBackground           = 0x2b2d31
	tableImageHeaderBackground     = 0x1e1f22
	tableImageGridColor            = 0x4e5058
	tableImageTextColor            = 0xf2f3f5
	tableImageHeaderTextColor      = 0xffffff
	tableImageFontDPI              = 144
	tableImageSystemFontDir        = "/usr/share/fonts"
	tableImageSystemFontRegular    = "NotoSans-Regular.ttf"
	tableImageSystemFontBold       = "NotoSans-Bold.ttf"
)

var tableImageFallbackFontOrder = []string{
	"NotoSansCJK-Regular.ttc",
	"NotoSansArabic-Regular.ttf",
	"NotoSansHebrew-Regular.ttf",
	"NotoSansThai-Regular.ttf",
	"NotoSansDevanagari-Regular.ttf",
	"NotoSansBengali-Regular.ttf",
	"NotoSansTamil-Regular.ttf",
	"NotoSansTelugu-Regular.ttf",
	"NotoSansKannada-Regular.ttf",
	"NotoSansMalayalam-Regular.ttf",
	"NotoSansGujarati-Regular.ttf",
	"NotoSansGurmukhi-Regular.ttf",
	"NotoSansOriya-Regular.ttf",
	"NotoSansSinhala-Regular.ttf",
	"NotoSansMyanmar-Regular.ttf",
	"NotoSansKhmer-Regular.ttf",
	"NotoSansLao-Regular.ttf",
	"NotoSansEthiopic-Regular.ttf",
	"NotoSansGeorgian-Regular.ttf",
	"NotoSansArmenian-Regular.ttf",
}

var tableImageFallbackFontFiles = func() map[string]struct{} {
	files := make(map[string]struct{}, len(tableImageFallbackFontOrder))

	for _, name := range tableImageFallbackFontOrder {
		files[name] = struct{}{}
	}

	return files
}()

type markdownTable struct {
	header []string
	rows   [][]string
}

type markdownTableBlock struct {
	table markdownTable
	start int
	end   int
}

type tableImageFontSet struct {
	regular   font.Face
	bold      font.Face
	fallbacks []font.Face
}

type tableImageFontLoader struct {
	root          string
	regularSuffix string
	boldSuffix    string
}

var (
	tableImageFontsOnce sync.Once
	tableImageFonts     tableImageFontSet
	tableImageFontsErr  error
	// tableImageRenderMu serializes table renders: opentype.Face keeps a
	// mutable rasterizer buffer and is not safe for concurrent use, while
	// responses render on per-message goroutines.
	tableImageRenderMu      sync.Mutex
	errTableImageFontAbsent = errors.New("table font not found")
	errTableImageTooLarge   = errors.New("table image too large")
)

func loadTableImageFonts() (tableImageFontSet, error) {
	tableImageFontsOnce.Do(func() {
		tableImageFonts, tableImageFontsErr = newTableImageFontSet()
	})

	return tableImageFonts, tableImageFontsErr
}

func newTableImageFontSet() (tableImageFontSet, error) {
	loader := tableImageFontLoader{
		root:          tableImageSystemFontDir,
		regularSuffix: tableImageSystemFontRegular,
		boldSuffix:    tableImageSystemFontBold,
	}

	return loader.load()
}

func (loader tableImageFontLoader) load() (tableImageFontSet, error) {
	var fonts tableImageFontSet

	regularFont, err := loader.parseFirstFont(loader.regularSuffix, goregular.TTF)
	if err != nil {
		return tableImageFontSet{}, err
	}

	boldFont, err := loader.parseFirstFont(loader.boldSuffix, gobold.TTF)
	if err != nil {
		return tableImageFontSet{}, err
	}

	fonts.regular, err = newTableImageFace(regularFont, tableImageFontSize)
	if err != nil {
		return tableImageFontSet{}, err
	}

	fonts.bold, err = newTableImageFace(boldFont, tableImageHeaderFontSize)
	if err != nil {
		return tableImageFontSet{}, err
	}

	fonts.fallbacks = loader.loadFallbackFaces()

	return fonts, nil
}

func newTableImageFace(parsedFont *opentype.Font, size float64) (font.Face, error) {
	face, err := opentype.NewFace(parsedFont, &opentype.FaceOptions{
		Size:    size,
		DPI:     tableImageFontDPI,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return nil, fmt.Errorf("create table font face: %w", err)
	}

	return face, nil
}

func (loader tableImageFontLoader) parseFirstFont(suffix string, fallback []byte) (*opentype.Font, error) {
	if path, ok := firstTableImageFontPath(loader.root, suffix); ok {
		fontBytes, readErr := readTableImageFontFile(path)
		if readErr == nil {
			parsed, parseErr := opentype.Parse(fontBytes)
			if parseErr == nil {
				return parsed, nil
			}

			logWarn("parse table system font", parseErr, "path", path)
		} else {
			logWarn("read table system font", readErr, "path", path)
		}
	}

	parsed, err := opentype.Parse(fallback)
	if err != nil {
		return nil, fmt.Errorf("parse table fallback font: %w", err)
	}

	return parsed, nil
}

func (loader tableImageFontLoader) loadFallbackFaces() []font.Face {
	paths := tableImageFallbackFontPaths(loader.root)
	fallbacks := make([]font.Face, 0, len(paths))

	for _, path := range paths {
		fontBytes, err := readTableImageFontFile(path)
		if err != nil {
			logWarn("read table fallback font", err, "path", path)

			continue
		}

		for _, parsed := range parseTableImageFonts(path, fontBytes) {
			face, err := newTableImageFace(parsed, tableImageFontSize)
			if err != nil {
				logWarn("create table fallback face", err, "path", path)

				continue
			}

			fallbacks = append(fallbacks, face)

			if len(fallbacks) >= tableImageMaxFallbackFaces {
				return fallbacks
			}
		}
	}

	return fallbacks
}

func parseTableImageFonts(path string, fontBytes []byte) []*opentype.Font {
	if parsed, err := opentype.Parse(fontBytes); err == nil {
		return []*opentype.Font{parsed}
	}

	collection, err := opentype.ParseCollection(fontBytes)
	if err != nil {
		logWarn("parse table fallback font", err, "path", path)

		return nil
	}

	parsed := make([]*opentype.Font, 0, collection.NumFonts())

	for index := range collection.NumFonts() {
		collectionFont, err := collection.Font(index)
		if err != nil {
			logWarn("select table fallback font", err, "path", path, "font_index", index)

			continue
		}

		parsed = append(parsed, collectionFont)
	}

	return parsed
}

func tableImageFallbackFontPaths(root string) []string {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}

	collector := tableImageFallbackCollector{found: make(map[string]struct{}, len(tableImageFallbackFontFiles))}

	_ = filepath.WalkDir(root, collector.visit)

	paths := make([]string, 0, len(tableImageFallbackFontFiles))

	for _, name := range tableImageFallbackFontOrder {
		for _, path := range collector.paths {
			if strings.HasSuffix(path, "/"+name) {
				paths = append(paths, path)

				break
			}
		}

		if len(paths) >= tableImageMaxFallbackFaces {
			break
		}
	}

	return paths
}

type tableImageFallbackCollector struct {
	found map[string]struct{}
	paths []string
}

func (collector *tableImageFallbackCollector) visit(path string, entry fs.DirEntry, walkErr error) error {
	if walkErr != nil || entry.IsDir() {
		return nil
	}

	if _, wanted := tableImageFallbackFontFiles[entry.Name()]; !wanted {
		return nil
	}

	if _, seen := collector.found[entry.Name()]; seen {
		return nil
	}

	collector.found[entry.Name()] = struct{}{}
	collector.paths = append(collector.paths, path)

	return nil
}

func firstTableImageFontPath(root, suffix string) (string, bool) {
	root = strings.TrimSpace(root)
	suffix = strings.TrimSpace(suffix)

	if root == "" || suffix == "" {
		return "", false
	}

	var match string

	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || match != "" {
			return nil
		}

		if entry.IsDir() {
			return nil
		}

		if strings.HasSuffix(entry.Name(), suffix) {
			match = path
		}

		return nil
	})

	if match == "" {
		return "", false
	}

	return match, true
}

func readTableImageFontFile(path string) ([]byte, error) {
	cleaned := filepath.Clean(path)

	fontBytes, err := os.ReadFile(cleaned)
	if err != nil {
		return nil, fmt.Errorf("read table font %q: %w", cleaned, err)
	}

	if len(fontBytes) == 0 {
		return nil, fmt.Errorf("empty table font %q: %w", cleaned, errTableImageFontAbsent)
	}

	return fontBytes, nil
}

func parseMarkdownTableBlocks(text string) []markdownTableBlock {
	lines := strings.Split(text, "\n")
	blocks := make([]markdownTableBlock, 0, 1)
	lineStartOffsets := make([]int, len(lines))

	offset := 0

	for index, line := range lines {
		lineStartOffsets[index] = offset
		offset += len(line) + 1
	}

	for index := 0; index+1 < len(lines); index++ {
		headerCells, headerOK := parseMarkdownTableRow(lines[index])
		if !headerOK || len(headerCells) > tableImageMaxColumns {
			continue
		}

		if !isMarkdownTableDelimiter(lines[index+1], len(headerCells)) {
			continue
		}

		header := normalizeMarkdownTableCells(headerCells)

		rows := make([][]string, 0, 8)
		next := index + 2

		for next < len(lines) {
			cells, rowOK := parseMarkdownTableRow(lines[next])
			if !rowOK || len(cells) != len(header) {
				break
			}

			rows = append(rows, normalizeMarkdownTableCells(cells))
			next++

			if len(rows) >= tableImageMaxRows {
				break
			}
		}

		if len(rows) == 0 {
			continue
		}

		blocks = append(blocks, markdownTableBlock{
			table: markdownTable{header: header, rows: rows},
			start: lineStartOffsets[index],
			end:   lineStartOffsets[next-1] + len(lines[next-1]),
		})

		index = next - 1
	}

	return blocks
}

func parseMarkdownTableRow(line string) ([]string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.Contains(trimmed, "|") || strings.Contains(trimmed, "\t") {
		return nil, false
	}

	trimmed = strings.TrimSpace(strings.Trim(trimmed, "|"))
	if trimmed == "" {
		return nil, false
	}

	rawCells := strings.Split(trimmed, "|")
	cells := make([]string, 0, len(rawCells))

	for _, rawCell := range rawCells {
		cells = append(cells, strings.TrimSpace(rawCell))
	}

	if len(cells) > tableImageMaxColumns {
		return nil, false
	}

	for _, cell := range cells {
		if cell != "" {
			return cells, true
		}
	}

	return nil, false
}

func isMarkdownTableDelimiter(line string, columns int) bool {
	cells, ok := parseMarkdownTableRow(line)
	if !ok || len(cells) != columns {
		return false
	}

	for _, cell := range cells {
		inner := strings.Trim(strings.TrimSpace(cell), ":")
		if inner == "" {
			return false
		}

		for _, delimiterRune := range inner {
			if delimiterRune != '-' {
				return false
			}
		}
	}

	return true
}

func normalizeMarkdownTableCells(cells []string) []string {
	normalized := make([]string, len(cells))

	for index, cell := range cells {
		cleaned := stripMarkdownTableCellFormatting(cell)
		if runeCount(cleaned) > tableImageMaxCellRunes {
			cleaned = truncateRunes(cleaned, tableImageMaxCellRunes)
		}

		normalized[index] = cleaned
	}

	return normalized
}

func stripMarkdownTableCellFormatting(cell string) string {
	cleaned := strings.TrimSpace(cell)
	cleaned = strings.ReplaceAll(cleaned, "`", "")
	cleaned = strings.ReplaceAll(cleaned, "**", "")
	cleaned = strings.ReplaceAll(cleaned, "__", "")
	cleaned = strings.ReplaceAll(cleaned, "<br/>", " ")
	cleaned = strings.ReplaceAll(cleaned, "<br>", " ")
	cleaned = strings.Join(strings.Fields(cleaned), " ")

	return cleaned
}

func renderMarkdownTablePNG(table markdownTable) ([]byte, error) {
	fonts, err := loadTableImageFonts()
	if err != nil {
		return nil, err
	}

	tableImageRenderMu.Lock()
	defer tableImageRenderMu.Unlock()

	grid := tableImageGrid(table)
	columnWidths := measureTableColumnWidths(grid, fonts)
	lineHeight := tableImageLineHeight(fonts.regular)

	rowHeights := make([]int, len(grid))
	for row := range grid {
		face := fonts.regular
		if row == 0 {
			face = fonts.bold
		}

		rowHeights[row] = wrappedTableRowHeight(fonts, grid[row], columnWidths, face, lineHeight) +
			2*tableImageCellPaddingY
	}

	width := 1

	for _, columnWidth := range columnWidths {
		width += columnWidth + 1
	}

	height := 1

	for _, rowHeight := range rowHeights {
		height += rowHeight + 1
	}

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fillTableImageRect(img, img.Bounds(), tableColor(tableImageBackground))

	drawer := &font.Drawer{Dst: img, Src: image.NewUniform(tableColor(tableImageTextColor))}
	headerDrawer := &font.Drawer{Dst: img, Src: image.NewUniform(tableColor(tableImageHeaderTextColor))}
	gridColor := image.NewUniform(tableColor(tableImageGridColor))

	y := 0

	for row := range grid {
		face := fonts.regular
		active := drawer

		if row == 0 {
			face = fonts.bold
			active = headerDrawer

			fillTableImageRect(img, image.Rect(0, y, width, y+rowHeights[row]+1), tableColor(tableImageHeaderBackground))
		}

		drawTableRow(img, fonts, active, grid[row], columnWidths, face, lineHeight, y)
		y += rowHeights[row] + 1

		drawTableHorizontalLine(img, gridColor, 0, y-1, width)
	}

	drawTableVerticalLines(img, gridColor, columnWidths, height)

	var rendered bytes.Buffer

	encoder := png.Encoder{CompressionLevel: png.BestCompression}
	if err := encoder.Encode(&rendered, downscaleTableImage(img)); err != nil {
		return nil, fmt.Errorf("encode table image: %w", err)
	}

	if rendered.Len() > tableImageMaxPNGBytes {
		return nil, fmt.Errorf("table image exceeds size limit: %w", errTableImageTooLarge)
	}

	return rendered.Bytes(), nil
}

func tableImageGrid(table markdownTable) [][]string {
	grid := make([][]string, 0, len(table.rows)+1)
	grid = append(grid, table.header)
	grid = append(grid, table.rows...)

	return grid
}

func measureTableColumnWidths(grid [][]string, fonts tableImageFontSet) []int {
	widths := make([]int, len(grid[0]))

	for column := range widths {
		widths[column] = tableImageMinColumnWidth
	}

	for rowIndex, row := range grid {
		face := fonts.regular
		if rowIndex == 0 {
			face = fonts.bold
		}

		for column, cell := range row {
			for _, word := range strings.Fields(cell) {
				wordWidth := tableImageMeasure(fonts, face, word).Ceil() + 2*tableImageCellPaddingX
				widths[column] = max(widths[column], wordWidth)
			}
		}
	}

	for column := range widths {
		widths[column] = min(widths[column], tableImageMaxColumnWidth)
	}

	total := 1

	for _, width := range widths {
		total += width + 1
	}

	if total <= tableImageMaxRenderWidth {
		return widths
	}

	overflow := total - tableImageMaxRenderWidth
	shrinkable := 0

	for _, width := range widths {
		shrinkable += max(0, width-tableImageMinColumnWidth)
	}

	if shrinkable == 0 {
		return widths
	}

	for column := range widths {
		slack := max(0, widths[column]-tableImageMinColumnWidth)
		widths[column] = max(widths[column]-slack*overflow/shrinkable, tableImageMinColumnWidth)
	}

	return widths
}

func tableImageLineHeight(face font.Face) int {
	metrics := face.Metrics()

	return max(1, (metrics.Ascent + metrics.Descent).Ceil())
}

func wrappedTableRowHeight(
	fonts tableImageFontSet,
	row []string,
	columnWidths []int,
	face font.Face,
	lineHeight int,
) int {
	maxLines := 1

	for column, cell := range row {
		lines := wrapTableCellLines(fonts, face, cell, columnWidths[column]-2*tableImageCellPaddingX)
		maxLines = max(maxLines, len(lines))
	}

	return maxLines * max(1, int(float64(lineHeight)*tableImageLineHeightMultiplier))
}

func wrapTableCellLines(fonts tableImageFontSet, face font.Face, cell string, maxWidth int) []string {
	words := strings.Fields(cell)
	if len(words) == 0 {
		return []string{""}
	}

	maxWidth = max(20, maxWidth)
	lines := make([]string, 0, 2)
	current := ""

	for _, word := range words {
		if tableImageMeasure(fonts, face, word).Ceil() > maxWidth {
			if current != "" {
				lines = append(lines, current)
				current = ""
			}

			lines = append(lines, splitWideTableWord(fonts, face, word, maxWidth)...)

			continue
		}

		candidate := word
		if current != "" {
			candidate = current + " " + word
		}

		if tableImageMeasure(fonts, face, candidate).Ceil() <= maxWidth {
			current = candidate

			continue
		}

		lines = append(lines, current)
		current = word
	}

	if current != "" {
		lines = append(lines, current)
	}

	if len(lines) == 0 {
		return []string{""}
	}

	return lines
}

func splitWideTableWord(fonts tableImageFontSet, face font.Face, word string, maxWidth int) []string {
	parts := make([]string, 0, 2)
	current := ""

	for _, wordRune := range word {
		candidate := current + string(wordRune)
		if current == "" || tableImageMeasure(fonts, face, candidate).Ceil() <= maxWidth {
			current = candidate

			continue
		}

		parts = append(parts, current)
		current = string(wordRune)
	}

	if current != "" {
		parts = append(parts, current)
	}

	return parts
}

// tableImageMeasure widths text rune by rune so glyphs missing from the
// primary face measure with the fallback face that will actually render
// them. Kerning is ignored: table cells are word-wrapped independently, so
// sub-pixel kerning precision buys nothing.
func tableImageMeasure(fonts tableImageFontSet, face font.Face, text string) fixed.Int26_6 {
	var width fixed.Int26_6

	for _, textRune := range text {
		width += tableImageRuneAdvance(fonts, face, textRune)
	}

	return width
}

func tableImageRuneAdvance(fonts tableImageFontSet, face font.Face, textRune rune) fixed.Int26_6 {
	if _, ok := face.GlyphAdvance(textRune); ok {
		advance, _ := face.GlyphAdvance(textRune)

		return advance
	}

	for _, fallback := range fonts.fallbacks {
		if fallback == nil {
			continue
		}

		if fallbackAdvance, ok := fallback.GlyphAdvance(textRune); ok {
			return fallbackAdvance
		}
	}

	advance, _ := face.GlyphAdvance(textRune)

	return advance
}

func tableImageRuneFace(fonts tableImageFontSet, face font.Face, textRune rune) font.Face {
	if _, ok := face.GlyphAdvance(textRune); ok {
		return face
	}

	for _, fallback := range fonts.fallbacks {
		if fallback == nil {
			continue
		}

		if _, ok := fallback.GlyphAdvance(textRune); ok {
			return fallback
		}
	}

	return face
}

func drawTableRow(
	img *image.RGBA,
	fonts tableImageFontSet,
	active *font.Drawer,
	row []string,
	columnWidths []int,
	face font.Face,
	lineHeight int,
	y int,
) {
	scaledLineHeight := max(1, int(float64(lineHeight)*tableImageLineHeightMultiplier))
	metrics := face.Metrics()
	baselineOffset := tableImageCellPaddingY + metrics.Ascent.Ceil()
	x := 1

	active.Face = face

	for column, cell := range row {
		lines := wrapTableCellLines(fonts, face, cell, columnWidths[column]-2*tableImageCellPaddingX)

		for lineIndex, line := range lines {
			active.Dot = fixed.P(x+tableImageCellPaddingX, y+baselineOffset+lineIndex*scaledLineHeight)
			tableImageDrawString(fonts, active, line)
		}

		x += columnWidths[column] + 1
	}
}

// tableImageDrawString draws rune by rune so glyphs missing from the active
// face fall back through the loaded Noto chain instead of rendering as tofu.
// Runes missing from every face render with the primary face, preserving
// position.
func tableImageDrawString(fonts tableImageFontSet, active *font.Drawer, text string) {
	for _, textRune := range text {
		previous := active.Dot
		active.Face = tableImageRuneFace(fonts, active.Face, textRune)
		active.Dot = previous
		active.DrawString(string(textRune))
	}
}

func drawTableHorizontalLine(img *image.RGBA, src image.Image, x0, y, x1 int) {
	line := image.Rect(x0, y, x1, y+1)
	draw.Draw(img, line, src, image.Point{}, draw.Src)
}

func drawTableVerticalLines(img *image.RGBA, src image.Image, columnWidths []int, height int) {
	x := 0
	draw.Draw(img, image.Rect(x, 0, x+1, height), src, image.Point{}, draw.Src)

	for _, width := range columnWidths {
		x += width + 1
		draw.Draw(img, image.Rect(x-1, 0, x, height), src, image.Point{}, draw.Src)
	}
}

func fillTableImageRect(img *image.RGBA, rect image.Rectangle, fill color.Color) {
	draw.Draw(img, rect, &image.Uniform{C: fill}, image.Point{}, draw.Src)
}

func tableColor(hex uint32) color.RGBA {
	return color.RGBA{R: uint8(hex >> 16), G: uint8(hex >> 8), B: uint8(hex), A: 0xff}
}

func downscaleTableImage(img *image.RGBA) image.Image {
	bounds := img.Bounds()
	if bounds.Dx() <= tableImageMaxRenderWidth {
		return img
	}

	ratio := float64(tableImageMaxRenderWidth) / float64(bounds.Dx())
	dst := image.NewRGBA(image.Rect(0, 0, tableImageMaxRenderWidth, max(1, int(float64(bounds.Dy())*ratio))))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), img, bounds, draw.Src, nil)

	return dst
}

type renderedTableImage struct {
	data     []byte
	filename string
}

// renderMarkdownTableImages renders every markdown table in text to a
// high-resolution PNG. History keeps the original markdown: release()
// persists the untouched model text and image replies are cached as empty
// auxiliary nodes, so follow-up turns still see the full table text.
func renderMarkdownTableImages(text string) []renderedTableImage {
	blocks := parseMarkdownTableBlocks(text)
	if len(blocks) == 0 {
		return nil
	}

	images := make([]renderedTableImage, 0, len(blocks))

	for index, block := range blocks {
		imageBytes, err := renderMarkdownTablePNG(block.table)
		if err != nil {
			logWarn("render markdown table image", err, "table_index", index)

			continue
		}

		images = append(images, renderedTableImage{
			data:     imageBytes,
			filename: fmt.Sprintf("table-%d.png", len(images)+1),
		})
	}

	return images
}
