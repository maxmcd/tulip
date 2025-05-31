package main

import (
	"fmt"
	"log/slog"
	"net/http"
)

// ErrorPageData contains data for the error template
type ErrorPageData struct {
	Meta         PageMeta
	Title        string
	ErrorMessage string
	ErrorDetail  string
	StackTrace   string
	Count        int
	User         *User
}

// ErrorHandler wraps an HTTP handler function to provide detailed error handling
func ErrorHandler(h func(http.ResponseWriter, *http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Catch any panics
		defer func() {
			if rec := recover(); rec != nil {
				err, ok := rec.(error)
				if !ok {
					err = fmt.Errorf("panic: %v", rec)
				}
				handleError(w, r, err, http.StatusInternalServerError)
			}
		}()

		// Handle regular errors
		if err := h(w, r); err != nil {
			code := http.StatusInternalServerError
			if httpErr, ok := err.(HTTPError); ok {
				code = httpErr.StatusCode
			}
			handleError(w, r, err, code)
		}
	}
}

// HTTPError represents an HTTP error with status code
type HTTPError struct {
	StatusCode int
	Err        error
}

// Error returns the error message
func (e HTTPError) Error() string {
	return e.Err.Error()
}

// NewHTTPError creates a new HTTP error
func NewHTTPError(err error, statusCode int) HTTPError {
	return HTTPError{
		StatusCode: statusCode,
		Err:        err,
	}
}

// handleError renders the error page with detailed information
func handleError(w http.ResponseWriter, r *http.Request, err error, statusCode int) {
	ctx := r.Context()

	// Log the error
	slog.ErrorContext(ctx, "Error handling request",
		"error", err.Error(),
		"path", r.URL.Path,
		"method", r.Method,
		"status", statusCode,
	)

	// Get current user if logged in
	var user *User
	currentUser, _ := getCurrentUser(r)
	if currentUser.ID > 0 {
		user = &currentUser
	}

	// Get page view count
	count, _ := IncrementCounter()

	// Get error details
	errorMessage := err.Error()
	var errorDetail string
	if httpErr, ok := err.(HTTPError); ok && httpErr.Err != nil {
		errorDetail = httpErr.Err.Error()
	}

	// Determine title based on status code
	var title string
	switch statusCode {
	case http.StatusNotFound:
		title = "Page Not Found"
	case http.StatusBadRequest:
		title = "Bad Request"
	case http.StatusForbidden:
		title = "Access Denied"
	case http.StatusUnauthorized:
		title = "Authentication Required"
	case http.StatusInternalServerError:
		title = "Internal Server Error"
	default:
		title = fmt.Sprintf("Error %d", statusCode)
	}

	// Render the error page
	data := ErrorPageData{
		Meta: PageMeta{
			Title: title,
		},
		Title:        title,
		ErrorMessage: errorMessage,
		ErrorDetail:  errorDetail,
		Count:        count,
		User:         user,
	}

	// Set the status code
	w.WriteHeader(statusCode)
	w.Header().Set("Content-Type", "text/html")

	// Try to render the error template
	if err := tmpl.ExecuteTemplate(w, "error.html", data); err != nil {
		// If template rendering fails, fall back to a simple error message
		slog.ErrorContext(ctx, "Failed to render error template", "error", err)
		http.Error(w, errorMessage, statusCode)
	}
}
