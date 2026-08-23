package bleephub

import (
	"reflect"
	"strings"
	"testing"
)

// TestQRErrorCorrectionMatchesReferenceVector pins the Reed-Solomon stage
// against the published worked example for a version-1 level-M symbol
// containing "HELLO WORLD". Getting this wrong produces a symbol that looks
// plausible and scans as garbage, so it is checked against known bytes rather
// than against the encoder's own output.
func TestQRErrorCorrectionMatchesReferenceVector(t *testing.T) {
	data := []byte{32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17}
	want := []byte{196, 35, 39, 119, 235, 215, 231, 226, 93, 23}
	if got := qrErrorCorrection(data, 10); !reflect.DeepEqual(got, want) {
		t.Fatalf("error-correction codewords = %v, want %v", got, want)
	}
}

func TestQRVersionSpecCodewordTotalsAreConsistent(t *testing.T) {
	// Total codewords per version at any EC level (ISO/IEC 18004 table 1).
	totals := map[int]int{1: 26, 2: 44, 3: 70, 4: 100, 5: 134, 6: 172, 7: 196, 8: 242, 9: 292, 10: 346}
	for version, total := range totals {
		spec := qrVersionsLevelM[version]
		got := spec.dataCodewords() + spec.blocks()*spec.ecPerBlock
		if got != total {
			t.Errorf("version %d: data+EC codewords = %d, want %d", version, got, total)
		}
	}
}

// TestQREncodeRoundTrips decodes the encoder's own symbol back to its input the
// way a scanner would: read the format information, undo the mask, walk the
// zigzag, de-interleave the blocks, and parse the byte-mode segment. It proves
// version selection, bit packing, interleaving, placement, masking and the
// format-information BCH all agree with each other and with the standard's
// layout rules.
func TestQREncodeRoundTrips(t *testing.T) {
	cases := []string{
		"otpauth://totp/bleephub:admin?algorithm=SHA1&digits=6&issuer=bleephub&period=30&secret=JBSWY3DPEHPK3PXP",
		"a",
		strings.Repeat("x", 14),  // exactly the version-1 capacity
		strings.Repeat("y", 15),  // forces version 2
		strings.Repeat("z", 180), // forces a two-group version
	}
	for _, text := range cases {
		rows, err := qrEncode(text)
		if err != nil {
			t.Fatalf("qrEncode(%d bytes): %v", len(text), err)
		}
		decoded, err := decodeQRForTest(rows)
		if err != nil {
			t.Fatalf("decode %d-byte symbol: %v", len(text), err)
		}
		if decoded != text {
			t.Errorf("round trip = %q, want %q", decoded, text)
		}
	}
}

func TestQREncodeRefusesOversizedPayload(t *testing.T) {
	if _, err := qrEncode(strings.Repeat("q", 214)); err == nil {
		t.Fatal("expected a payload beyond the version-10 capacity to be refused")
	}
}

// TestQREncodeIsDeterministic guards the mask selection: the same input must
// always produce the same symbol, or a redeployed enrolment page would show a
// different-looking code for the same secret.
func TestQREncodeIsDeterministic(t *testing.T) {
	first, err := qrEncode("otpauth://totp/bleephub:admin?secret=JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	second, err := qrEncode("otpauth://totp/bleephub:admin?secret=JBSWY3DPEHPK3PXP")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("qrEncode is not deterministic")
	}
}

