package codex

import (
	"bufio"
	"errors"
	"io"
)

// line is one stdout line from the harness. Oversize lines carry no data: the
// bytes were discarded without being buffered.
type line struct {
	data []byte
	size int // full length of the line, including bytes that were discarded
	over bool
}

// lineReader splits a stream into lines of at most max bytes. A longer line is
// consumed to its newline without being held in memory and reported as over.
type lineReader struct {
	r   *bufio.Reader
	max int
}

func newLineReader(r io.Reader, max int) *lineReader {
	return &lineReader{r: bufio.NewReaderSize(r, 64<<10), max: max}
}

// next returns the next line. At EOF it returns io.EOF; a final line without a
// newline is still returned first.
func (l *lineReader) next() (line, error) {
	var buf []byte
	size := 0
	over := false
	for {
		chunk, err := l.r.ReadSlice('\n')
		size += len(chunk)
		if !over {
			if size > l.max+1 { // +1 for the newline
				over = true
				buf = nil
			} else {
				buf = append(buf, chunk...)
			}
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if size > 0 && errors.Is(err, io.EOF) {
				return trimLine(buf, size, over), nil
			}
			return line{}, err
		}
		return trimLine(buf, size, over), nil
	}
}

func trimLine(buf []byte, size int, over bool) line {
	if over {
		return line{size: size, over: true}
	}
	for len(buf) > 0 && (buf[len(buf)-1] == '\n' || buf[len(buf)-1] == '\r') {
		buf = buf[:len(buf)-1]
	}
	return line{data: buf, size: size}
}
