package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
)

type readerOnly struct{ io.Reader }
type writerOnly struct{ io.Writer }

type copyResult struct {
	bytes int64
	err   error
}

func closeWrite(conn net.Conn) {
	if c, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = c.CloseWrite()
	} else {
		_ = conn.Close()
	}
}

func upstreamError(err error, op string) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	return errors.As(err, &netErr) && netErr.Op == op
}

func tunnel(client, upstream net.Conn, clientReader, upstreamReader *bufio.Reader, bufferSize int) (int64, bool) {
	results := make(chan copyResult, 1)
	go func() {
		n, err := io.CopyBuffer(writerOnly{upstream}, readerOnly{clientReader}, make([]byte, bufferSize))
		closeWrite(upstream)
		results <- copyResult{n, err}
	}()
	n, err := io.CopyBuffer(writerOnly{client}, readerOnly{upstreamReader}, make([]byte, bufferSize))
	closeWrite(client)
	sent := <-results
	return n + sent.bytes, upstreamError(err, "read") || upstreamError(sent.err, "write")
}
