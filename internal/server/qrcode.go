package bleephub

import (
	"fmt"
)

// A minimal QR Code encoder (ISO/IEC 18004) for otpauth:// provisioning URIs.
// Scope is narrow — byte mode, EC level M, versions 1..10 (213 bytes) — which
// is all a provisioning URI needs; anything longer is refused, not truncated.
// The server emits the module matrix and the browser draws it as themed SVG.

const (
	// qrECLevelM is the 2-bit EC level indicator for level M (~15% recovery).
	qrECLevelM = 0b00
	// qrByteMode is the 4-bit mode indicator for 8-bit byte mode.
	qrByteMode   = 0b0100
	qrMaxVersion = 10
)

// qrVersionSpec is one version's block structure at EC level M (ISO/IEC 18004
// tables 9 and E.1): RS block split, EC codewords per block, alignment centers.
type qrVersionSpec struct {
	ecPerBlock   int
	group1Blocks int
	group1Data   int
	group2Blocks int
	group2Data   int
	alignment    []int
}

func (spec qrVersionSpec) dataCodewords() int {
	return spec.group1Blocks*spec.group1Data + spec.group2Blocks*spec.group2Data
}

func (spec qrVersionSpec) blocks() int { return spec.group1Blocks + spec.group2Blocks }

var qrVersionsLevelM = [qrMaxVersion + 1]qrVersionSpec{
	1:  {ecPerBlock: 10, group1Blocks: 1, group1Data: 16},
	2:  {ecPerBlock: 16, group1Blocks: 1, group1Data: 28, alignment: []int{6, 18}},
	3:  {ecPerBlock: 26, group1Blocks: 1, group1Data: 44, alignment: []int{6, 22}},
	4:  {ecPerBlock: 18, group1Blocks: 2, group1Data: 32, alignment: []int{6, 26}},
	5:  {ecPerBlock: 24, group1Blocks: 2, group1Data: 43, alignment: []int{6, 30}},
	6:  {ecPerBlock: 16, group1Blocks: 4, group1Data: 27, alignment: []int{6, 34}},
	7:  {ecPerBlock: 18, group1Blocks: 4, group1Data: 31, alignment: []int{6, 22, 38}},
	8:  {ecPerBlock: 22, group1Blocks: 2, group1Data: 38, group2Blocks: 2, group2Data: 39, alignment: []int{6, 24, 42}},
	9:  {ecPerBlock: 22, group1Blocks: 3, group1Data: 36, group2Blocks: 2, group2Data: 37, alignment: []int{6, 26, 46}},
	10: {ecPerBlock: 26, group1Blocks: 4, group1Data: 43, group2Blocks: 1, group2Data: 44, alignment: []int{6, 28, 50}},
}

// qrRemainderBits is the zero bits appended after the codeword stream so it
// fills the symbol (ISO/IEC 18004 table 1).
var qrRemainderBits = [qrMaxVersion + 1]int{0, 0, 7, 7, 7, 7, 7, 0, 0, 0, 0}

// GF(2^8) modulo 0x11D with 2 as generator, as specified for QR Codes.
var (
	qrGFExp [512]byte
	qrGFLog [256]byte
)

func init() {
	value := 1
	for i := 0; i < 255; i++ {
		qrGFExp[i] = byte(value)
		qrGFLog[value] = byte(i)
		value <<= 1
		if value&0x100 != 0 {
			value ^= 0x11d
		}
	}
	for i := 255; i < 512; i++ {
		qrGFExp[i] = qrGFExp[i-255]
	}
}

func qrGFMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return qrGFExp[int(qrGFLog[a])+int(qrGFLog[b])]
}

// qrGeneratorPoly builds the degree-`degree` RS generator polynomial
// (x-2^0)(x-2^1)…, coefficients high-order first.
func qrGeneratorPoly(degree int) []byte {
	poly := []byte{1}
	for i := 0; i < degree; i++ {
		next := make([]byte, len(poly)+1)
		for j, coefficient := range poly {
			next[j] ^= coefficient
			next[j+1] ^= qrGFMul(coefficient, qrGFExp[i])
		}
		poly = next
	}
	return poly
}

// qrErrorCorrection returns the `count` EC codewords for one data block (the
// block polynomial's remainder modulo the generator).
func qrErrorCorrection(data []byte, count int) []byte {
	generator := qrGeneratorPoly(count)
	remainder := make([]byte, count)
	for _, datum := range data {
		factor := datum ^ remainder[0]
		copy(remainder, remainder[1:])
		remainder[count-1] = 0
		if factor != 0 {
			for i := 0; i < count; i++ {
				remainder[i] ^= qrGFMul(generator[i+1], factor)
			}
		}
	}
	return remainder
}

