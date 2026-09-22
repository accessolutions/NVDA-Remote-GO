package server

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const httpRelayProtocol = "nvdaremote-http/1"

type httpCreateSessionRequest struct {
	Protocol string `json:"protocol"`
}

type httpCreateSessionResponse struct {
	SessionID        string `json:"sessionId"`
	Token            string `json:"token"`
	Protocol         string `json:"protocol"`
	PollWaitSeconds  int    `json:"pollWaitSeconds"`
	MaxBatchMessages int    `json:"maxBatchMessages"`
	MaxMessageBytes  int    `json:"maxMessageBytes"`
}

type httpInboundMessage struct {
	Sequence uint64          `json:"sequence"`
	Payload  json.RawMessage `json:"payload"`
}

type httpInboundRequest struct {
	Messages        []httpInboundMessage `json:"messages"`
	ReceivedThrough uint64               `json:"receivedThrough,omitempty"`
}

type httpOutboundMessage struct {
	Sequence uint64          `json:"sequence"`
	Payload  json.RawMessage `json:"payload"`
}

type httpPollResponse struct {
	Messages     []httpOutboundMessage `json:"messages"`
	LastSequence uint64                `json:"lastSequence"`
}

func normalizeHTTPRelayPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/remote/http/v1"
	}
	path = "/" + strings.TrimLeft(path, "/")
	if len(path) > 1 {
		path = strings.TrimRight(path, "/")
	}
	return path
}

func registerHTTPRelayRoutes(mux *http.ServeMux, s *Server, path string) {
	base := normalizeHTTPRelayPath(path)
	mux.HandleFunc(base+"/sessions", s.handleHTTPCreateSession)
	mux.HandleFunc(base+"/sessions/", s.handleHTTPSession)
}

func (s *Server) httpRelay() *httpRelayManager {
	s.Lock()
	defer s.Unlock()
	if s.httpRelayManager == nil {
		s.httpRelayManager = newHTTPRelayManager(defaultHTTPRelayConfig())
	}
	return s.httpRelayManager
}

func writeHTTPRelayJSON(w http.ResponseWriter, status int, value interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if status != http.StatusNoContent {
		_ = json.NewEncoder(w).Encode(value)
	}
}

func writeHTTPRelayError(w http.ResponseWriter, status int, code, message string) {
	writeHTTPRelayJSON(w, status, map[string]string{
		"error":   code,
		"message": message,
	})
}

func requireBearerToken(r *http.Request) (string, bool) {
	value := r.Header.Get("Authorization")
	if !strings.HasPrefix(value, "Bearer ") {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", false
	}
	return token, true
}

func tokenMatches(session *httpSession, token string) bool {
	tokenBytes, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(tokenBytes) != 32 {
		return false
	}
	hash := sha256.Sum256(tokenBytes)
	session.mu.Lock()
	stored := session.tokenHash
	session.mu.Unlock()
	return subtle.ConstantTimeCompare(stored[:], hash[:]) == 1
}

