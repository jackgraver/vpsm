package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"vpsm-collector/internal/protocol"
	"vpsm-collector/internal/query"
)

// handleReports ingests a batch of samples from one agent.
func (s *Server) handleReports(w http.ResponseWriter, r *http.Request) {
	defer r.Body.Close()

	body, code, err := s.requestBody(w, r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, code, err.Error())
		return
	}
	defer body.Close()

	var report protocol.Report
	if err := json.NewDecoder(body).Decode(&report); err != nil {
		// A body over either limit surfaces here, since both are enforced
		// by the reader the decoder is pulling from.
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) || errors.Is(err, errBodyTooLarge) {
			s.fail(w, r, http.StatusRequestEntityTooLarge, protocol.CodeTooLarge,
				"report body exceeds limit; halve the batch and retry")
			return
		}
		s.fail(w, r, http.StatusBadRequest, protocol.CodeMalformed, "malformed JSON: "+err.Error())
		return
	}

	if err := report.Validate(); err != nil {
		s.fail(w, r, http.StatusBadRequest, protocol.CodeInvalid, err.Error())
		return
	}
	if err := s.checkClock(&report); err != nil {
		// Logged as well as returned: the collector's operator may notice a
		// skewed agent before the agent's operator does.
		s.log.WarnContext(r.Context(), "rejected report",
			slog.String("host", report.Host.ID),
			slog.String("code", string(protocol.CodeClockSkew)),
			slog.Any("err", err))
		s.fail(w, r, http.StatusBadRequest, protocol.CodeClockSkew, err.Error())
		return
	}

	resp, err := s.store.SaveReport(r.Context(), &report, s.now())
	if err != nil {
		// Storage failure is ours, not the agent's: 5xx tells it to keep
		// buffering and retry rather than discard the batch as rejected.
		s.log.ErrorContext(r.Context(), "save report",
			slog.String("host", report.Host.ID),
			slog.Int("samples", len(report.Samples)),
			slog.Any("err", err))
		s.fail(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not store report")
		return
	}

	s.log.InfoContext(r.Context(), "report stored",
		slog.String("host", report.Host.ID),
		slog.Int("accepted", resp.Accepted),
		slog.Int("duplicate", resp.Duplicate))
	s.write(w, r, http.StatusOK, resp)
}

// hostsBody wraps the overview so the response is a JSON object and can grow
// fields later without breaking clients that already parse it.
type hostsBody struct {
	Hosts []query.HostStatus `json:"hosts"`
}

// handleHosts returns every host with recent samples.
func (s *Server) handleHosts(w http.ResponseWriter, r *http.Request) {
	n, err := history(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, protocol.CodeInvalid, "history must be an integer")
		return
	}

	hosts, err := s.source.Overview(r.Context(), n)
	if err != nil {
		s.log.ErrorContext(r.Context(), "read overview", slog.Any("err", err))
		s.fail(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not read hosts")
		return
	}
	if hosts == nil {
		hosts = []query.HostStatus{}
	}
	s.write(w, r, http.StatusOK, hostsBody{Hosts: hosts})
}

// handleHost returns one host with recent samples.
func (s *Server) handleHost(w http.ResponseWriter, r *http.Request) {
	n, err := history(r)
	if err != nil {
		s.fail(w, r, http.StatusBadRequest, protocol.CodeInvalid, "history must be an integer")
		return
	}

	id := r.PathValue("id")
	host, err := s.source.Host(r.Context(), id, n)
	switch {
	case errors.Is(err, query.ErrNoHost):
		s.fail(w, r, http.StatusNotFound, protocol.CodeNotFound, "no such host")
		return
	case err != nil:
		s.log.ErrorContext(r.Context(), "read host", slog.String("host", id), slog.Any("err", err))
		s.fail(w, r, http.StatusInternalServerError, protocol.CodeInternal, "could not read host")
		return
	}
	s.write(w, r, http.StatusOK, host)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	s.write(w, r, http.StatusOK, map[string]string{"status": "ok", "protocol": protocol.Version})
}
