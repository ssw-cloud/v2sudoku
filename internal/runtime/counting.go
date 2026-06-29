package runtime

import (
	"net"
	"sync"
	"time"
)

type trafficCounter struct {
	uid  int
	uuid string
	mu   sync.Mutex
	up   int64
	down int64
}

func (c *trafficCounter) addUpload(n int64) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	c.up += n
	c.mu.Unlock()
}

func (c *trafficCounter) addDownload(n int64) {
	if n <= 0 {
		return
	}
	c.mu.Lock()
	c.down += n
	c.mu.Unlock()
}

func (c *trafficCounter) snapshot() (int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.up, c.down
}

func (c *trafficCounter) subtract(up, down int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if up >= c.up {
		c.up = 0
	} else {
		c.up -= up
	}
	if down >= c.down {
		c.down = 0
	} else {
		c.down -= down
	}
}

type countingConn struct {
	net.Conn
	counter *trafficCounter
}

func (c *countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if n > 0 && c.counter != nil {
		c.counter.addUpload(int64(n))
	}
	return n, err
}

func (c *countingConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if n > 0 && c.counter != nil {
		c.counter.addDownload(int64(n))
	}
	return n, err
}

type onlineState struct {
	seenAt time.Time
	ip     string
}
