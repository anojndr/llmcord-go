package app

import (
	"bytes"
	"encoding/binary"
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
	"golang.org/x/image/font/sfnt"
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
	tableImageEmojiFontFile        = "NotoColorEmoji.ttf"
	tableImageEmojiStrikePixels    = 128
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
	emoji     *tableImageEmojiFont
}

type tableImageFontLoader struct {
	root          string
	regularSuffix string
	boldSuffix    string
	emojiFile     string
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
		emojiFile:     tableImageEmojiFontFile,
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

	if emoji, err := loader.loadEmojiFont(); err != nil {
		logWarn("load table emoji font", err)
	} else if emoji != nil {
		fonts.emoji = emoji
	}

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

func (loader tableImageFontLoader) loadEmojiFont() (*tableImageEmojiFont, error) {
	path, ok := firstTableImageFontPath(loader.root, loader.emojiFile)
	if !ok {
		return nil, fmt.Errorf("no table font matching %q: %w", loader.emojiFile, errTableImageFontAbsent)
	}

	fontBytes, err := readTableImageFontFile(path)
	if err != nil {
		return nil, err
	}

	parsed, err := sfnt.Parse(fontBytes)
	if err != nil {
		return nil, fmt.Errorf("parse table emoji font %q: %w", path, err)
	}

	emoji, err := newTableImageEmojiFont(parsed, fontBytes)
	if err != nil {
		return nil, fmt.Errorf("index table emoji font %q: %w", path, err)
	}

	return emoji, nil
}

// tableImageEmojiFont serves Noto Color Emoji CBDT/CBLC bitmap strikes.
// opentype faces report these glyphs as colored (ErrColoredGlyph) and
// render them as tofu boxes; the embedded PNG strikes are the only
// full-color source, decoded once per glyph and cached.
type tableImageEmojiFont struct {
	parsed   *sfnt.Font
	fontData []byte
	strike   tableImageEmojiStrike
	glyphs   map[rune]tableImageEmojiGlyph
	cache    map[rune]image.Image
	mu       sync.Mutex
}

type tableImageEmojiStrike struct {
	dataOffset   int
	entries      []tableImageEmojiIndexEntry
	ppem         int
	strikePixels int
}

type tableImageEmojiIndexEntry struct {
	firstGlyph uint16
	lastGlyph  uint16
	base       int
}

type tableImageEmojiGlyph struct {
	data     []byte
	bearingX int
	bearingY int
	advance  int
	width    int
	height   int
}

// newTableImageEmojiFont indexes the single 128px CBDT/CBLC bitmap strike
// in Noto Color Emoji. Only format-1 index subtables with format-17/18
// small-glyph PNG records are supported, which covers the Noto strike.
func newTableImageEmojiFont(parsed *sfnt.Font, fontData []byte) (*tableImageEmojiFont, error) {
	strike, glyphs, err := indexTableImageEmojiStrike(parsed, fontData)
	if err != nil {
		return nil, err
	}

	return &tableImageEmojiFont{
		parsed:   parsed,
		fontData: fontData,
		strike:   strike,
		glyphs:   glyphs,
		cache:    make(map[rune]image.Image),
	}, nil
}

func indexTableImageEmojiStrike(
	parsed *sfnt.Font,
	fontData []byte,
) (tableImageEmojiStrike, map[rune]tableImageEmojiGlyph, error) {
	var strike tableImageEmojiStrike

	cblcOffset, cblcLength, found := tableImageFontTable(fontData, "CBLC")
	if !found {
		return strike, nil, fmt.Errorf("emoji font missing CBLC table: %w", errTableImageFontAbsent)
	}

	cbdtOffset, _, found := tableImageFontTable(fontData, "CBDT")
	if !found {
		return strike, nil, fmt.Errorf("emoji font missing CBDT table: %w", errTableImageFontAbsent)
	}

	if cblcLength < 8 {
		return strike, nil, fmt.Errorf("emoji CBLC table truncated: %w", errTableImageFontAbsent)
	}

	sizeCount := int(binary.BigEndian.Uint32(fontData[cblcOffset+4 : cblcOffset+8]))
	if sizeCount <= 0 || 8+sizeCount*48 > cblcLength {
		return strike, nil, fmt.Errorf("emoji CBLC size table invalid: %w", errTableImageFontAbsent)
	}

	best := -1

	for index := range sizeCount {
		ppemX, ppemY := tableImageEmojiStrikePPEM(fontData, cblcOffset+8+index*48)
		pixels := max(ppemX, ppemY)

		if best < 0 || absInt(pixels-tableImageEmojiStrikePixels) <
			absInt(strike.strikePixels-tableImageEmojiStrikePixels) {
			best = index
			strike.strikePixels = pixels
			strike.ppem = pixels
		}
	}

	sizeBase := cblcOffset + 8 + best*48
	subArrayOffset := int(binary.BigEndian.Uint32(fontData[sizeBase : sizeBase+4]))
	subCount := int(binary.BigEndian.Uint32(fontData[sizeBase+8 : sizeBase+12]))

	entries := make([]tableImageEmojiIndexEntry, 0, subCount)

	for index := range subCount {
		entryBase := cblcOffset + subArrayOffset + index*8
		first := binary.BigEndian.Uint16(fontData[entryBase : entryBase+2])
		last := binary.BigEndian.Uint16(fontData[entryBase+2 : entryBase+4])
		additional := int(binary.BigEndian.Uint32(fontData[entryBase+4 : entryBase+8]))
		entries = append(entries, tableImageEmojiIndexEntry{
			firstGlyph: first,
			lastGlyph:  last,
			base:       cblcOffset + subArrayOffset + additional,
		})
	}

	strike.entries = entries
	strike.dataOffset = cbdtOffset

	glyphs, err := tableImageEmojiGlyphRanges(parsed, strike)
	if err != nil {
		return strike, nil, err
	}

	resolved := make(map[rune]tableImageEmojiGlyph, len(glyphs))

	for textRune, glyphIndex := range glyphs {
		record, ok := tableImageEmojiGlyphRecord(fontData, strike, glyphIndex)
		if !ok {
			continue
		}

		if _, err := png.Decode(bytes.NewReader(record.data)); err != nil {
			continue
		}

		resolved[textRune] = record
	}

	if len(resolved) == 0 {
		return strike, nil, fmt.Errorf("emoji strike has no resolvable glyphs: %w", errTableImageFontAbsent)
	}

	return strike, resolved, nil
}

func tableImageEmojiStrikePPEM(fontData []byte, sizeBase int) (int, int) {
	return int(fontData[sizeBase+16]), int(fontData[sizeBase+17])
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}

	return value
}

