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

	xhttp "github.com/minio/minio/internal/http"
	"github.com/prometheus/client_golang/prometheus"
)

// S3 request phase labels used for breakdown histograms.
const (
	PhaseHTTPParse    = "http_parse"    // conn Read → middleware entry (Go HTTP request parse)
	PhaseHTTPHeaders  = "http_headers"  // handler entry → WriteHeader call (S3 response headers)
	PhaseAuthCrypto   = "auth_crypto"   // auth middleware outer + inner SigV4
	PhaseHandlerLogic = "handler_logic" // handler minus I/O, auth, HTTP headers
	PhaseIOWait       = "io_wait"       // raw socket Read/Write wall-clock time
)

// breakdownCtxKey is the unexported context key for *BreakdownTiming.
type breakdownCtxKey struct{}

// BreakdownTiming carries per-request phase timestamps and accumulated I/O
// wait (in nanoseconds). It is attached to the request context by the
// breakdown-timing middleware.
type BreakdownTiming struct {
	TCPAccept   time.Time // conn Accept time (after TLS handshake if enabled)
	T0          time.Time // outermost middleware entry
	T05         time.Time // after addCustomHeaders, before httpTracer
	T1          time.Time // auth middleware entry
	T2          time.Time // auth complete / handler entry
	T3          time.Time // handler complete
	Operation   string    // S3 operation name set by httpTrace
	AuthTotal       int64     // accumulated internal SigV4 time (ns, atomic)
	IOWaitTotal     int64     // per-request socket Read/Write diff (ns)
	HTTPHeaderSent  time.Time // when WriteHeader() was called (HTTP status+headers written)
	readStart       int64     // ConnTiming.ReadSnapshot at T0
	writeStart      int64     // ConnTiming.WriteSnapshot at T0
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
			Buckets: prometheus.ExponentialBuckets(0.00001, 2, 20), // 10 us .. ~5 s
		},
		[]string{"phase", "operation", "method"},
	)
)

// breakdownTimingMiddleware is the outermost middleware.
func breakdownTimingMiddleware(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bt := &BreakdownTiming{T0: time.Now()}

		// Snapshot conn I/O and capture TLS/HTTP parse boundary.
		var ctLastRead time.Time
		if ct, ok := r.Context().Value(xhttp.ConnTimingCtxKey{}).(*xhttp.ConnTiming); ok {
			bt.TCPAccept = ct.AcceptTime
			bt.readStart = ct.ReadSnapshot()
			bt.writeStart = ct.WriteSnapshot()
			ctLastRead = ct.LastReadTime
		}

		ctx := context.WithValue(r.Context(), breakdownCtxKey{}, bt)
		r = r.WithContext(ctx)

		// Wrap ResponseWriter to capture WriteHeader (HTTP status+headers).
		tw := &headerTimingResponseWriter{ResponseWriter: w, bt: bt}

		h.ServeHTTP(tw, r)
		bt.T3 = time.Now()

		// Compute per-request conn-level I/O via diff from T0 snapshots.
		if ct, ok := r.Context().Value(xhttp.ConnTimingCtxKey{}).(*xhttp.ConnTiming); ok {
			bt.IOWaitTotal = (ct.ReadSnapshot() - bt.readStart) + (ct.WriteSnapshot() - bt.writeStart)
		}

		op := bt.Operation
		if op == "" {
			op = "unknown"
		}
		method := r.Method

		// http_parse: time since the last conn.Read() completed.
		// On a new connection the Read includes TLS handshake (1-5ms).
		// On keep-alive connections the Read delivers HTTP headers (~20-50us Go parse).
		if !ctLastRead.IsZero() {
			breakdownDuration.WithLabelValues(PhaseHTTPParse, op, method).
				Observe(bt.T0.Sub(ctLastRead).Seconds())
		} else {
			breakdownDuration.WithLabelValues(PhaseHTTPParse, op, method).
				Observe(bt.T05.Sub(bt.T0).Seconds())
		}

		// HTTP headers: handler entry → WriteHeader, minus internal auth.
		if !bt.HTTPHeaderSent.IsZero() {
			hdrDelta := bt.HTTPHeaderSent.Sub(bt.T2).Seconds() -
				time.Duration(atomic.LoadInt64(&bt.AuthTotal)).Seconds()
			if hdrDelta > 0 {
				breakdownDuration.WithLabelValues(PhaseHTTPHeaders, op, method).
					Observe(hdrDelta)
			}
		}

		breakdownDuration.WithLabelValues(PhaseAuthCrypto, op, method).
			Observe(bt.T2.Sub(bt.T1).Seconds() + time.Duration(atomic.LoadInt64(&bt.AuthTotal)).Seconds())
		// handler_logic: from WriteHeader to T3, minus I/O.
		handlerDur := bt.T3.Sub(bt.T2).Seconds() -
			time.Duration(bt.IOWaitTotal+atomic.LoadInt64(&bt.AuthTotal)).Seconds()
		if !bt.HTTPHeaderSent.IsZero() {
			handlerDur = bt.T3.Sub(bt.HTTPHeaderSent).Seconds() -
				time.Duration(bt.IOWaitTotal).Seconds()
		}
		breakdownDuration.WithLabelValues(PhaseHandlerLogic, op, method).
			Observe(handlerDur)
		breakdownDuration.WithLabelValues(PhaseIOWait, op, method).
			Observe(time.Duration(bt.IOWaitTotal).Seconds())
	})
}

// headerTimingResponseWriter intercepts WriteHeader to record the HTTP
// response status+headers write time, separating it from handler logic.
type headerTimingResponseWriter struct {
	http.ResponseWriter
	bt       *BreakdownTiming
	wroteHdr bool
}

func (w *headerTimingResponseWriter) WriteHeader(code int) {
	if !w.wroteHdr {
		w.bt.HTTPHeaderSent = time.Now()
		w.wroteHdr = true
	}
	w.ResponseWriter.WriteHeader(code)
}
