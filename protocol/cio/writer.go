package cio

import (
	"bufio"
	"io"
)

type Writer struct {
	writer *bufio.Writer
}

func NewWriter(conn io.Writer) *Writer {
	return &Writer{
		writer: bufio.NewWriter(conn),
	}
}

// Write 将数据写入缓冲区。
func (bd *Writer) Write(data []byte) (int, error) {
	n, err := bd.writer.Write(data)
	if err != nil {
		return 0, err
	}
	err = bd.writer.Flush()
	if err != nil {
		return 0, err
	}
	return n, nil
}