func tableImageFontTable(fontData []byte, tag string) (int, int, bool) {
	if len(fontData) < 12 || len(tag) != 4 {
		return 0, 0, false
	}

	tableCount := int(binary.BigEndian.Uint16(fontData[4:6]))

	for index := range tableCount {
		base := 12 + index*16
		if base+16 > len(fontData) {
			return 0, 0, false
		}

		if string(fontData[base:base+4]) != tag {
			continue
		}

		offset := int(binary.BigEndian.Uint32(fontData[base+8 : base+12]))
		length := int(binary.BigEndian.Uint32(fontData[base+12 : base+16]))

		if offset < 0 || length <= 0 || offset+length > len(fontData) {
			return 0, 0, false
		}

		return offset, length, true
	}

	return 0, 0, false
}

func tableImageEmojiGlyphRanges(
	parsed *sfnt.Font,
	strike tableImageEmojiStrike,
) (map[rune]uint32, error) {
	covered := make(map[uint32]struct{})

	for _, entry := range strike.entries {
		for glyph := int(entry.firstGlyph); glyph <= int(entry.lastGlyph); glyph++ {
			covered[uint32(glyph)] = struct{}{}
		}
	}

	var buffer sfnt.Buffer

	resolved := make(map[rune]uint32)

	for textRune := rune(0x20); textRune <= rune(0x1FAFF); textRune++ {
		glyph, err := parsed.GlyphIndex(&buffer, textRune)
		if err != nil || glyph == 0 {
			continue
		}

		if _, ok := covered[uint32(glyph)]; !ok {
			continue
		}

		if !tableImageRunePrefersColorBitmap(textRune) {
			continue
		}

		resolved[textRune] = uint32(glyph)
	}

	return resolved, nil
}

// tableImageRunePrefersColorBitmap reports whether a rune should render from
// the CBDT color strike instead of a vector face. Noto Color Emoji maps many
// text codepoints (digits, #, * and other text-presentation symbols) to
// monochrome keycap-base bitmaps; those must stay on the vector path, or a
// genuinely grey emoji (🩶, 🩷) becomes indistinguishable from a keycap base
// under any pixel-color test. Supplementary-plane pictographs always render
// as emoji and take the bitmap path. BMP symbols take it only with an
// immediately following VS16 (⚡ vs ⚡️), matching platform text-vs-emoji
// behavior; without the selector the base stays vector.
func tableImageRunePrefersColorBitmap(textRune rune) bool {
	return textRune >= 0x1F000
}

