package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"
	"time"
)

const (
	probeTimeout     = 10 * time.Second
	echoMessageLimit = 4096
)

var errEchoTooLarge = errors.New("echo 消息超过 4KiB")
var echoAdmission = echoLimiter{peers: make(map[string]int)}

type probeStream interface {
	io.ReadWriteCloser
	CloseWrite() error
	Reset() error
}

type echoLimiter struct {
	mu     sync.Mutex
	active int
	peers  map[string]int
}

func (l *echoLimiter) acquire(peer string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active >= 16 || l.peers[peer] >= 2 {
		return false
	}
	if l.peers == nil {
		l.peers = make(map[string]int)
	}
	l.active++
	l.peers[peer]++
	return true
}

func (l *echoLimiter) release(peer string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.active--
	l.peers[peer]--
	if l.peers[peer] == 0 {
		delete(l.peers, peer)
	}
}

// 注销时等待已启动的取消回调，避免正常结束后残留回调误 Reset。
func resetOnCancel(ctx context.Context, s probeStream) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); _ = s.Reset() })
	return func() {
		if !stop() {
			<-done
		}
	}
}

func finishProbeStream(ctx context.Context, s probeStream, stop func(), err *error) {
	stop()
	if ctx.Err() != nil {
		*err = ctx.Err()
	}
	if *err != nil {
		_ = s.Reset()
	} else {
		_ = s.Close()
	}
}

func probeExchange(ctx context.Context, s probeStream, payload []byte) (err error) {
	defer finishProbeStream(ctx, s, resetOnCancel(ctx, s), &err)
	if err = ctx.Err(); err != nil {
		return err
	}
	if len(payload) > echoMessageLimit {
		return errEchoTooLarge
	}
	if err = writeEcho(s, payload); err != nil {
		return err
	}
	if err = s.CloseWrite(); err != nil {
		return err
	}
	buf := make([]byte, len(payload))
	n, err := readAll(s, buf)
	if err != nil {
		return err
	}
	if !bytes.Equal(buf[:n], payload) {
		return errors.New("回显内容不匹配")
	}
	return nil
}

// 保留流式回显兼容性，但期限、配额与总消息预算仅作用于 echo 协议。
func serveEcho(ctx context.Context, s probeStream, peer string, limiter *echoLimiter) (err error) {
	if !limiter.acquire(peer) {
		_ = s.Reset()
		return errors.New("echo 并发配额已满")
	}
	defer limiter.release(peer)
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	defer finishProbeStream(ctx, s, resetOnCancel(ctx, s), &err)
	buf := make([]byte, echoMessageLimit+1)
	total := 0
	for {
		if err = ctx.Err(); err != nil {
			return err
		}
		n, readErr := s.Read(buf[:echoMessageLimit-total+1])
		if n < 0 || n > echoMessageLimit-total+1 {
			return errors.New("echo reader 返回无效长度")
		}
		total += n
		if total > echoMessageLimit {
			return errEchoTooLarge
		}
		if n > 0 {
			if err = writeEcho(s, buf[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return s.CloseWrite()
		}
		if readErr != nil {
			return readErr
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func writeEcho(w io.Writer, b []byte) error {
	if len(b) == 0 {
		return nil
	}
	n, err := w.Write(b)
	if err != nil {
		return err
	}
	if n != len(b) {
		return io.ErrShortWrite
	}
	return nil
}

// 必须确认 EOF；满缓冲后再读一个字节区分恰好满与超限，不吞非 EOF 错误。
func readAll(r io.Reader, buf []byte) (int, error) {
	total := 0
	var extra [1]byte
	for {
		dst := buf[total:]
		if len(dst) == 0 {
			dst = extra[:]
		}
		n, err := r.Read(dst)
		if n < 0 || n > len(dst) {
			return total, errors.New("echo reader 返回无效长度")
		}
		if total == len(buf) && n > 0 {
			return total, errEchoTooLarge
		}
		total += n
		if err == io.EOF {
			return total, nil
		}
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrNoProgress
		}
	}
}
