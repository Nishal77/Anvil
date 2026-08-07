package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/anvil-dev/anvil/internal/auth"
)

// maxRequestBodyBytes caps every request body this package decodes.
// Without it, json.Decoder will allocate without bound against an
// unauthenticated endpoint (BLOCKER, Phase 1 adversarial review).
const maxRequestBodyBytes = 16 * 1024

const minPasswordBytes = 8

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (r credentialsRequest) validate() error {
	if !strings.Contains(r.Email, "@") {
		return errors.New("email must contain @")
	}
	if len(r.Password) < minPasswordBytes {
		return errors.New("password must be at least 8 bytes")
	}
	return nil
}

type refreshRequest struct {
	RefreshToken string `json:"refresh_token"`
}

type tokenPairResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresAt    string `json:"expires_at"`
}

// decodeJSON bounds the request body (see maxRequestBodyBytes) and decodes
// it into dst, writing a 400 problem document and returning false on
// failure. Every handler in this package goes through this.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/malformed-body", "Malformed request body", err.Error())
		return false
	}
	return true
}

func writeTokenPair(w http.ResponseWriter, status int, pair auth.TokenPair) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(tokenPairResponse{
		AccessToken:  pair.AccessToken,
		RefreshToken: pair.RefreshToken,
		ExpiresAt:    pair.ExpiresAt.Format("2006-01-02T15:04:05Z07:00"),
	})
}

// handleRegister — POST /auth/register.
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := req.validate(); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "https://anvil.dev/errors/invalid-request", "Invalid request", err.Error())
		return
	}

	pair, err := s.auth.Register(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeInvalidCredentials(w, r)
			return
		}
		s.writeInternalError(w, r, err)
		return
	}
	writeTokenPair(w, http.StatusCreated, pair)
}

// handleLogin — POST /auth/login.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req credentialsRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	pair, err := s.auth.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, auth.ErrInvalidCredentials) {
			writeInvalidCredentials(w, r)
			return
		}
		s.writeInternalError(w, r, err)
		return
	}
	writeTokenPair(w, http.StatusOK, pair)
}

// handleAuthRefresh — POST /auth/refresh.
func (s *Server) handleAuthRefresh(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	pair, err := s.auth.Refresh(r.Context(), req.RefreshToken)
	if err != nil {
		if errors.Is(err, auth.ErrRefreshTokenExpired) || errors.Is(err, auth.ErrRefreshTokenRevoked) {
			writeInvalidCredentials(w, r)
			return
		}
		s.writeInternalError(w, r, err)
		return
	}
	writeTokenPair(w, http.StatusOK, pair)
}

// handleLogout — POST /auth/logout. Requires a Bearer access token
// (PRD §11.2). Only revokes a refresh token that belongs to the
// authenticated caller (Phase 1 adversarial review, MAJOR: this used to
// revoke any token presented, regardless of who was authenticated).
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	var req refreshRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	if err := s.auth.Logout(r.Context(), authenticatedUserID(r.Context()), req.RefreshToken); err != nil {
		s.writeInternalError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
