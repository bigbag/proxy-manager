package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"time"
)

type activityReader struct {
	io.Reader
	timer   *time.Timer
	timeout time.Duration
}

func (r activityReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

type writerOnly struct{ io.Writer }

type copyResult struct {
	bytes  int64
	failed bool
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

func tunnel(client, upstream net.Conn, clientReader, upstreamReader *bufio.Reader, bufferSize int, idleTimeout time.Duration) (int64, bool) {
	var expired atomic.Bool
	timer := time.AfterFunc(idleTimeout, func() {
		expired.Store(true)
		_ = client.Close()
		_ = upstream.Close()
	})
	defer timer.Stop()
	results := make(chan copyResult, 1)
	go func() {
		n, err := io.CopyBuffer(writerOnly{upstream}, activityReader{clientReader, timer, idleTimeout}, make([]byte, bufferSize))
		failed := !expired.Load() && upstreamError(err, "write")
		closeWrite(upstream)
		results <- copyResult{n, failed}
	}()
	n, err := io.CopyBuffer(writerOnly{client}, activityReader{upstreamReader, timer, idleTimeout}, make([]byte, bufferSize))
	failed := !expired.Load() && upstreamError(err, "read")
	closeWrite(client)
	sent := <-results
	return n + sent.bytes, failed || sent.failed
}
