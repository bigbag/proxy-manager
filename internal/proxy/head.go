package proxy

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
)

const maxHead = 64 << 10

type limitedHead struct {
	source io.Reader
	left   int64
	done   bool
}

func (l *limitedHead) Read(p []byte) (int, error) {
	if !l.done {
		if l.left == 0 {
			return 0, errors.New("head too large")
		}
		if int64(len(p)) > l.left {
			p = p[:l.left]
		}
	}
	n, err := l.source.Read(p)
	if !l.done {
		l.left -= int64(n)
	}
	return n, err
}

func readHead(conn net.Conn) (*bufio.Reader, string, int, string, int) {
	limit := &limitedHead{source: conn, left: maxHead}
	reader := bufio.NewReader(limit)
	request, err := http.ReadRequest(reader)
	if err != nil {
		return nil, "", 0, "", http.StatusBadRequest
	}
	limit.done = true
	defer request.Body.Close()
	if request.Method != http.MethodConnect {
		return nil, "", 0, "", http.StatusMethodNotAllowed
	}
	host, portText, err := net.SplitHostPort(request.Host)
	if err != nil || host == "" {
		return nil, "", 0, "", http.StatusBadRequest
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, "", 0, "", http.StatusBadRequest
	}
	key := request.Header.Get("X-Proxy-Affinity")
	if len(key) > 256 {
		return nil, "", 0, "", http.StatusBadRequest
	}
	return reader, host, port, key, 0
}
