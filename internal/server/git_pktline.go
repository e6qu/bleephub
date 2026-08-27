package bleephub

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// gitPktKind distinguishes the four pkt-line shapes. go-git's pktline.Scanner
// collapses flush-pkt and rejects delim/response-end as invalid lengths, and
// protocol v2 needs delim-pkt, so this reader decodes the framing itself.
type gitPktKind int

const (
	gitPktData        gitPktKind = iota // ordinary pkt-line carrying a payload
	gitPktFlush                         // "0000": ends a section or request
	gitPktDelim                         // "0001": separates protocol v2 sections
	gitPktResponseEnd                   // "0002": ends a multiplexed stateless response
)

// gitPktLineMax is the largest length prefix git writes: four bytes over the
// nominal 65516-byte payload, because git rounds side-band-64k packets up to it.
const gitPktLineMax = 65520

// gitPktDelimLine is written directly because go-git's encoder has no delim-pkt.
var gitPktDelimLine = []byte("0001")

type gitPktReader struct {
	in     *bufio.Reader
	header [4]byte
	buf    []byte
}

func newGitPktReader(in *bufio.Reader) *gitPktReader {
	return &gitPktReader{in: in}
}

// next reads one pkt-line. The returned payload is valid until the next call,
// with git's trailing newline stripped. A stream ending on a line boundary
// reports io.EOF, which is how both transports learn the request is over.
func (r *gitPktReader) next() ([]byte, gitPktKind, error) {
	if _, err := io.ReadFull(r.in, r.header[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, gitPktData, errors.New("truncated pkt-line length")
		}
		return nil, gitPktData, err
	}
	// The prefix is four hex digits; parse at 16 bits for a lossless int conversion.
	length, err := strconv.ParseUint(string(r.header[:]), 16, 16)
	if err != nil {
		return nil, gitPktData, fmt.Errorf("invalid pkt-line length %q", r.header[:])
	}
	switch length {
	case 0:
		return nil, gitPktFlush, nil
	case 1:
		return nil, gitPktDelim, nil
	case 2:
		return nil, gitPktResponseEnd, nil
	}
	if length <= 4 || length > gitPktLineMax {
		return nil, gitPktData, fmt.Errorf("invalid pkt-line length %q", r.header[:])
	}
	size := int(length) - 4
	if cap(r.buf) < size {
		r.buf = make([]byte, size)
	}
	r.buf = r.buf[:size]
	if _, err := io.ReadFull(r.in, r.buf); err != nil {
		return nil, gitPktData, fmt.Errorf("truncated pkt-line payload: %w", err)
	}
	return bytes.TrimSuffix(r.buf, []byte("\n")), gitPktData, nil
}

// writeGitPktDelim writes a delim-pkt, the protocol v2 section separator.
func writeGitPktDelim(out io.Writer) error {
	_, err := out.Write(gitPktDelimLine)
	return err
}
