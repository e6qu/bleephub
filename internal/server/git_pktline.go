package bleephub

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// The pkt-line format carries three lines that have no payload at all:
// flush-pkt ("0000"), delim-pkt ("0001") and response-end-pkt ("0002").
// go-git's pktline.Scanner collapses flush-pkt into an empty payload and
// rejects the other two as invalid lengths, while protocol v2 separates a
// command's capability list from its arguments with a delim-pkt. The reader
// below therefore decodes the framing itself and reports which of the four
// line shapes it read.
type gitPktKind int

const (
	// gitPktData is an ordinary pkt-line carrying a payload.
	gitPktData gitPktKind = iota
	// gitPktFlush is "0000", which ends a section or a whole request.
	gitPktFlush
	// gitPktDelim is "0001", which separates the sections of a protocol v2
	// command request or response.
	gitPktDelim
	// gitPktResponseEnd is "0002", which ends a multiplexed stateless
	// response.
	gitPktResponseEnd
)

// gitPktLineMax is the largest length prefix git will write. It is four bytes
// more than the 65516-byte payload the format nominally allows, because
// canonical git rounds side-band-64k packets up to this bound.
const gitPktLineMax = 65520

// gitPktDelimLine is the on-the-wire spelling of a delim-pkt. go-git's encoder
// can write payloads and flush-pkts but has no delim-pkt, so it is written
// directly.
var gitPktDelimLine = []byte("0001")

// gitPktReader reads pkt-lines from a client.
type gitPktReader struct {
	in     *bufio.Reader
	header [4]byte
	buf    []byte
}

func newGitPktReader(in *bufio.Reader) *gitPktReader {
	return &gitPktReader{in: in}
}

// next reads one pkt-line. The payload it returns is valid until the following
// call and has had the trailing newline git puts on its text lines removed. A
// stream that ends exactly on a line boundary reports io.EOF, which is how both
// transports learn the request is over.
func (r *gitPktReader) next() ([]byte, gitPktKind, error) {
	if _, err := io.ReadFull(r.in, r.header[:]); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, gitPktData, errors.New("truncated pkt-line length")
		}
		return nil, gitPktData, err
	}
	// A length prefix is exactly four hex digits, so parsing at 16 bits makes
	// the bound the format guarantees explicit and the conversion to int
	// lossless on every platform.
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

// writeGitPktDelim writes a delim-pkt, the separator between two sections of a
// protocol v2 message.
func writeGitPktDelim(out io.Writer) error {
	_, err := out.Write(gitPktDelimLine)
	return err
}