type qrSymbol struct {
	size     int
	modules  []bool // row-major; true = dark
	function []bool // true where a function pattern owns the module
}

func newQRSymbol(version int) *qrSymbol {
	size := version*4 + 17
	return &qrSymbol{size: size, modules: make([]bool, size*size), function: make([]bool, size*size)}
}

func (q *qrSymbol) at(row, col int) bool        { return q.modules[row*q.size+col] }
func (q *qrSymbol) set(row, col int, dark bool) { q.modules[row*q.size+col] = dark }

func (q *qrSymbol) setFunction(row, col int, dark bool) {
	if row < 0 || col < 0 || row >= q.size || col >= q.size {
		return
	}
	q.set(row, col, dark)
	q.function[row*q.size+col] = true
}

func (q *qrSymbol) isFunction(row, col int) bool { return q.function[row*q.size+col] }

// qrEncode renders text as a QR Code, one string per row, each char '0' (light)
// or '1' (dark).
func qrEncode(text string) ([]string, error) {
	version, spec, err := qrSelectVersion(len(text))
	if err != nil {
		return nil, err
	}
	codewords := qrCodewords(version, spec, []byte(text))

	symbol := newQRSymbol(version)
	symbol.drawFunctionPatterns(version, spec)
	symbol.drawCodewords(codewords, qrRemainderBits[version])

	best, bestPenalty := 0, -1
	for mask := 0; mask < 8; mask++ {
		symbol.applyMask(mask)
		symbol.drawFormatBits(mask)
		penalty := symbol.penalty()
		if bestPenalty < 0 || penalty < bestPenalty {
			best, bestPenalty = mask, penalty
		}
		symbol.applyMask(mask) // XOR is its own inverse: undo before the next trial.
	}
	symbol.applyMask(best)
	symbol.drawFormatBits(best)

	rows := make([]string, symbol.size)
	for row := 0; row < symbol.size; row++ {
		line := make([]byte, symbol.size)
		for col := 0; col < symbol.size; col++ {
			line[col] = '0'
			if symbol.at(row, col) {
				line[col] = '1'
			}
		}
		rows[row] = string(line)
	}
	return rows, nil
}

// qrSelectVersion picks the smallest version whose level-M byte-mode capacity
// holds `length` bytes.
func qrSelectVersion(length int) (int, qrVersionSpec, error) {
	for version := 1; version <= qrMaxVersion; version++ {
		spec := qrVersionsLevelM[version]
		if spec.dataCodewords()*8 >= 4+qrCharCountBits(version)+length*8 {
			return version, spec, nil
		}
	}
	return 0, qrVersionSpec{}, fmt.Errorf("qr: %d bytes exceed the version-%d capacity of this encoder", length, qrMaxVersion)
}

// qrCharCountBits is the byte-mode character-count field width: 8 bits up to
// version 9, 16 from version 10.
func qrCharCountBits(version int) int {
	if version < 10 {
		return 8
	}
	return 16
}

// qrCodewords assembles the interleaved codeword stream: the data segment
// padded to capacity, split into RS blocks with their EC codewords, interleaved
// as the standard requires.
func qrCodewords(version int, spec qrVersionSpec, data []byte) []byte {
	bits := &qrBitBuffer{}
	bits.append(qrByteMode, 4)
	bits.append(len(data), qrCharCountBits(version))
	for _, b := range data {
		bits.append(int(b), 8)
	}
	capacityBits := spec.dataCodewords() * 8
	// Terminator: up to four zero bits, then pad to a whole codeword.
	for i := 0; i < 4 && bits.length < capacityBits; i++ {
		bits.append(0, 1)
	}
	for bits.length%8 != 0 {
		bits.append(0, 1)
	}
	// Alternating pad codewords.
	for pad := 0xec; bits.length < capacityBits; pad ^= 0xec ^ 0x11 {
		bits.append(pad, 8)
	}

	blocks := make([][]byte, 0, spec.blocks())
	ecBlocks := make([][]byte, 0, spec.blocks())
	offset := 0
	appendBlock := func(size int) {
		block := bits.bytes[offset : offset+size]
		offset += size
		blocks = append(blocks, block)
		ecBlocks = append(ecBlocks, qrErrorCorrection(block, spec.ecPerBlock))
	}
	for i := 0; i < spec.group1Blocks; i++ {
		appendBlock(spec.group1Data)
	}
	for i := 0; i < spec.group2Blocks; i++ {
		appendBlock(spec.group2Data)
	}

	longest := spec.group1Data
	if spec.group2Data > longest {
		longest = spec.group2Data
	}
	out := make([]byte, 0, spec.dataCodewords()+spec.blocks()*spec.ecPerBlock)
	for i := 0; i < longest; i++ {
		for _, block := range blocks {
			if i < len(block) {
				out = append(out, block[i])
			}
		}
	}
	for i := 0; i < spec.ecPerBlock; i++ {
		for _, block := range ecBlocks {
			out = append(out, block[i])
		}
	}
	return out
}

