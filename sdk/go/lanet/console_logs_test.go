package lanet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConsoleLogBoundsAndBatches(t *testing.T) {
	b := &consoleLogBuffer{generation: 1}
	for i := 0; i < consoleLogCapacity+100; i++ {
		b.write([]byte("node relay\n"))
	}
	if b.count != consoleLogCapacity {
		t.Fatalf("count=%d", b.count)
	}
	batch := b.batch(0, 0)
	if !batch.Reset || !batch.More || len(batch.Records) != consoleLogBatchCount {
		t.Fatalf("batch=%+v", batch)
	}
	cursor := batch.Cursor
	next := b.batch(cursor, batch.Generation)
	if next.Reset || next.Records[0].ID != cursor+1 {
		t.Fatal("cursor discontinuity")
	}
	for i := 0; i < 1000; i++ {
		b.write([]byte(strings.Repeat("x", consoleLogLineBytes*2) + "\n"))
	}
	if b.size > consoleLogBytes || b.count > consoleLogCapacity {
		t.Fatal("unbounded cache")
	}
	batch = b.batch(0, 0)
	size := 0
	for _, record := range batch.Records {
		size += len(record.Text)
		if len(record.Text) > consoleLogLineBytes+len(" [truncated]") {
			t.Fatal("unbounded record")
		}
	}
	if size > consoleLogBatchBytes || len(batch.Records) > consoleLogBatchCount {
		t.Fatal("unbounded batch")
	}
}

func TestConsoleLogClearGenerationAndConcurrency(t *testing.T) {
	b := &consoleLogBuffer{generation: 1}
	b.write([]byte("before\n"))
	before := b.batch(0, 0)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 100; n++ {
				b.write([]byte("node\nrelay\n"))
				b.batch(0, 0)
			}
		}()
	}
	wg.Wait()
	b.clear()
	after := b.batch(before.Cursor, before.Generation)
	if !after.Reset || len(after.Records) != 0 || after.Cursor != b.next {
		t.Fatalf("clear=%+v", after)
	}
	b.write([]byte("after\n"))
	after = b.batch(after.Cursor, after.Generation)
	if after.Reset || len(after.Records) != 1 || after.Records[0].Text != "after" {
		t.Fatalf("after=%+v", after)
	}
}

func TestConsoleLogWriterPreservesDestinationAndCapturesRelay(t *testing.T) {
	var destination bytes.Buffer
	b := &consoleLogBuffer{generation: 1}
	logger := log.New(consoleLogWriter{destination: &destination, buffer: b}, "", 0)
	logger.Print("[node] started")
	logger.Print("[lanet-serverless] relay")
	batch := b.batch(0, 0)
	if len(batch.Records) != 2 || !strings.Contains(batch.Records[1].Text, "relay") {
		t.Fatalf("logs=%+v", batch)
	}
	original := destination.String()
	b.clear()
	if destination.String() != original {
		t.Fatal("clear touched destination")
	}
}

func TestConsoleLogRoutesAuthentication(t *testing.T) {
	c := &Client{sessionToken: "secret"}
	mux := http.NewServeMux()
	c.registerLogRoutes(mux)
	handler := c.authMiddleware(mux)
	for _, path := range []string{"/api/logs/stream", "/api/logs/clear"} {
		method := "GET"
		if strings.Contains(path, "clear") {
			method = "POST"
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s unauth status=%d", path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/logs/clear", nil)
	r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: "secret"})
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("authenticated clear=%d", w.Code)
	}
}

func TestConsoleLogSSEBatchAndCancellation(t *testing.T) {
	processConsoleLogs.clear()
	for i := 0; i < 150; i++ {
		processConsoleLogs.write([]byte("[lanet-serverless] relay\n"))
	}
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	c := &Client{rootCtx: root}
	finished := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { c.apiLogStream(w, r); finished <- struct{}{} }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL, nil)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatal("not SSE")
	}
	scanner := bufio.NewScanner(response.Body)
	batches := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var batch consoleLogBatch
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &batch); err != nil {
			t.Fatal(err)
		}
		if len(batch.Records) > consoleLogBatchCount {
			t.Fatal("oversized batch")
		}
		batches++
		if batches == 2 {
			break
		}
	}
	if batches != 2 {
		t.Fatalf("only %d batches: %v", batches, scanner.Err())
	}
	cancel()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("request cancellation leaked SSE handler")
	}
	// Client.Close's root cancellation must also terminate an idle stream.
	response, err = http.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	cancelRoot()
	_, _ = io.Copy(io.Discard, response.Body)
	response.Body.Close()
	select {
	case <-finished:
	case <-time.After(time.Second):
		t.Fatal("root cancellation leaked SSE handler")
	}
}

func TestConsoleLogStreamLimitsAndBadCursor(t *testing.T) {
	c := &Client{rootCtx: context.Background()}
	w := httptest.NewRecorder()
	c.apiLogStream(w, httptest.NewRequest("GET", "/api/logs/stream?cursor=invalid", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatal("invalid cursor accepted")
	}
	for i := 0; i < cap(consoleLogStreams); i++ {
		consoleLogStreams <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(consoleLogStreams); i++ {
			<-consoleLogStreams
		}
	}()
	w = httptest.NewRecorder()
	c.apiLogStream(w, httptest.NewRequest("GET", "/api/logs/stream", nil))
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("limit=%d", w.Code)
	}
}
