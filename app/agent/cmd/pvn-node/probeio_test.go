package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type pipeProbe struct {
	r      *io.PipeReader
	w      *io.PipeWriter
	resets atomic.Int32
}

func (s *pipeProbe) Read(b []byte) (int, error)  { return s.r.Read(b) }
func (s *pipeProbe) Write(b []byte) (int, error) { return s.w.Write(b) }
func (s *pipeProbe) CloseWrite() error           { return s.w.Close() }
func (s *pipeProbe) Close() error                { _ = s.w.Close(); return s.r.Close() }
func (s *pipeProbe) Reset() error                { s.resets.Add(1); return s.Close() }
func probePair() (*pipeProbe, *pipeProbe) {
	ar, bw := io.Pipe()
	br, aw := io.Pipe()
	return &pipeProbe{r: ar, w: aw}, &pipeProbe{r: br, w: bw}
}
func awaitProbe(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("流未及时释放")
		return nil
	}
}
func TestProbeBlockedCancellation(t *testing.T) {
	for _, phase := range []string{"write", "read", "noEOF"} {
		t.Run(phase, func(t *testing.T) {
			a, b := probePair()
			defer b.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			remote := make(chan error, 1)
			if phase != "write" {
				go func() {
					data, err := io.ReadAll(b)
					if err == nil && phase == "noEOF" {
						_, err = b.Write(data)
					}
					remote <- err
				}()
			}
			done := make(chan error, 1)
			go func() { done <- probeExchange(ctx, a, []byte("probe")) }()
			if err := awaitProbe(t, done); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err=%v", err)
			}
			if a.resets.Load() == 0 {
				t.Fatal("未 Reset")
			}
			if phase != "write" {
				awaitProbe(t, remote)
			}
		})
	}
}
func TestProbeEchoCompatibility(t *testing.T) {
	for _, size := range []int{0, 20, echoMessageLimit} {
		a, b := probePair()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		limiter := &echoLimiter{}
		done := make(chan error, 1)
		go func() { done <- serveEcho(ctx, b, "peer", limiter) }()
		if err := probeExchange(ctx, a, bytes.Repeat([]byte("x"), size)); err != nil {
			t.Fatal(err)
		}
		if err := awaitProbe(t, done); err != nil {
			t.Fatal(err)
		}
		cancel()
		if a.resets.Load() != 0 || b.resets.Load() != 0 {
			t.Fatal("正常完成后取消仍执行 Reset")
		}
		if limiter.active != 0 || len(limiter.peers) != 0 {
			t.Fatal("配额未释放")
		}
	}
}

type resultReader struct {
	n   int
	err error
}

func (r resultReader) Read(b []byte) (int, error) {
	if r.n > 0 {
		b[0] = 'a'
	}
	return r.n, r.err
}
func TestProbeReadAllBoundaries(t *testing.T) {
	fault := errors.New("底层错误")
	cases := []struct {
		name    string
		r       io.Reader
		size, n int
		err     error
	}{
		{"empty", strings.NewReader(""), 4, 0, nil},
		{"exact", strings.NewReader("abcd"), 4, 4, nil},
		{"oversize", strings.NewReader("abcde"), 4, 4, errEchoTooLarge},
		{"zero", resultReader{}, 4, 0, io.ErrNoProgress},
		{"errorWithData", resultReader{1, fault}, 4, 1, fault},
		{"EOFWithData", resultReader{1, io.EOF}, 4, 1, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n, err := readAll(c.r, make([]byte, c.size))
			if n != c.n || !errors.Is(err, c.err) {
				t.Fatalf("n=%d err=%v", n, err)
			}
		})
	}
}

type scriptedProbe struct {
	io.Reader
	bytes.Buffer
	resets, closes, halfCloses int
	writeErr, halfErr          error
	short                      bool
}

func (s *scriptedProbe) Read(b []byte) (int, error) { return s.Reader.Read(b) }
func (s *scriptedProbe) Write(b []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	if s.short {
		return 0, nil
	}
	return s.Buffer.Write(b)
}
func (s *scriptedProbe) Close() error      { s.closes++; return nil }
func (s *scriptedProbe) Reset() error      { s.resets++; return nil }
func (s *scriptedProbe) CloseWrite() error { s.halfCloses++; return s.halfErr }
func TestProbeValidationAndErrors(t *testing.T) {
	fault := errors.New("错误")
	cases := []struct {
		name, response    string
		writeErr, halfErr error
		short             bool
		want              error
	}{
		{name: "valid", response: "probe"},
		{name: "extra", response: "probe!", want: errEchoTooLarge},
		{name: "mismatch", response: "wrong", want: errors.New("不匹配")},
		{name: "short", response: "pro", want: errors.New("不匹配")},
		{name: "write", writeErr: fault, want: fault},
		{name: "halfClose", halfErr: fault, want: fault},
		{name: "shortWrite", short: true, want: io.ErrShortWrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := &scriptedProbe{Reader: strings.NewReader(c.response), writeErr: c.writeErr, halfErr: c.halfErr, short: c.short}
			err := probeExchange(context.Background(), s, []byte("probe"))
			if (err == nil) != (c.want == nil) {
				t.Fatalf("err=%v", err)
			}
			if c.want != nil && c.name != "mismatch" && c.name != "short" && !errors.Is(err, c.want) {
				t.Fatalf("err=%v", err)
			}
			if c.want != nil && s.resets != 1 {
				t.Fatal("异常未 Reset")
			}
		})
	}
}
func TestEchoLimitsAndRelease(t *testing.T) {
	limiter := &echoLimiter{}
	for _, reader := range []io.Reader{strings.NewReader(strings.Repeat("x", 4097)), resultReader{}} {
		s := &scriptedProbe{Reader: reader}
		if err := serveEcho(context.Background(), s, "peer", limiter); err == nil {
			t.Fatal("异常输入成功")
		}
		if s.resets != 1 || limiter.active != 0 || len(limiter.peers) != 0 {
			t.Fatal("异常未回收")
		}
	}
	if !limiter.acquire("same") || !limiter.acquire("same") || limiter.acquire("same") {
		t.Fatal("单 peer 配额错误")
	}
	limiter.release("same")
	limiter.release("same")
	for i := 0; i < 16; i++ {
		if !limiter.acquire(string(rune('a' + i))) {
			t.Fatal("全局过早拒绝")
		}
	}
	rejected := &scriptedProbe{Reader: strings.NewReader("")}
	if serveEcho(context.Background(), rejected, "other", limiter) == nil || rejected.resets != 1 {
		t.Fatal("全局超限未拒绝")
	}
	for i := 0; i < 16; i++ {
		limiter.release(string(rune('a' + i)))
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if limiter.acquire("race") {
					limiter.release("race")
				}
			}
		}()
	}
	wg.Wait()
	if limiter.active != 0 || len(limiter.peers) != 0 {
		t.Fatal("并发配额泄漏")
	}
}
func TestEchoBlockedCancellation(t *testing.T) {
	for _, write := range []bool{false, true} {
		a, b := probePair()
		defer a.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		limiter := &echoLimiter{}
		done := make(chan error, 1)
		go func() { done <- serveEcho(ctx, b, "peer", limiter) }()
		if write {
			if _, err := a.Write([]byte("x")); err != nil {
				t.Fatal(err)
			}
		}
		if err := awaitProbe(t, done); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err=%v", err)
		}
		cancel()
		if limiter.active != 0 || len(limiter.peers) != 0 || b.resets.Load() == 0 {
			t.Fatal("阻塞未回收")
		}
	}
}
