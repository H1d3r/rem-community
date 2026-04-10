package core

import "sync"

const (
	ErrCmdParseFailed  = 1
	ErrArgsParseFailed = 2
	ErrPrepareFailed   = 3
	ErrNoConsoleURL    = 4
	ErrCreateConsole   = 5
	ErrDialFailed      = 6
	ErrWouldBlock      = 7 // Non-blocking read/write: no data available
)

var Conns sync.Map
