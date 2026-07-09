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
	"context"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// S3 request phase labels used for breakdown histograms.
const (
	PhaseHTTPParse    = "http_parse"    // middleware entry → auth entry
	PhaseAuthCrypto   = "auth_crypto"   // auth middleware duration
	PhaseHandlerLogic = "handler_logic" // handler minus I/O wait
	PhaseIOWait       = "io_wait"       // accumulated Read/Write syscall time
)

// breakdownCtxKey is the unexported context key for *BreakdownTiming.
type breakdownCtxKey struct{}

// BreakdownTiming carries per-request phase timestamps and accumulated I/O
// wait (in nanoseconds). It is attached to the request context by the
// breakdown-timing middleware and consumed by the auth middleware and the
// I/O wrappers.
type BreakdownTiming struct {
	T0          time.Time // outermost middleware entry
	T05         time.Time // after addCustomHeaders, before httpTracer
	T1          time.Time // auth middleware entry
	T2          time.Time // auth complete / handler entry
	T3          time.Time // handler complete
	Operation   string    // S3 operation name set by httpTrace
	AuthTotal   int64     // accumulated internal SigV4 time (ns, atomic)
	IOWaitTotal int64     // accumulated I/O wait (ns, atomic)
}

// getBreakdown extracts *BreakdownTiming from the request context, or nil.
func getBreakdown(r *http.Request) *BreakdownTiming {
	if bt, ok := r.Context().Value(breakdownCtxKey{}).(*BreakdownTiming); ok {
		return bt
	}
	return nil
}

var (
	breakdownDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "minio_s3_breakdown_duration_seconds",
			Help:    "S3 request phase breakdown duration.",
			Buckets: prometheus.ExponentialBuckets(0.00001, 2, 20), // 10 µs .. ~5 s
		},
		[]string{"phase", "operation", "method"},
	)
)

// breakdownTimingMiddleware is the outermost middleware. It creates a
// BreakdownTiming, wraps the ResponseWriter and Request Body for I/O
// accounting, and records Prometheus histograms after the handler returns.
func breakdownTimingMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bt := &BreakdownTiming{T0: time.Now()}
		ctx := context.WithValue(r.Context(), breakdownCtxKey{}, bt)
		r = r.WithContext(ctx)

		tw := &timingResponseWriter{ResponseWriter: w, bt: bt}
		r.Body = &timingReadCloser{ReadCloser: r.Body, bt: bt}

		h.ServeHTTP(tw, r)
		bt.T3 = time.Now()

		op := bt.Operation
		if op == "" {
			op = "unknown"
		}
		method := r.Method

		breakdownDuration.WithLabelValues(PhaseHTTPParse, op, method).
			Observe(bt.T05.Sub(bt.T0).Seconds())
		breakdownDuration.WithLabelValues(PhaseAuthCrypto, op, method).
			Observe(bt.T2.Sub(bt.T1).Seconds() + time.Duration(atomic.LoadInt64(&bt.AuthTotal)).Seconds())
		breakdownDuration.WithLabelValues(PhaseHandlerLogic, op, method).
			Observe(bt.T3.Sub(bt.T2).Seconds() -
				time.Duration(atomic.LoadInt64(&bt.IOWaitTotal)+atomic.LoadInt64(&bt.AuthTotal)).Seconds())
		breakdownDuration.WithLabelValues(PhaseIOWait, op, method).
			Observe(time.Duration(atomic.LoadInt64(&bt.IOWaitTotal)).Seconds())
	})
}