func (s *Server) handleHTTPCreateSession(w http.ResponseWriter, r *http.Request) {
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
	if request.Protocol != httpRelayProtocol {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_protocol", "Unsupported HTTP relay protocol")
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
	writeHTTPRelayJSON(w, http.StatusCreated, httpCreateSessionResponse{
		SessionID:        session.id,
		Token:            token,
		Protocol:         httpRelayProtocol,
		PollWaitSeconds:  int(config.pollWait / time.Second),
		MaxBatchMessages: config.maxBatchMessages,
		MaxMessageBytes:  config.maxMessageBytes,
	})
}

func (s *Server) handleHTTPSession(w http.ResponseWriter, r *http.Request) {
	base := normalizeHTTPRelayPath(s.httpRelayPath)
	prefix := base + "/sessions/"
	if !strings.HasPrefix(r.URL.Path, prefix) {
		writeHTTPRelayError(w, http.StatusNotFound, "not_found", "HTTP relay route was not found")
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, prefix)
	parts := strings.Split(rest, "/")
	if len(parts) == 1 && parts[0] != "" && r.Method == http.MethodDelete {
		s.handleHTTPSessionDelete(w, r, parts[0])
		return
	}
	if len(parts) == 2 && parts[0] != "" && parts[1] == "messages" {
		s.handleHTTPSessionMessages(w, r, parts[0])
		return
	}
	w.Header().Set("Allow", "DELETE, GET, POST")
	writeHTTPRelayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method or route is not allowed")
}

func (s *Server) handleHTTPSessionDelete(w http.ResponseWriter, r *http.Request, id string) {
	session := s.httpRelay().get(id)
	if session == nil {
		writeHTTPRelayJSON(w, http.StatusNoContent, nil)
		return
	}
	token, ok := requireBearerToken(r)
	if !ok || !tokenMatches(session, token) {
		writeHTTPRelayError(w, http.StatusUnauthorized, "unauthorized", "A valid bearer token is required")
		return
	}
	session.close()
	writeHTTPRelayJSON(w, http.StatusNoContent, nil)
}

func (s *Server) handleHTTPSessionMessages(w http.ResponseWriter, r *http.Request, id string) {
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
	switch r.Method {
	case http.MethodPost:
		s.handleHTTPSessionPost(w, r, session)
	case http.MethodGet:
		s.handleHTTPSessionPoll(w, r, session)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeHTTPRelayError(w, http.StatusMethodNotAllowed, "method_not_allowed", "Method is not allowed")
	}
}

func (s *Server) handleHTTPSessionPost(w http.ResponseWriter, r *http.Request, session *httpSession) {
	if !isJSONContentType(r.Header.Get("Content-Type")) {
		writeHTTPRelayError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return
	}
	config := session.config()
	var request httpInboundRequest
	if err := decodeHTTPJSON(w, r, &request, int64(config.maxMessageBytes*config.maxBatchMessages)+4096); err != nil {
		if errors.Is(err, errHTTPBodyTooLarge) {
			writeHTTPRelayError(w, http.StatusRequestEntityTooLarge, "body_too_large", "Request body is too large")
			return
		}
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_json", "Request body is not valid JSON")
		return
	}
	if request.ReceivedThrough > 0 {
		if err := session.acknowledgeOutbound(request.ReceivedThrough); err != nil {
			session.mu.Lock()
			expectedSequence := session.nextOutboundSequence
			session.mu.Unlock()
			writeHTTPRelayJSON(w, http.StatusConflict, map[string]interface{}{
				"error":            "invalid_received_through",
				"message":          "The acknowledged sequence is ahead of the server",
				"expectedSequence": expectedSequence,
			})
			return
		}
	}
	if len(request.Messages) > config.maxBatchMessages {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_batch", "Message batch size is invalid")
		return
	}
	if len(request.Messages) == 0 && request.ReceivedThrough == 0 {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_batch", "Message batch size is invalid")
		return
	}
	for _, message := range request.Messages {
		if len(message.Payload) == 0 || !isJSONObject(message.Payload) {
			writeHTTPRelayError(w, http.StatusBadRequest, "invalid_message", "Message payload must be a JSON object")
			return
		}
		if len(message.Payload) > config.maxMessageBytes {
			writeHTTPRelayError(w, http.StatusRequestEntityTooLarge, "message_too_large", "Message payload is too large")
			return
		}
		if err := session.acceptInbound(message.Sequence, message.Payload); err != nil {
			var sequenceErr *httpSequenceError
			if errors.As(err, &sequenceErr) {
				if sequenceErr.duplicate {
					continue
				}
				writeHTTPRelayJSON(w, http.StatusConflict, map[string]interface{}{
					"error":            "sequence_gap",
					"message":          "Message sequence is not the expected value",
					"expectedSequence": sequenceErr.expected,
				})
				return
			}
			if errors.Is(err, errHTTPMessageTooLarge) {
				writeHTTPRelayError(w, http.StatusRequestEntityTooLarge, "message_too_large", "Message payload is too large")
				return
			}
			writeHTTPRelayError(w, http.StatusTooManyRequests, "session_capacity", "Session capacity is exhausted")
			return
		}
	}
	session.mu.Lock()
	acceptedThrough := session.nextInboundSequence
	session.mu.Unlock()
	writeHTTPRelayJSON(w, http.StatusOK, map[string]uint64{"acceptedThrough": acceptedThrough})
}

func (s *Server) handleHTTPSessionPoll(w http.ResponseWriter, r *http.Request, session *httpSession) {
	if err := session.beginPoll(); err != nil {
		if errors.Is(err, errHTTPPollActive) {
			writeHTTPRelayError(w, http.StatusConflict, "poll_active", "Another poll is already active")
			return
		}
		writeHTTPRelayError(w, http.StatusNotFound, "not_found", "HTTP relay session was closed")
		return
	}
	defer session.endPoll()
	config := session.config()
	after, err := parseHTTPSequence(r.URL.Query().Get("after"))
	if err != nil {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_after", "The after sequence is invalid")
		return
	}
	wait, err := parseHTTPPollWait(r.URL.Query().Get("wait"), config.pollWait)
	if err != nil {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_wait", "The poll wait is invalid")
		return
	}
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
	messages, last, err := session.messagesAfter(after, config.maxBatchMessages)
	if err != nil {
		writeHTTPRelayError(w, http.StatusBadRequest, "invalid_after", "The after sequence is invalid")
		return
	}
	if len(messages) == 0 && wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-session.notify:
		case <-timer.C:
		case <-session.ctx.Done():
		}
		messages, last, _ = session.messagesAfter(after, config.maxBatchMessages)
	}
	response := httpPollResponse{
		Messages:     make([]httpOutboundMessage, 0, len(messages)),
		LastSequence: last,
	}
	for _, message := range messages {
		response.Messages = append(response.Messages, httpOutboundMessage{
			Sequence: message.sequence,
			Payload:  json.RawMessage(message.payload),
		})
	}
	writeHTTPRelayJSON(w, http.StatusOK, response)
}

var errHTTPBodyTooLarge = errors.New("HTTP request body too large")

func decodeHTTPJSON(w http.ResponseWriter, r *http.Request, value interface{}, limit int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(value); err != nil {
		if errors.Is(err, io.EOF) {
			return err
		}
		var maxBytesError *http.MaxBytesError
		if errors.As(err, &maxBytesError) {
			return errHTTPBodyTooLarge
		}
		return err
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func isJSONContentType(value string) bool {
	mediaType, _, err := mime.ParseMediaType(value)
	return err == nil && strings.EqualFold(mediaType, "application/json")
}

func isJSONObject(payload []byte) bool {
	payload = bytes.TrimSpace(payload)
	var value map[string]interface{}
	return len(payload) > 0 && payload[0] == '{' && json.Unmarshal(payload, &value) == nil
}

func parseHTTPSequence(value string) (uint64, error) {
	if value == "" {
		return 0, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func parseHTTPPollWait(value string, maximum time.Duration) (time.Duration, error) {
	if value == "" {
		return maximum, nil
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 1 || time.Duration(seconds)*time.Second > maximum {
		return 0, errors.New("poll wait outside allowed range")
	}
	return time.Duration(seconds) * time.Second, nil
}

type httpRemoteAddr string

func (a httpRemoteAddr) Network() string { return "tcp" }
func (a httpRemoteAddr) String() string  { return string(a) }
