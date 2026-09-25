package proxy

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/bigbag/proxy-manager/internal/store"
)

func (s *Server) connectUpstream(ctx context.Context, p store.Proxy, host string, port int) (net.Conn, *bufio.Reader, *http.Response, error) {
	dial := s.Dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", net.JoinHostPort(p.Host, strconv.Itoa(p.Port)))
	if err != nil {
		return nil, nil, nil, err
	}
	deadline, _ := ctx.Deadline()
	if !deadline.IsZero() {
		if err := conn.SetDeadline(deadline); err != nil {
			_ = conn.Close()
			return nil, nil, nil, err
		}
	}
	target := net.JoinHostPort(host, strconv.Itoa(port))
	_, err = fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n", target, target)
	if err == nil && p.HasAuth() {
		credentials := base64.StdEncoding.EncodeToString([]byte(p.User + ":" + p.Password))
		_, err = fmt.Fprintf(conn, "Proxy-Authorization: Basic %s\r\n", credentials)
	}
	if err == nil {
		_, err = fmt.Fprint(conn, "\r\n")
	}
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	limit := &limitedHead{source: conn, left: maxHead}
	reader := bufio.NewReader(limit)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = conn.Close()
		return nil, nil, nil, err
	}
	limit.done = true
	if response.StatusCode < 200 || response.StatusCode > 599 {
		_ = conn.Close()
		return nil, nil, nil, errors.New("invalid upstream status")
	}
	return conn, reader, response, nil
}
