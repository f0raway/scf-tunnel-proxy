package main

import (
	"errors"
	"github.com/gorilla/websocket"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const chunkSize = 16 * 1024
const maxTunnelBytes = 128 * 1024 * 1024
const maxTunnelSeconds = 7200
const idleSeconds = 120

type wsStream struct {
	c            *websocket.Conn
	reader       io.Reader
	writeMu      sync.Mutex
	nextWrite    time.Time
	lastActivity atomic.Int64
	transferred  atomic.Int64
	maxBytes     int64
	onClose      func()
	onProgress   func(int64) error
	closeOnce    sync.Once
}

func newStream(c *websocket.Conn) *wsStream {
	c.SetReadLimit(256 * 1024)
	s := &wsStream{c: c, maxBytes: maxTunnelBytes}
	s.lastActivity.Store(time.Now().UnixNano())
	return s
}
func (s *wsStream) account(n int) error {
	s.lastActivity.Store(time.Now().UnixNano())
	total := s.transferred.Add(int64(n))
	if s.onProgress != nil {
		if err := s.onProgress(total); err != nil {
			return err
		}
	}
	if total > s.maxBytes {
		return errors.New("connection traffic limit reached")
	}
	return nil
}
func (s *wsStream) Read(p []byte) (int, error) {
	for {
		if s.reader != nil {
			n, err := s.reader.Read(p)
			if n > 0 {
				if e := s.account(n); e != nil {
					return 0, e
				}
				return n, nil
			}
			if err != io.EOF {
				return n, err
			}
			s.reader = nil
		}
		kind, r, err := s.c.NextReader()
		if err != nil {
			return 0, err
		}
		if kind == websocket.TextMessage {
			b, e := io.ReadAll(io.LimitReader(r, 32))
			if e != nil {
				return 0, e
			}
			if string(b) == "EOF" {
				return 0, io.EOF
			}
			return 0, errors.New("unexpected tunnel control message")
		}
		if kind != websocket.BinaryMessage {
			return 0, errors.New("unexpected tunnel message")
		}
		s.reader = r
	}
}
func (s *wsStream) Write(p []byte) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > chunkSize {
			n = chunkSize
		}
		// Pace below SCF's default 10-frame/s and 128-KB/s ingress limits.
		if delay := time.Until(s.nextWrite); delay > 0 {
			time.Sleep(delay)
		}
		s.nextWrite = time.Now().Add(150 * time.Millisecond)
		if err := s.account(n); err != nil {
			return total, err
		}
		s.c.SetWriteDeadline(time.Now().Add(15 * time.Second))
		if err := s.c.WriteMessage(websocket.BinaryMessage, p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}
func (s *wsStream) CloseWrite() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.c.WriteMessage(websocket.TextMessage, []byte("EOF"))
}
func (s *wsStream) Close() error {
	err := s.c.Close()
	s.closeOnce.Do(func() {
		if s.onClose != nil {
			s.onClose()
		}
	})
	return err
}
func (s *wsStream) LocalAddr() net.Addr  { return s.c.LocalAddr() }
func (s *wsStream) RemoteAddr() net.Addr { return s.c.RemoteAddr() }
func (s *wsStream) SetDeadline(t time.Time) error {
	if err := s.c.SetReadDeadline(t); err != nil {
		return err
	}
	return s.c.SetWriteDeadline(t)
}
func (s *wsStream) SetReadDeadline(t time.Time) error  { return s.c.SetReadDeadline(t) }
func (s *wsStream) SetWriteDeadline(t time.Time) error { return s.c.SetWriteDeadline(t) }

// relay preserves TCP half-close and never reconnects an established session.
func relay(raw net.Conn, s *wsStream) {
	done := make(chan struct{})
	defer close(done)
	defer raw.Close()
	defer s.Close()
	go func() {
		timer := time.NewTimer(maxTunnelSeconds * time.Second)
		defer timer.Stop()
		tick := time.NewTicker(5 * time.Second)
		defer tick.Stop()
		for {
			select {
			case <-done:
				return
			case <-timer.C:
				raw.Close()
				s.Close()
				return
			case <-tick.C:
				if time.Since(time.Unix(0, s.lastActivity.Load())) > idleSeconds*time.Second {
					raw.Close()
					s.Close()
					return
				}
			}
		}
	}()
	finished := make(chan error, 2)
	copyOne := func(dst net.Conn, src net.Conn) {
		_, err := io.CopyBuffer(dst, src, make([]byte, chunkSize))
		if err == nil {
			if half, ok := dst.(interface{ CloseWrite() error }); ok {
				err = half.CloseWrite()
			}
		}
		if err != nil {
			raw.Close()
			s.Close()
		}
		finished <- err
	}
	go copyOne(s, raw)
	go copyOne(raw, s)
	<-finished
	<-finished
}
