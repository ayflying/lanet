package lanet

import (
	"bytes"
	"io"
	"log"
	"sync"
)

const (
	consoleLogCapacity   = 2048
	consoleLogBytes      = 2 * 1024 * 1024
	consoleLogLineBytes  = 4096
	consoleLogBatchCount = 64
	consoleLogBatchBytes = 64 * 1024
)

type consoleLogRecord struct {
	ID   uint64 `json:"id"`
	Text string `json:"text"`
}

type consoleLogBatch struct {
	Records    []consoleLogRecord `json:"records"`
	Cursor     uint64             `json:"cursor"`
	Generation uint64             `json:"generation"`
	Reset      bool               `json:"reset"`
	More       bool               `json:"more"`
}

// consoleLogBuffer is a fixed-size ring. No per-reader queues retain log history.
type consoleLogBuffer struct {
	mu                sync.Mutex
	records           [consoleLogCapacity]consoleLogRecord
	head, count, size int
	next, generation  uint64
}

var processConsoleLogs = &consoleLogBuffer{generation: 1}
var captureConsoleLogsOnce sync.Once

// CaptureConsoleLogs tees the process-wide standard logger into bounded console
// memory, preserving its existing Writer (stderr, Docker and rotating files).
// Call after configuring log.SetOutput and before starting node/relay workers.
// Clearing this cache never touches the original writer or its files.
func CaptureConsoleLogs() {
	captureConsoleLogsOnce.Do(func() {
		log.SetOutput(consoleLogWriter{destination: log.Writer(), buffer: processConsoleLogs})
	})
}

type consoleLogWriter struct {
	destination io.Writer
	buffer      *consoleLogBuffer
}

func (w consoleLogWriter) Write(p []byte) (int, error) {
	w.buffer.write(p)
	return w.destination.Write(p)
}

func (b *consoleLogBuffer) write(p []byte) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		if end < 0 {
			end = len(p)
		}
		line := p[:end]
		if len(line) > consoleLogLineBytes {
			line = line[:consoleLogLineBytes]
		}
		text := string(line)
		if end > consoleLogLineBytes {
			text += " [truncated]"
		}
		for b.count > 0 && (b.count == consoleLogCapacity || b.size+len(text) > consoleLogBytes) {
			b.size -= len(b.records[b.head].Text)
			b.records[b.head] = consoleLogRecord{}
			b.head = (b.head + 1) % consoleLogCapacity
			b.count--
		}
		b.next++
		b.records[(b.head+b.count)%consoleLogCapacity] = consoleLogRecord{ID: b.next, Text: text}
		b.count++
		b.size += len(text)
		if end == len(p) {
			break
		}
		p = p[end+1:]
	}
}

func (b *consoleLogBuffer) clear() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.records = [consoleLogCapacity]consoleLogRecord{}
	b.head, b.count, b.size = 0, 0, 0
	b.generation++
}

func (b *consoleLogBuffer) batch(cursor, generation uint64) consoleLogBatch {
	b.mu.Lock()
	defer b.mu.Unlock()
	result := consoleLogBatch{Records: make([]consoleLogRecord, 0, consoleLogBatchCount), Cursor: cursor, Generation: b.generation}
	if generation != b.generation || cursor > b.next || (b.count > 0 && cursor < b.records[b.head].ID-1) {
		result.Reset = true
		result.Cursor = 0
		if b.count > 0 {
			result.Cursor = b.records[b.head].ID - 1
		} else {
			result.Cursor = b.next
		}
	}
	size := 0
	for i := 0; i < b.count; i++ {
		record := b.records[(b.head+i)%consoleLogCapacity]
		if record.ID <= result.Cursor {
			continue
		}
		if len(result.Records) == consoleLogBatchCount || size+len(record.Text) > consoleLogBatchBytes {
			result.More = true
			break
		}
		result.Records = append(result.Records, record)
		result.Cursor = record.ID
		size += len(record.Text)
	}
	return result
}
