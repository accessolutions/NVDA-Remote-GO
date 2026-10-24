package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const httpRelayProtocolV2 = "nvdaremote-http/2"

func normalizeHTTPRelayV2Path(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/remote/http/v2"
	}
	return normalizeHTTPRelayPath(path)
}

func registerHTTPRelayV2Routes(mux *http.ServeMux, s *Server, path string) {
	base := normalizeHTTPRelayV2Path(path)
	mux.HandleFunc(base+"/sessions", s.handleHTTPCreateSessionV2)
	mux.HandleFunc(base+"/sessions/", s.handleHTTPSessionV2)
}

func (s *Server) handleHTTPCreateSessionV2(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeHTTPRelayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not allowed")
		return
	}
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeHTTPRelayError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	var request httpCreateSessionRequest
	if err := decodeHTTPJSON(w, r, &request, 4096); err != nil {
		if errors.Is(err, errHTTPBodyTooLarge) {
			writeHTTPRelayError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
			return
		}
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON")
		return
	}
	if request.Protocol != httpRelayProtocolV2 {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_protocol", "Unsupported HTTP relay protocol")
		return
	}
	if !httpRelayConnectionAllowed(w, r) {
		return
	}
	manager := s.httpRelay()
	session, token, err := manager.create(httpRemoteAddr(r.RemoteAddr))
	if err != nil {
		writeHTTPRelayError(w, http.StatusTooManyRequests, "session_limit", "HTTP relay session capacity is exhausted")
		return
	}
	client := s.newHTTPClient(newHTTPConn(session), session.remoteAddr)
	go client.listen()
	config := manager.config
	writeHTTPRelayJSON(w, http.StatusCreated, httpCreateSessionResponseV2{
		SessionID:              session.id,
		Token:                  token,
		Protocol:               httpRelayProtocolV2,
		PollWaitSeconds:        int(config.pollWait / time.Second),
		MaxBatchMessages:       config.maxBatchMessages,
		MaxMessageBytes:        config.maxMessageBytes,
		StreamKeepaliveSeconds: int(config.streamKeepalive / time.Second),
	})
}

type httpCreateSessionResponseV2 struct {
	SessionID              string `json:"sessionId"`
	Token                  string `json:"token"`
	Protocol               string `json:"protocol"`
	PollWaitSeconds        int    `json:"pollWaitSeconds"`
	MaxBatchMessages       int    `json:"maxBatchMessages"`
	MaxMessageBytes        int    `json:"maxMessageBytes"`
	StreamKeepaliveSeconds int    `json:"streamKeepaliveSeconds"`
}

func (s *Server) handleHTTPSessionV2(w http.ResponseWriter, r *http.Request) {
	base := normalizeHTTPRelayV2Path(s.httpRelayV2Path)
	prefix := base + "/sessions/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeHTTPRelayError(w, http.StatusNotFound, "not_found", "HTTP relay route was not found")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, prefix), "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete {
		s.handleHTTPSessionDelete(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "messages" {
		s.handleHTTPSessionMessages(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "stream" {
		s.handleHTTPSessionStreamRoute(w, r, parts[0])
		return
	}
	w.Header().Set("Allow", "DELETE, GET, POST")
	writeHTTPRelayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method or route is not allowed")
}

func (s *Server) handleHTTPSessionStreamRoute(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeHTTPRelayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not allowed")
		return
	}
	session := s.httpRelay().get(id)
	if session == nil {
		writeHTTPRelayError(w, http.StatusNotFound, "not_found", "HTTP relay session was not found")
		return
	}
	token, ok := requireBearerToken(r)
	if !ok || !tokenMatches(session, token) {
		writeHTTPRelayError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required")
		return
	}
	s.handleHTTPSessionStream(w, r, session)
}

func (s *Server) handleHTTPSessionStream(w http.ResponseWriter, r *http.Request, session *httpSession) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeHTTPRelayError(w, http.StatusInternalServerError, "streaming_unsupported", "Streaming is not supported")
		return
	}
	after, err := parseHTTPSequence(r.URL.Query().Get("after"))
	if err != nil {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_after", "The after sequence is invalid")
		return
	}
	if err := session.beginPoll(); err != nil {
		if errors.Is(err, errHTTPPollActive) {
			writeHTTPRelayError(w, http.StatusConflict, "poll_active", "Another reader is already active")
			return
		}
		writeHTTPRelayError(w, http.StatusNotFound, "not_found", "HTTP relay session was closed")
		return
	}
	defer session.endPoll()
	if err := session.acknowledgeOutbound(after); err != nil {
		session.mu.Lock()
		expectedSequence := session.nextOutboundSequence
		session.mu.Unlock()
		writeHTTPRelayJSON(w, http.StatusConflict, map[string]interface{}{
			"error":            "invalid_after",
			"message":          "The after sequence is ahead of the server",
			"expectedSequence": expectedSequence,
		})
		return
	}

	header := w.Header()
	header.Set("Content-Type", "text/event-stream; charset=utf-8")
	header.Set("Cache-Control", "no-store")
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, "event: ready\ndata: {}\n\n"); err != nil {
		return
	}
	flusher.Flush()

	config := session.config()
	keepalive := config.streamKeepalive
	if keepalive <= 0 {
		keepalive = 15 * time.Second
	}
	timer := time.NewTimer(keepalive)
	defer timer.Stop()

	for {
		messages, _, err := session.messagesAfter(after, config.maxBatchMessages)
		if err != nil {
			return
		}
		for _, message := range messages {
			if err := writeSSEMessage(w, message); err != nil {
				return
			}
			after = message.sequence
		}
		if len(messages) > 0 {
			flusher.Flush()
			// A flushed event has left the server, so the backlog up to that
			// sequence can be released without waiting for the client, which
			// only acknowledges every few seconds. Without this the backlog
			// would keep growing until enqueueOutbound rejects a write and the
			// relay tears the session down. The sequence always comes from a
			// queued message, so the acknowledgement cannot be ahead.
			_ = session.acknowledgeOutbound(after)
			session.touch()
			continue
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(keepalive)
		select {
		case <-session.notify:
		case <-timer.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
			session.touch()
		case <-r.Context().Done():
			return
		case <-session.ctx.Done():
			return
		}
	}
}

func writeSSEMessage(w io.Writer, message sequencedMessage) error {
	var compact bytes.Buffer
	if err := json.Compact(&compact, message.payload); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "id: %d\ndata: %s\n\n", message.sequence, compact.Bytes())
	return err
}
