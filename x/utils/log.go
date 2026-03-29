package utils

import (
	"container/ring"
	"fmt"
	"github.com/chainreactors/logs"
	"strings"
	"sync"
)

var (
	DUMPLog logs.Level = 1
	IOLog   logs.Level = 2
	Log     *logs.Logger
)

type RingLogWriter struct {
	buffer *ring.Ring
	mu     sync.RWMutex
	size   int
	quiet  bool
}

func NewRingLogWriter(size int) *RingLogWriter {
	return &RingLogWriter{
		buffer: ring.New(size),
		size:   size,
	}
}

func (w *RingLogWriter) Write(p []byte) (n int, err error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := string(p)
	w.buffer.Value = s
	w.buffer = w.buffer.Next()
	fmt.Print(s)
	return len(p), nil
}

// 获取最近的日志
func (w *RingLogWriter) GetRecentLogs() []string {
	w.mu.RLock()
	defer w.mu.RUnlock()

	logs := make([]string, 0, w.size)
	w.buffer.Do(func(v interface{}) {
		if v != nil {
			logs = append(logs, v.(string))
		}
	})

	return logs
}

// 清空日志
func (w *RingLogWriter) Clear() {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.buffer = ring.New(w.size)
}

// 获取日志数量
func (w *RingLogWriter) Len() int {
	w.mu.RLock()
	defer w.mu.RUnlock()

	count := 0
	w.buffer.Do(func(v interface{}) {
		if v != nil {
			count++
		}
	})
	return count
}

// 导出日志为字符串
func (w *RingLogWriter) String() string {
	logs := w.GetRecentLogs()
	var result strings.Builder
	for _, entry := range logs {
		result.WriteString(entry)
	}
	return result.String()
}
