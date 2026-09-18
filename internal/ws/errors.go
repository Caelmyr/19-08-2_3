package ws

import "errors"

// 定义错误
var (
	ErrSendBufferFull       = errors.New("send buffer full")
	ErrDocNotFound          = errors.New("document not found")
	ErrInvalidOp            = errors.New("invalid operation")
	ErrVersionMismatch      = errors.New("version mismatch")
	ErrCannotFollowSelf     = errors.New("cannot follow yourself")
	ErrFollowTargetNotFound = errors.New("follow target not found")
)
