package server

import "net/http"

// HTTPError carries a status code through handler code (mirrors HttpError).
type HTTPError struct {
	Status  int
	Message string
}

func (e *HTTPError) Error() string { return e.Message }

func badRequest(msg string) *HTTPError {
	return &HTTPError{Status: http.StatusBadRequest, Message: msg}
}
func notFound(msg string) *HTTPError { return &HTTPError{Status: http.StatusNotFound, Message: msg} }
func conflict(msg string) *HTTPError { return &HTTPError{Status: http.StatusConflict, Message: msg} }
