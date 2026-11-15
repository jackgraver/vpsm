package api

import (
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"vpsm-collector/internal/protocol"
)

// errBodyTooLarge is returned by cappedReader once a stream exceeds its
// budget. http.MaxBytesReader covers the wire bytes; this covers what a
// gzipped body expands into.
var errBodyTooLarge = errors.New("body exceeds limit")

// requestBody returns the report body, transparently decompressing it when
// the agent chose to gzip. Compression is optional on the agent side: this
// exists so that a long buffered flush, which is exactly when a report is
// biggest, does not have to cross the network uncompressed.
//
// On failure it returns the error code the caller should report, so the
// agent can tell a bad Content-Encoding from a corrupt body.
func (s *Server) requestBody(w http.ResponseWriter, r *http.Request) (io.ReadCloser, protocol.ErrorCode, error) {
	wire := http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	encoding := strings.TrimSpace(r.Header.Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return wire, "", nil
	}
	if !strings.EqualFold(encoding, "gzip") {
		return nil, protocol.CodeUnsupportedEncoding,
			fmt.Errorf("unsupported Content-Encoding %q, want gzip or none", encoding)
	}

	zr, err := gzip.NewReader(wire)
	if err != nil {
		return nil, protocol.CodeMalformed, fmt.Errorf("body is not valid gzip: %w", err)
	}
	// Capping the decompressed stream as well as the wire bytes is what
	// keeps a few kilobytes of crafted gzip from expanding into gigabytes.
	return &cappedReader{r: zr, closer: zr, remaining: MaxDecodedBytes}, "", nil
}

// cappedReader fails once more than remaining bytes have been read, rather
// than silently truncating the way io.LimitReader would — a truncated JSON
// body would surface as a confusing parse error instead of "too large".
type cappedReader struct {
	r         io.Reader
	closer    io.Closer
	remaining int64
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.remaining <= 0 {
		return 0, errBodyTooLarge
	}
	if int64(len(p)) > c.remaining {
		p = p[:c.remaining]
	}
	n, err := c.r.Read(p)
	c.remaining -= int64(n)
	return n, err
}

func (c *cappedReader) Close() error { return c.closer.Close() }

// checkClock rejects samples stamped implausibly far in the future.
//
// Timestamps come from the agent's own clock, and the collector trusts them
// for ordering — that is what makes buffered backfill land in the right
// place. A badly skewed clock abuses that trust: a sample dated next year
// would sit at the top of every "latest" query until it aged out, hiding
// everything real behind it. Past timestamps are left alone, since they are
// indistinguishable from legitimate backfill and retention sweeps them
// eventually.
//
// The rejection carries protocol.CodeClockSkew so the agent can log "the
// collector says my clock is wrong" — operator-fixable on the monitored
// box — rather than lumping it in with its own serialisation bugs.
func (s *Server) checkClock(r *protocol.Report) error {
	horizon := s.now().Add(s.maxSkew)
	for i := range r.Samples {
		if at := r.Samples[i].At; at.After(horizon) {
			return fmt.Errorf(
				"sample %d is stamped %s, more than %s ahead of the collector's clock; check the agent's time sync",
				i, at.UTC().Format(time.RFC3339), s.maxSkew)
		}
	}
	return nil
}
