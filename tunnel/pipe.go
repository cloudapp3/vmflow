package tunnel

import (
	"io"
	"net"
	"sync"
	"time"
)

func pipePair(left net.Conn, right net.Conn, leftToRightSource io.Reader, onLeftToRight func(int64), onRightToLeft func(int64)) {
	var closeOnce sync.Once
	closeBoth := func() {
		closeOnce.Do(func() {
			_ = left.Close()
			_ = right.Close()
		})
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = copyAndCount(right, leftToRightSource, onLeftToRight)
		closeBoth()
	}()
	go func() {
		defer wg.Done()
		_, _ = copyAndCount(left, right, onRightToLeft)
		closeBoth()
	}()
	wg.Wait()
}

func copyAndCount(dst io.Writer, src io.Reader, onBytes func(int64)) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		nr, readErr := src.Read(buf)
		if nr > 0 {
			nw, writeErr := dst.Write(buf[:nr])
			if nw > 0 {
				total += int64(nw)
				if onBytes != nil {
					onBytes(int64(nw))
				}
			}
			if writeErr != nil {
				return total, writeErr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if readErr != nil {
			if readErr == io.EOF {
				return total, nil
			}
			return total, readErr
		}
	}
}

func setConnDeadline(conn net.Conn, timeout time.Duration) {
	if conn != nil && timeout > 0 {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}
}

func clearConnDeadline(conn net.Conn) {
	if conn != nil {
		_ = conn.SetDeadline(time.Time{})
	}
}