// qrBitBuffer accumulates a big-endian bit stream into whole bytes.
type qrBitBuffer struct {
	bytes  []byte
	length int
}

func (b *qrBitBuffer) append(value, width int) {
	for i := width - 1; i >= 0; i-- {
		if b.length%8 == 0 {
			b.bytes = append(b.bytes, 0)
		}
		if value>>uint(i)&1 == 1 {
			b.bytes[b.length/8] |= 1 << uint(7-b.length%8)
		}
		b.length++
	}
}

func (q *qrSymbol) drawFunctionPatterns(version int, spec qrVersionSpec) {
	// Timing patterns run full width and height; finders overwrite their ends.
	for i := 0; i < q.size; i++ {
		q.setFunction(6, i, i%2 == 0)
		q.setFunction(i, 6, i%2 == 0)
	}
	q.drawFinder(3, 3)
	q.drawFinder(3, q.size-4)
	q.drawFinder(q.size-4, 3)

	centers := spec.alignment
	for i, row := range centers {
		for j, col := range centers {
			// Skip the three finder-occupied corners.
			last := len(centers) - 1
			if (i == 0 && j == 0) || (i == 0 && j == last) || (i == last && j == 0) {
				continue
			}
			q.drawAlignment(row, col)
		}
	}
	// Reserve the format area; the real bits are written once the mask is chosen.
	q.drawFormatBits(0)
	q.drawVersionBits(version)
}

// drawFinder draws one 7×7 finder pattern centred at row, col with its light
// separator (hence the -4..4 loop).
func (q *qrSymbol) drawFinder(row, col int) {
	for dr := -4; dr <= 4; dr++ {
		for dc := -4; dc <= 4; dc++ {
			distance := max(abs(dr), abs(dc))
			q.setFunction(row+dr, col+dc, distance != 2 && distance != 4)
		}
	}
}

func (q *qrSymbol) drawAlignment(row, col int) {
	for dr := -2; dr <= 2; dr++ {
		for dc := -2; dc <= 2; dc++ {
			q.setFunction(row+dr, col+dc, max(abs(dr), abs(dc)) != 1)
		}
	}
}

// drawFormatBits writes both copies of the 15-bit format info (EC level + mask,
// BCH(15,5)-protected).
func (q *qrSymbol) drawFormatBits(mask int) {
	data := qrECLevelM<<3 | mask
	remainder := data
	for i := 0; i < 10; i++ {
		remainder = (remainder << 1) ^ ((remainder >> 9) * 0x537)
	}
	bits := (data<<10 | remainder) ^ 0x5412

	bit := func(i int) bool { return bits>>uint(i)&1 == 1 }
	for i := 0; i <= 5; i++ {
		q.setFunction(i, 8, bit(i))
	}
	q.setFunction(7, 8, bit(6))
	q.setFunction(8, 8, bit(7))
	q.setFunction(8, 7, bit(8))
	for i := 9; i < 15; i++ {
		q.setFunction(8, 14-i, bit(i))
	}
	for i := 0; i < 8; i++ {
		q.setFunction(8, q.size-1-i, bit(i))
	}
	for i := 8; i < 15; i++ {
		q.setFunction(q.size-15+i, 8, bit(i))
	}
	// Always-dark module below the bottom-left finder.
	q.setFunction(q.size-8, 8, true)
}

// drawVersionBits writes the 18-bit version info (BCH(18,6)), present only from
// version 7.
func (q *qrSymbol) drawVersionBits(version int) {
	if version < 7 {
		return
	}
	remainder := version
	for i := 0; i < 12; i++ {
		remainder = (remainder << 1) ^ ((remainder >> 11) * 0x1f25)
	}
	bits := version<<12 | remainder
	for i := 0; i < 18; i++ {
		dark := bits>>uint(i)&1 == 1
		a, b := q.size-11+i%3, i/3
		q.setFunction(b, a, dark)
		q.setFunction(a, b, dark)
	}
}

