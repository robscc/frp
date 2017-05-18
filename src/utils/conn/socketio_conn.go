package conn

import (
	"bufio"
	"bytes"
	"io"
	"time"

	"sync"

	"fmt"

	"ubntgo/logger"

	"github.com/googollee/go-socket.io"
	"github.com/robscc/frp/src/utils/pool"
)

type SocketIOListener struct {
	accept chan *SocketIOConn
}

func SocketIOListen(bindAddr string, bindPort int64) (l *SocketIOListener, err error) {
	server, err := socketio.NewServer(nil)
	if err != nil {
		return l, err
	}

	l = &SocketIOListener{
		accept: make(chan *SocketIOConn),
	}

	server.On("connection", func(so socketio.Socket) {
		conn := NewSocketIOConn(so)
		conn.Loop()
		l.accept <- conn
	})

	return l, err
}

func (l *SocketIOListener) Accept() (*SocketIOConn, error) {
	conn, ok := <-l.accept
	if !ok {
		return conn, fmt.Errorf("channel close")
	}
	return conn, nil
}

func (l *SocketIOListener) Close() error {
	close(l.accept)
	return nil
}

type SocketIOConn struct {
	Conn        socketio.Socket
	Reader      *bufio.Reader
	buffer      *bytes.Buffer
	closeFlag   bool
	connTimeout int64

	closeChan chan bool

	mutex           sync.RWMutex
	timer           *time.Timer
	heartbeatTikcer *time.Ticker
}

func NewSocketIOConn(conn socketio.Socket) *SocketIOConn {
	c := &SocketIOConn{
		Conn:        conn,
		buffer:      nil,
		closeFlag:   false,
		connTimeout: 10,
		closeChan:   make(chan bool, 1),
	}
	return c
}

func (c *SocketIOConn) Loop() {
	defer c.destroy()

	c.timer = time.AfterFunc(time.Duration(c.connTimeout)*time.Second, func() {
		c.mutex.Lock()
		c.closeFlag = true
		c.mutex.Unlock()
		if !c.timer.Stop() {
			<-c.timer.C
		}
		c.timer = nil
		c.closeChan <- true
	})

	c.heartbeatTikcer = time.NewTicker(time.Duration(c.connTimeout/2) * time.Second)

	c.Conn.On("heartbeat", func(msg string) {
		c.mutex.RLock()
		if c.closeFlag {
			c.mutex.RUnlock()
			c.destroy()
			//@TODO send close chan to loop
		} else {
			c.mutex.RUnlock()
		}

		if c.timer == nil {
			logger.Debugf("timer should not be nil")
			//@TODO must have some errors here
			return
		}

		c.timer.Reset(time.Duration(c.connTimeout) * time.Second)
	})

	c.Conn.On("binary", func(stream []byte) {
		c.mutex.Lock()
		if c.closeFlag {
			c.mutex.Unlock()
			return
		}
		if c.buffer == nil {
			c.buffer = bytes.NewBuffer(make([]byte, 0, 2048))
		}
		c.buffer.Write(stream)
		c.mutex.Unlock()

		if c.timer == nil {
			logger.Debugf("timer should not be nil")
			//@TODO must have some errors here
			return
		}

		c.timer.Reset(time.Duration(c.connTimeout) * time.Second)
	})

	for {
		select {
		case <-c.closeChan:
			c.destroy()
			return
		case <-c.heartbeatTikcer.C:
			if c.closeFlag == true {
				go func(c *SocketIOConn) {
					c.closeChan <- true
				}(c)
			} else {
				c.Conn.Emit("heartbeat", "1")
			}
		}
	}

}

func (c *SocketIOConn) destroy() {
	c.mutex.Lock()
	c.buffer = nil
	c.Reader = nil
	//c.closeFlag = true
	if c.Conn != nil {
		//unregister heartbeat
		c.Conn.On("heartbeat", nil)
		c.Conn.On("binary", nil)
	}
	close(c.closeChan)
	c.mutex.Unlock()
}

func (c *SocketIOConn) Read(p []byte) (n int, err error) {
	c.mutex.RLock()
	if c.buffer == nil {
		c.mutex.RUnlock()
		return c.Reader.Read(p)
	}
	c.mutex.RUnlock()

	n, err = c.buffer.Read(p)
	if err == io.EOF {
		c.mutex.Lock()
		c.buffer = nil
		c.mutex.Unlock()
		var n2 int
		n2, err = c.Reader.Read(p[n:])
		n += n2
	}
	return
}

func (c *SocketIOConn) ReadLine() (buff string, err error) {
	buff, err = c.Reader.ReadString('\n')
	if err != nil {
		if err == io.EOF {
			c.mutex.Lock()
			c.closeFlag = true
			c.mutex.Unlock()
		}
	}
	return buff, err
}

func (c *SocketIOConn) Write(content []byte) (n int, err error) {
	err = c.Conn.Emit("binary", content)
	return len(content), err
}

func (c *SocketIOConn) WriteString(content string) (err error) {
	err = c.Conn.Emit("binary", []byte(content))
	return err
}

func (c *SocketIOConn) AppendReaderBuffer(content []byte) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if c.buffer == nil {
		c.buffer = bytes.NewBuffer(make([]byte, 0, 2048))
	}
	c.buffer.Write(content)
}

func (c *SocketIOConn) SetDeadline(t time.Time) error {
	return nil
}

func (c *SocketIOConn) SetReadDeadline(t time.Time) error {
	return nil
}

func (c *SocketIOConn) Close() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.closeFlag = true
	return nil
}

func (c *SocketIOConn) IsClosed() bool {
	c.mutex.RLock()
	defer c.mutex.RUnlock()
	closeFlag := c.closeFlag
	return closeFlag
}

func (c *SocketIOConn) CheckClosed() bool {
	c.mutex.RLock()
	if c.closeFlag {
		c.mutex.RUnlock()
		return true
	}
	c.mutex.RUnlock()

	tmp := pool.GetBuf(2048)
	defer pool.PutBuf(tmp)

	//set close flag to true for auto close
	go func(c *SocketIOConn) {
		time.Sleep(time.Millisecond)
		c.mutex.Lock()
		c.closeFlag = true
		c.mutex.Unlock()
	}(c)

	return false
}
