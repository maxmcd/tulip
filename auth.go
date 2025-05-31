package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
)

const (
	sessionCookieName = "tulip_session"
	cookieMaxAge      = 7 * 24 * 60 * 60 // 7 days in seconds
)

// generateRandomToken creates a secure random token
func generateRandomToken(length int) (string, error) {
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// getCurrentUser gets the current user from a request's cookies
func getCurrentUser(r *http.Request) (User, error) {
	cookie, err := r.Cookie(sessionCookieName)
	if err != nil {
		return User{}, fmt.Errorf("no session cookie: %w", err)
	}

	user, err := GetUserFromSession(cookie.Value)
	if err != nil {
		return User{}, fmt.Errorf("invalid session: %w", err)
	}

	return user, nil
}

// createLoginLink generates a magic login link for a user
func createLoginLink(email string, r *http.Request) (string, error) {
	// Create magic link token
	token, err := CreateMagicLink(email)
	if err != nil {
		return "", fmt.Errorf("failed to create magic link: %w", err)
	}

	// Build login URL
	scheme := "http"
	if r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https" {
		scheme = "https"
	}

	host := r.Host
	loginURL := fmt.Sprintf("%s://%s/login/verify?token=%s", scheme, host, url.QueryEscape(token))

	return loginURL, nil
}

// sendLoginEmail sends a magic login link to the user's email
func sendLoginEmail(email, loginURL string) error {
	subject := "Your Login Link for Tulip"
	body := fmt.Sprintf(`Hello,

Click the link below to log in to your Tulip account:

%s

This link will expire in 15 minutes.

If you didn't request this login link, you can safely ignore this email.

Best regards,
The Tulip Team
`, loginURL)

	return sendMail(email, subject, body)
}

// setSessionCookie sets a session cookie for the authenticated user
func setSessionCookie(w http.ResponseWriter, sessionToken string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    sessionToken,
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   cookieMaxAge,
	})
}

// clearSessionCookie clears the session cookie
func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	})
}

// handleLogin processes the login form submission
func handleLogin(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Only handle POST requests
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return
	}

	// Parse form
	if err := r.ParseForm(); err != nil {
		slog.ErrorContext(ctx, "Failed to parse form", "error", err)
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	// Get email from form
	email := strings.TrimSpace(r.FormValue("email"))
	if email == "" {
		http.Redirect(w, r, "/login?error=email_required", http.StatusSeeOther)
		return
	}

	// Generate login link
	loginURL, err := createLoginLink(email, r)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create login link", "error", err)
		http.Redirect(w, r, "/login?error=server_error", http.StatusSeeOther)
		return
	}

	// Send login email
	err = sendLoginEmail(email, loginURL)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send login email", "error", err, "email", email)
		http.Redirect(w, r, "/login?error=email_send_failed", http.StatusSeeOther)
		return
	}

	slog.InfoContext(ctx, "Login email sent", "email", email)
	http.Redirect(w, r, "/login?status=email_sent", http.StatusSeeOther)
}

// handleLoginVerify processes magic link verification
func handleLoginVerify(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get token from query
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/login?error=invalid_token", http.StatusSeeOther)
		return
	}

	// Verify token
	email, err := VerifyMagicLink(token)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to verify magic link", "error", err)
		http.Redirect(w, r, "/login?error=invalid_token", http.StatusSeeOther)
		return
	}

	// Get or create user
	user, err := CreateOrGetUser(email)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get/create user", "error", err, "email", email)
		http.Redirect(w, r, "/login?error=server_error", http.StatusSeeOther)
		return
	}

	// Create session
	sessionToken, err := CreateSession(user.ID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create session", "error", err, "user_id", user.ID)
		http.Redirect(w, r, "/login?error=server_error", http.StatusSeeOther)
		return
	}

	// Set session cookie
	setSessionCookie(w, sessionToken)

	slog.InfoContext(ctx, "User logged in", "user_id", user.ID, "email", user.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLogout processes logout requests
func handleLogout(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Get session token from cookie
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		// Delete session from database
		err = DeleteSession(cookie.Value)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to delete session", "error", err)
		}
	}

	// Clear session cookie
	clearSessionCookie(w)

	slog.InfoContext(ctx, "User logged out")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// handleLoginWithError is a wrapper for handleLogin that returns errors
func handleLoginWithError(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	// Only handle POST requests
	if r.Method != http.MethodPost {
		http.Redirect(w, r, "/login", http.StatusSeeOther)
		return nil
	}

	// Parse form
	if err := r.ParseForm(); err != nil {
		slog.ErrorContext(ctx, "Failed to parse form", "error", err)
		return NewHTTPError(err, http.StatusBadRequest)
	}

	// Get email from form
	email := strings.TrimSpace(r.FormValue("email"))
	if email == "" {
		http.Redirect(w, r, "/login?error=email_required", http.StatusSeeOther)
		return nil
	}

	// Generate login link
	loginURL, err := createLoginLink(email, r)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create login link", "error", err)
		http.Redirect(w, r, "/login?error=server_error", http.StatusSeeOther)
		return err
	}

	// Send login email
	err = sendLoginEmail(email, loginURL)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to send login email", "error", err, "email", email)
		http.Redirect(w, r, "/login?error=email_send_failed", http.StatusSeeOther)
		return err
	}

	slog.InfoContext(ctx, "Login email sent", "email", email)
	http.Redirect(w, r, "/login?status=email_sent", http.StatusSeeOther)
	return nil
}

// handleLoginVerifyWithError is a wrapper for handleLoginVerify that returns errors
func handleLoginVerifyWithError(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	// Get token from query
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Redirect(w, r, "/login?error=invalid_token", http.StatusSeeOther)
		return nil
	}

	// Verify token
	email, err := VerifyMagicLink(token)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to verify magic link", "error", err)
		http.Redirect(w, r, "/login?error=invalid_token", http.StatusSeeOther)
		return fmt.Errorf("invalid magic link: %w", err)
	}

	// Get or create user
	user, err := CreateOrGetUser(email)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get/create user", "error", err, "email", email)
		http.Redirect(w, r, "/login?error=server_error", http.StatusSeeOther)
		return fmt.Errorf("failed to get/create user: %w", err)
	}

	// Create session
	sessionToken, err := CreateSession(user.ID)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to create session", "error", err, "user_id", user.ID)
		http.Redirect(w, r, "/login?error=server_error", http.StatusSeeOther)
		return fmt.Errorf("failed to create session: %w", err)
	}

	// Set session cookie
	setSessionCookie(w, sessionToken)

	slog.InfoContext(ctx, "User logged in", "user_id", user.ID, "email", user.Email)
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}

// handleLogoutWithError is a wrapper for handleLogout that returns errors
func handleLogoutWithError(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()

	// Get session token from cookie
	cookie, err := r.Cookie(sessionCookieName)
	if err == nil {
		// Delete session from database
		err = DeleteSession(cookie.Value)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to delete session", "error", err)
			// Continue with logout even if session deletion fails
		}
	}

	// Clear session cookie
	clearSessionCookie(w)

	slog.InfoContext(ctx, "User logged out")
	http.Redirect(w, r, "/", http.StatusSeeOther)
	return nil
}
