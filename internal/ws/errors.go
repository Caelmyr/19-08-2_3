package ws

import "errors"

// 定义错误
var (
	ErrSendBufferFull  = errors.New("send buffer full")
	ErrDocNotFound     = errors.New("document not found")
	ErrInvalidOp       = errors.New("invalid operation")
	ErrVersionMismatch = errors.New("version mismatch")

	ErrFollowSelf       = errors.New("cannot follow yourself")
	ErrFollowNotFound   = errors.New("follow target not found")
	ErrFollowChain      = errors.New("target is already following someone else")
	ErrConnectionClosed = errors.New("connection closed")
)
