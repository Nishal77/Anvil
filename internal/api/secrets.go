package api

import (
	"encoding/json"
	"errors"
	"net/http"
)

type putSecretRequest struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

func (r putSecretRequest) validate() error {
	if r.Name == "" {
		return errors.New("name is required")
	}
	if r.Value == "" {
		return errors.New("value is required")
	}
	return nil
}

type secretNamesResponse struct {
	Names []string `json:"names"`
}

// handlePutSecret — POST /secrets. Stores name/value envelope-encrypted
// under the caller's user ID. The value is never echoed back, logged,
// or stored in plaintext (PRD §16.5).
func (s *Server) handlePutSecret(w http.ResponseWriter, r *http.Request) {
	var req putSecretRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid request", err.Error())
		return
	}

	if err := s.auth.PutSecret(r.Context(), authenticatedUserID(r.Context()), req.Name, req.Value); err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleListSecretNames — GET /secrets. Returns names only — PRD §16.5:
// "There is no read-back endpoint. Ever."
func (s *Server) handleListSecretNames(w http.ResponseWriter, r *http.Request) {
	names, err := s.auth.ListSecretNames(r.Context(), authenticatedUserID(r.Context()))
	if err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(secretNamesResponse{Names: names})
}

// handleDeleteSecret — DELETE /secrets/{name}.
func (s *Server) handleDeleteSecret(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.auth.DeleteSecret(r.Context(), authenticatedUserID(r.Context()), name); err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
