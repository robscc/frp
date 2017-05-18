package conn

import (
	"time"
)

type ListenerInterface interface {
	Accept() (ConnInterface, error)
	Close() error
}

type ConnInterface interface {
	Loop()
	Read(p []byte) (n int, err error)
	ReadLine() (buff string, err error)
	Write(content []byte) (n int, err error)
	WriteString(content string) (err error)
	AppendReaderBuffer(content []byte)
	SetDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error

	Close() error
	IsClosed() bool
	CheckClosed() bool
}