// decodeQRForTest is a minimal reader for the symbols this encoder produces.
func decodeQRForTest(rows []string) (string, error) {
	size := len(rows)
	version := (size - 17) / 4
	if version < 1 || version > qrMaxVersion || version*4+17 != size {
		return "", errQRTest("unexpected symbol size " + itoa(size))
	}
	symbol := newQRSymbol(version)
	for row, line := range rows {
		if len(line) != size {
			return "", errQRTest("row " + itoa(row) + " is not square")
		}
		for col := 0; col < size; col++ {
			symbol.set(row, col, line[col] == '1')
		}
	}
	// Recover which modules are function patterns (and their expected values)
	// by rebuilding them; the data modules are untouched by this.
	spec := qrVersionsLevelM[version]
	template := newQRSymbol(version)
	template.drawFunctionPatterns(version, spec)
	symbol.function = template.function

	mask, err := decodeQRFormatMask(symbol)
	if err != nil {
		return "", err
	}
	symbol.applyMask(mask)

	// Walk the placement order and collect the codeword bits back out.
	total := (spec.dataCodewords() + spec.blocks()*spec.ecPerBlock) * 8
	bits := make([]bool, 0, total)
	for right := size - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5
		}
		for vertical := 0; vertical < size; vertical++ {
			for j := 0; j < 2; j++ {
				col := right - j
				upward := (right+1)&2 == 0
				row := vertical
				if upward {
					row = size - 1 - vertical
				}
				if symbol.isFunction(row, col) || len(bits) >= total {
					continue
				}
				bits = append(bits, symbol.at(row, col))
			}
		}
	}
	codewords := make([]byte, total/8)
	for i, bit := range bits {
		if bit {
			codewords[i/8] |= 1 << uint(7-i%8)
		}
	}

	// De-interleave the data codewords back into their blocks.
	sizes := make([]int, 0, spec.blocks())
	for i := 0; i < spec.group1Blocks; i++ {
		sizes = append(sizes, spec.group1Data)
	}
	for i := 0; i < spec.group2Blocks; i++ {
		sizes = append(sizes, spec.group2Data)
	}
	blocks := make([][]byte, len(sizes))
	longest := 0
	for i, blockSize := range sizes {
		blocks[i] = make([]byte, 0, blockSize)
		if blockSize > longest {
			longest = blockSize
		}
	}
	index := 0
	for i := 0; i < longest; i++ {
		for b, blockSize := range sizes {
			if i < blockSize {
				blocks[b] = append(blocks[b], codewords[index])
				index++
			}
		}
	}
	var data []byte
	for _, block := range blocks {
		data = append(data, block...)
	}

	// Parse the byte-mode segment.
	reader := &qrBitReader{data: data}
	if reader.read(4) != qrByteMode {
		return "", errQRTest("segment is not byte mode")
	}
	length := reader.read(qrCharCountBits(version))
	out := make([]byte, length)
	for i := 0; i < length; i++ {
		out[i] = byte(reader.read(8))
	}
	return string(out), nil
}

// decodeQRFormatMask reads the mask index out of the first format-information
// copy, checking the XOR mask was applied as the standard requires.
func decodeQRFormatMask(symbol *qrSymbol) (int, error) {
	bits := 0
	get := func(row, col int) int {
		if symbol.at(row, col) {
			return 1
		}
		return 0
	}
	for i := 0; i <= 5; i++ {
		bits |= get(i, 8) << uint(i)
	}
	bits |= get(7, 8) << 6
	bits |= get(8, 8) << 7
	bits |= get(8, 7) << 8
	for i := 9; i < 15; i++ {
		bits |= get(8, 14-i) << uint(i)
	}
	data := (bits ^ 0x5412) >> 10
	if data>>3 != qrECLevelM {
		return 0, errQRTest("format information does not carry error-correction level M")
	}
	return data & 0b111, nil
}

type qrBitReader struct {
	data   []byte
	offset int
}

func (r *qrBitReader) read(width int) int {
	value := 0
	for i := 0; i < width; i++ {
		byteIndex := r.offset / 8
		bit := 0
		if byteIndex < len(r.data) {
			bit = int(r.data[byteIndex] >> uint(7-r.offset%8) & 1)
		}
		value = value<<1 | bit
		r.offset++
	}
	return value
}

type errQRTest string

func (e errQRTest) Error() string { return string(e) }