func tableImageRunePrefersColorBitmapAt(runes []rune, index int) bool {
	textRune := runes[index]

	if textRune >= 0x1F000 {
		return true
	}

	return tableImageEmojiPresentationPairBefore(runes, index)
}

func tableImageEmojiGlyphRecord(
	fontData []byte,
	strike tableImageEmojiStrike,
	glyphIndex uint32,
) (tableImageEmojiGlyph, bool) {
	var empty tableImageEmojiGlyph

	for _, entry := range strike.entries {
		if glyphIndex < uint32(entry.firstGlyph) || glyphIndex > uint32(entry.lastGlyph) {
			continue
		}

		indexFormat := binary.BigEndian.Uint16(fontData[entry.base : entry.base+2])
		imageFormat := binary.BigEndian.Uint16(fontData[entry.base+2 : entry.base+4])
		imageOffset := int(binary.BigEndian.Uint32(fontData[entry.base+4 : entry.base+8]))

		if indexFormat != 1 || (imageFormat != 17 && imageFormat != 18) {
			continue
		}

		slot := int(glyphIndex) - int(entry.firstGlyph)
		offsetBase := entry.base + 8 + slot*4
		start := int(binary.BigEndian.Uint32(fontData[offsetBase : offsetBase+4]))
		end := int(binary.BigEndian.Uint32(fontData[offsetBase+4 : offsetBase+8]))

		if end <= start {
			continue
		}

		recordBase := strike.dataOffset + imageOffset + start
		if recordBase+8 > len(fontData) || recordBase+end-start > len(fontData) {
			continue
		}

		record := fontData[recordBase : recordBase+(end-start)]
		if len(record) < 8 {
			continue
		}

		height := int(record[0])
		width := int(record[1])

		pngStart := -1

		for offset := range min(16, len(record)) {
			if len(record[offset:]) >= 4 && string(record[offset:offset+4]) == "\x89PNG" {
				pngStart = offset

				break
			}
		}

		if pngStart < 0 || width <= 0 || height <= 0 {
			continue
		}

		return tableImageEmojiGlyph{
			data:     record[pngStart:],
			bearingX: int(int8(record[2])),
			bearingY: int(int8(record[3])),
			advance:  int(record[4]),
			width:    width,
			height:   height,
		}, true
	}

	return empty, false
}

func (emoji *tableImageEmojiFont) has(textRune rune) bool {
	if emoji == nil {
		return false
	}

	_, ok := emoji.glyphs[textRune]

	return ok
}

func (emoji *tableImageEmojiFont) advancePixels(lineHeight int) int {
	if emoji == nil {
		return 0
	}

	return max(lineHeight, int(float64(lineHeight)*1.1))
}

func (emoji *tableImageEmojiFont) decoded(textRune rune) (image.Image, bool) {
	if emoji == nil {
		return nil, false
	}

	emoji.mu.Lock()
	defer emoji.mu.Unlock()

	if cached, ok := emoji.cache[textRune]; ok {
		return cached, true
	}

	record, ok := emoji.glyphs[textRune]
	if !ok || len(record.data) == 0 {
		return nil, false
	}

	decoded, err := png.Decode(bytes.NewReader(record.data))
	if err != nil {
		delete(emoji.glyphs, textRune)

		return nil, false
	}

	emoji.cache[textRune] = decoded

	return decoded, true
}

func (emoji *tableImageEmojiFont) draw(
	dst *image.RGBA,
	textRune rune,
	dotX int,
	baselineY int,
	lineHeight int,
) (int, bool) {
	glyph, ok := emoji.glyphs[textRune]
	if !ok {
		return 0, false
	}

	decoded, ok := emoji.decoded(textRune)
	if !ok {
		return 0, false
	}

	bounds := decoded.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return 0, false
	}

	target := max(1, int(float64(lineHeight)*1.2))
	scaled := decoded

	if bounds.Dx() != target || bounds.Dy() != target {
		fitted := image.NewRGBA(image.Rect(0, 0, target, target))
		draw.ApproxBiLinear.Scale(fitted, fitted.Bounds(), decoded, bounds, draw.Over, nil)
		scaled = fitted
	}

	ascent := int(float64(lineHeight) * 0.85)
	top := baselineY - ascent - (target-ascent)/2
	draw.Draw(dst, image.Rect(dotX, top, dotX+target, top+target), scaled, image.Point{}, draw.Over)

	_ = glyph

	return emoji.advancePixels(lineHeight), true
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
			cellWidth := tableImageMeasure(fonts, face, cell).Ceil() + 2*tableImageCellPaddingX
			widths[column] = max(widths[column], cellWidth)

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

