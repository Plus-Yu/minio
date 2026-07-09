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

package cmd

import (
	"io"
	"net/http"
	"sync/atomic"
	"time"
)

// timingReadCloser wraps an io.ReadCloser and accumulates the time spent
// inside Read calls into BreakdownTiming.IOWaitTotal.
type timingReadCloser struct {
	io.ReadCloser
	bt *BreakdownTiming
}

func (t *timingReadCloser) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := t.ReadCloser.Read(p)
	atomic.AddInt64(&t.bt.IOWaitTotal, int64(time.Since(start)))
	return n, err
}

// timingResponseWriter wraps an http.ResponseWriter and accumulates the
// time spent inside Write calls into BreakdownTiming.IOWaitTotal.
type timingResponseWriter struct {
	http.ResponseWriter
	bt *BreakdownTiming
}

func (t *timingResponseWriter) Write(p []byte) (int, error) {
	start := time.Now()
	n, err := t.ResponseWriter.Write(p)
	atomic.AddInt64(&t.bt.IOWaitTotal, int64(time.Since(start)))
	return n, err
}