// drawCodewords lays the codeword stream into the symbol in the two-module-wide
// zigzag from the bottom-right corner.
func (q *qrSymbol) drawCodewords(codewords []byte, remainderBits int) {
	total := len(codewords)*8 + remainderBits
	index := 0
	for right := q.size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // the vertical timing pattern is not a data column
		}
		for vertical := 0; vertical < q.size; vertical++ {
			for j := 0; j < 2; j++ {
				col := right - j
				upward := (right+1)&2 == 0
				row := vertical
				if upward {
					row = q.size - 1 - vertical
				}
				if q.isFunction(row, col) || index >= total {
					continue
				}
				dark := false
				if byteIndex := index / 8; byteIndex < len(codewords) {
					dark = codewords[byteIndex]>>uint(7-index%8)&1 == 1
				}
				q.set(row, col, dark)
				index++
			}
		}
	}
}

// applyMask XORs one of the eight mask patterns over the data modules. It is an
// involution: calling it twice restores the symbol.
func (q *qrSymbol) applyMask(mask int) {
	for row := 0; row < q.size; row++ {
		for col := 0; col < q.size; col++ {
			if q.isFunction(row, col) {
				continue
			}
			var invert bool
			switch mask {
			case 0:
				invert = (row+col)%2 == 0
			case 1:
				invert = row%2 == 0
			case 2:
				invert = col%3 == 0
			case 3:
				invert = (row+col)%3 == 0
			case 4:
				invert = (row/2+col/3)%2 == 0
			case 5:
				invert = row*col%2+row*col%3 == 0
			case 6:
				invert = (row*col%2+row*col%3)%2 == 0
			case 7:
				invert = ((row+col)%2+row*col%3)%2 == 0
			}
			if invert {
				q.set(row, col, !q.at(row, col))
			}
		}
	}
}

// penalty scores a masked symbol by the four standard rules; the lowest score
// reads most reliably.
func (q *qrSymbol) penalty() int {
	const (
		n1, n2, n3, n4 = 3, 3, 40, 10
	)
	score := 0

	// Rule 1: runs of five or more same-coloured modules in a row or column.
	runScore := func(runLength int) int {
		if runLength >= 5 {
			return n1 + runLength - 5
		}
		return 0
	}
	for i := 0; i < q.size; i++ {
		rowRun, colRun := 1, 1
		for j := 1; j < q.size; j++ {
			if q.at(i, j) == q.at(i, j-1) {
				rowRun++
			} else {
				score += runScore(rowRun)
				rowRun = 1
			}
			if q.at(j, i) == q.at(j-1, i) {
				colRun++
			} else {
				score += runScore(colRun)
				colRun = 1
			}
		}
		score += runScore(rowRun) + runScore(colRun)
	}

	// Rule 2: every 2×2 block of one colour.
	for row := 0; row+1 < q.size; row++ {
		for col := 0; col+1 < q.size; col++ {
			value := q.at(row, col)
			if value == q.at(row, col+1) && value == q.at(row+1, col) && value == q.at(row+1, col+1) {
				score += n2
			}
		}
	}

	// Rule 3: the 1:1:3:1:1 finder-like pattern with four light modules on
	// either side, in any row or column.
	forward := []bool{true, false, true, true, true, false, true, false, false, false, false}
	backward := []bool{false, false, false, false, true, false, true, true, true, false, true}
	matches := func(get func(int) bool, start int, want []bool) bool {
		for offset, value := range want {
			if get(start+offset) != value {
				return false
			}
		}
		return true
	}
	for i := 0; i < q.size; i++ {
		rowAt := func(j int) bool { return q.at(i, j) }
		colAt := func(j int) bool { return q.at(j, i) }
		for start := 0; start+len(forward) <= q.size; start++ {
			for _, want := range [][]bool{forward, backward} {
				if matches(rowAt, start, want) {
					score += n3
				}
				if matches(colAt, start, want) {
					score += n3
				}
			}
		}
	}

	// Rule 4: deviation of the dark-module proportion from 50%.
	dark := 0
	for _, module := range q.modules {
		if module {
			dark++
		}
	}
	total := q.size * q.size
	percent := dark * 100 / total
	deviation := abs(percent-50) / 5
	score += deviation * n4
	return score
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
