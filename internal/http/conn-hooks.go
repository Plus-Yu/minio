// Copyright (c) 2015-2025 MinIO, Inc.
//
// This file is part of MinIO Object Storage stack
//
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package http

import (
	"context"
	"crypto/tls"
	"net"
	"sync/atomic"
	"time"
)

// ConnTimingCtxKey is the context key used to store a *ConnTiming in the
// per-connection base context set via http.Server.ConnContext.
type ConnTimingCtxKey struct{}

// ConnTiming holds per-connection timing data (TCP accept time + raw socket
// Read/Write accumulators). One ConnTiming is created per accepted connection
// and shared across all requests on that connection.
type ConnTiming struct {
	AcceptTime   time.Time // when Accept() returned (after TLS if enabled)
	LastReadTime time.Time // wall clock when most recent conn Read completed
	readTotal  int64     // nanoseconds inside Read (atomic)
	writeTotal int64     // nanoseconds inside Write (atomic)
}

// ReadSnapshot returns the current accumulated socket read time.
func (ct *ConnTiming) ReadSnapshot() int64 { return atomic.LoadInt64(&ct.readTotal) }

// WriteSnapshot returns the current accumulated socket write time.
func (ct *ConnTiming) WriteSnapshot() int64 { return atomic.LoadInt64(&ct.writeTotal) }

// timingListener wraps net.Listener and emits ConnTiming-enabled connections.
type timingListener struct {
	net.Listener
}

func (l *timingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	ct := &ConnTiming{AcceptTime: time.Now()}
	return &connTimingWrapper{Conn: conn, ct: ct}, nil
}

// connTimingWrapper wraps net.Conn (*tls.Conn at this point) and accumulates
// Read/Write wall-clock time.
type connTimingWrapper struct {
	net.Conn
	ct *ConnTiming
}

func (w *connTimingWrapper) Read(b []byte) (int, error) {
	start := time.Now()
	n, err := w.Conn.Read(b)
	atomic.AddInt64(&w.ct.readTotal, int64(time.Since(start)))
	w.ct.LastReadTime = time.Now()
	return n, err
}

func (w *connTimingWrapper) Write(b []byte) (int, error) {
	start := time.Now()
	n, err := w.Conn.Write(b)
	atomic.AddInt64(&w.ct.writeTotal, int64(time.Since(start)))
	return n, err
}

// connTimingContext is an http.Server.ConnContext-compatible function.
func connTimingContext(ctx context.Context, c net.Conn) context.Context {
	// Unwrap *tls.Conn to reach the underlying connTimingWrapper.
	if tc, ok := c.(*tls.Conn); ok {
		c = tc.NetConn()
	}
	if tw, ok := c.(*connTimingWrapper); ok {
		return context.WithValue(ctx, ConnTimingCtxKey{}, tw.ct)
	}
	return ctx
}