func tableImageMeasure(fonts tableImageFontSet, face font.Face, text string) fixed.Int26_6 {
	var width fixed.Int26_6

	runes := []rune(text)

	for index, textRune := range runes {
		if tableImageIsEmojiModifier(textRune) {
			continue
		}

		width += tableImageMeasuredAdvance(fonts, face, runes, index)
	}

	return width
}

func tableImageMeasuredAdvance(fonts tableImageFontSet, face font.Face, runes []rune, index int) fixed.Int26_6 {
	textRune := runes[index]

	if fonts.emoji != nil && fonts.emoji.has(textRune) &&
		tableImageRunePrefersColorBitmapAt(runes, index) {
		return fixed.I(fonts.emoji.advancePixels(tableImageLineHeight(face)))
	}

	return tableImageVectorAdvance(fonts, face, textRune)
}

func tableImageVectorAdvance(fonts tableImageFontSet, face font.Face, textRune rune) fixed.Int26_6 {
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
	if fonts.emoji != nil && fonts.emoji.has(textRune) {
		return face
	}

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

func tableImageDrawString(fonts tableImageFontSet, active *font.Drawer, text string) {
	runes := []rune(text)

	for index, textRune := range runes {
		if tableImageIsEmojiModifier(textRune) {
			continue
		}

		if fonts.emoji != nil && fonts.emoji.has(textRune) &&
			tableImageRunePrefersColorBitmapAt(runes, index) {
			tableImageDrawEmoji(fonts, active, textRune)

			continue
		}

		tableImageDrawVectorGlyph(fonts, active, textRune)
	}
}

// tableImageEmojiPresentationPairBefore reports whether the rune at index is
// immediately followed by VS16, requesting emoji presentation for a BMP base
// (⚡ vs ⚡️). The bitmap path applies only with the selector; without it the
// base stays vector, matching platform text-vs-emoji behavior.
func tableImageEmojiPresentationPairBefore(runes []rune, index int) bool {
	return index+1 < len(runes) && runes[index+1] == 0xFE0F
}

func tableImageDrawVectorGlyph(fonts tableImageFontSet, active *font.Drawer, textRune rune) {
	previous := active.Dot
	active.Face = tableImageRuneFace(fonts, active.Face, textRune)
	active.Dot = previous
	active.DrawString(string(textRune))
}

// tableImageIsEmojiModifier reports variation selectors, ZWJ, keycap marks,
// and skin-tone modifiers. The CBDT strike has no glyphs for these shaping
// runes (VS16 maps to glyph 0), and drawing them with a vector face emits
// tofu boxes over the color bitmap. They only steer GSUB shaping, which the
// bitmap renderer does not perform.
func tableImageIsEmojiModifier(textRune rune) bool {
	switch {
	case textRune == 0x200D || textRune == 0xFE0E || textRune == 0xFE0F || textRune == 0x20E3:
		return true
	case textRune >= 0x1F3FB && textRune <= 0x1F3FF:
		return true
	}

	return false
}

func tableImageDrawEmoji(fonts tableImageFontSet, active *font.Drawer, textRune rune) {
	emoji := fonts.emoji
	if emoji == nil {
		return
	}

	dst, ok := active.Dst.(*image.RGBA)
	if !ok {
		tableImageDrawEmojiFallback(fonts, active, textRune)

		return
	}

	advance, drawn := tableImageBlitEmoji(
		emoji,
		dst,
		active,
		textRune,
	)
	if !drawn {
		tableImageDrawEmojiFallback(fonts, active, textRune)

		return
	}

	active.Dot.X += fixed.I(advance)
}

func tableImageBlitEmoji(
	emoji *tableImageEmojiFont,
	dst *image.RGBA,
	active *font.Drawer,
	textRune rune,
) (int, bool) {
	lineHeight := tableImageLineHeight(active.Face)
	baselineY := tableImageEmojiBaselineY(active)

	return emoji.draw(dst, textRune, active.Dot.X.Ceil(), baselineY, lineHeight)
}

func tableImageDrawEmojiFallback(fonts tableImageFontSet, active *font.Drawer, textRune rune) {
	previous := active.Dot
	active.Face = tableImageRuneFace(fonts, active.Face, textRune)
	active.Dot = previous
	active.DrawString(string(textRune))
}

func tableImageEmojiBaselineY(active *font.Drawer) int {
	if baseline := active.Dot.Y.Ceil(); baseline != 0 {
		return baseline
	}

	return active.Face.Metrics().Ascent.Ceil()
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
