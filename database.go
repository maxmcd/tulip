package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// DB is a global database connection
var DB *sql.DB

// InitDB initializes the database connection and creates necessary tables
func InitDB() error {
	// Determine database path
	dbPath := "tulip.db"
	if _, exists := os.LookupEnv("RENDER"); exists {
		dbPath = filepath.Join("/data", "tulip.db")
	}
	slog.Info("Using database path", "path", dbPath)

	// Open database
	var err error
	DB, err = sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("failed to open database: %w", err)
	}

	// Create tables if they don't exist
	err = createTables()
	if err != nil {
		return fmt.Errorf("failed to create tables: %w", err)
	}

	// Initialize counter if needed
	err = initializeCounter()
	if err != nil {
		return fmt.Errorf("failed to initialize counter: %w", err)
	}

	return nil
}

// createTables creates all required tables if they don't exist
func createTables() error {
	queries := []string{
		`CREATE TABLE IF NOT EXISTS counter (
			id INTEGER PRIMARY KEY,
			count INTEGER
		)`,
		`CREATE TABLE IF NOT EXISTS users (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT UNIQUE NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			token TEXT UNIQUE NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id)
		)`,
		`CREATE TABLE IF NOT EXISTS magic_links (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			email TEXT NOT NULL,
			token TEXT UNIQUE NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			used BOOLEAN NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`DROP TABLE IF EXISTS devices`,
		`CREATE TABLE IF NOT EXISTS devices (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id INTEGER NOT NULL,
			hostname TEXT NOT NULL,
			device_type TEXT NOT NULL,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			FOREIGN KEY (user_id) REFERENCES users(id)
		)`,
	}

	for _, query := range queries {
		_, err := DB.Exec(query)
		if err != nil {
			return fmt.Errorf("failed to create table: %w", err)
		}
	}

	return nil
}

// initializeCounter initializes the counter table if needed
func initializeCounter() error {
	var count int
	err := DB.QueryRow("SELECT count FROM counter WHERE id = 1").Scan(&count)
	if err == sql.ErrNoRows {
		_, err = DB.Exec("INSERT INTO counter (id, count) VALUES (1, 0)")
		if err != nil {
			return fmt.Errorf("failed to initialize counter: %w", err)
		}
	} else if err != nil {
		return fmt.Errorf("failed to query counter: %w", err)
	}
	return nil
}

// IncrementCounter increments the page view counter and returns the new count
func IncrementCounter() (int, error) {
	_, err := DB.Exec("UPDATE counter SET count = count + 1 WHERE id = 1")
	if err != nil {
		return 0, fmt.Errorf("failed to update counter: %w", err)
	}

	var count int
	err = DB.QueryRow("SELECT count FROM counter WHERE id = 1").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to read counter: %w", err)
	}

	return count, nil
}

// User represents a user in the database
type User struct {
	ID        int64
	Email     string
	CreatedAt time.Time
}

// CreateOrGetUser creates a new user or gets an existing one by email
func CreateOrGetUser(email string) (User, error) {
	var user User

	// Check if user exists
	err := DB.QueryRow("SELECT id, email, created_at FROM users WHERE email = ?", email).Scan(&user.ID, &user.Email, &user.CreatedAt)
	if err == sql.ErrNoRows {
		// Create new user
		result, err := DB.Exec("INSERT INTO users (email) VALUES (?)", email)
		if err != nil {
			return User{}, fmt.Errorf("failed to create user: %w", err)
		}

		id, err := result.LastInsertId()
		if err != nil {
			return User{}, fmt.Errorf("failed to get user ID: %w", err)
		}

		user.ID = id
		user.Email = email
		user.CreatedAt = time.Now()
		return user, nil
	} else if err != nil {
		return User{}, fmt.Errorf("failed to query user: %w", err)
	}

	return user, nil
}

// CreateMagicLink creates a new magic link for the given email
func CreateMagicLink(email string) (string, error) {
	// Generate a random token
	token, err := generateRandomToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}

	// Set expiration time (15 minutes from now)
	expiresAt := time.Now().Add(15 * time.Minute)

	// Insert into database
	_, err = DB.Exec(
		"INSERT INTO magic_links (email, token, expires_at) VALUES (?, ?, ?)",
		email, token, expiresAt,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create magic link: %w", err)
	}

	return token, nil
}

// VerifyMagicLink verifies a magic link token and returns the associated email if valid
func VerifyMagicLink(token string) (string, error) {
	var email string
	var expiresAt time.Time
	var used bool

	// Find the magic link
	err := DB.QueryRow(
		"SELECT email, expires_at, used FROM magic_links WHERE token = ?",
		token,
	).Scan(&email, &expiresAt, &used)

	if err == sql.ErrNoRows {
		return "", fmt.Errorf("invalid magic link")
	} else if err != nil {
		return "", fmt.Errorf("failed to query magic link: %w", err)
	}

	// Check if it's expired
	if time.Now().After(expiresAt) {
		return "", fmt.Errorf("magic link expired")
	}

	// Check if it's been used
	if used {
		return "", fmt.Errorf("magic link already used")
	}

	// Mark it as used
	_, err = DB.Exec("UPDATE magic_links SET used = 1 WHERE token = ?", token)
	if err != nil {
		return "", fmt.Errorf("failed to mark magic link as used: %w", err)
	}

	return email, nil
}

// CreateSession creates a new session for the given user
func CreateSession(userID int64) (string, error) {
	// Generate a random token
	token, err := generateRandomToken(32)
	if err != nil {
		return "", fmt.Errorf("failed to generate token: %w", err)
	}

	// Set expiration time (7 days from now)
	expiresAt := time.Now().Add(7 * 24 * time.Hour)

	// Insert into database
	_, err = DB.Exec(
		"INSERT INTO sessions (user_id, token, expires_at) VALUES (?, ?, ?)",
		userID, token, expiresAt,
	)
	if err != nil {
		return "", fmt.Errorf("failed to create session: %w", err)
	}

	return token, nil
}

// GetUserFromSession retrieves a user from a session token
func GetUserFromSession(token string) (User, error) {
	var user User
	var expiresAt time.Time

	// Find the session and user
	err := DB.QueryRow(`
		SELECT u.id, u.email, u.created_at, s.expires_at
		FROM sessions s
		JOIN users u ON s.user_id = u.id
		WHERE s.token = ?
	`, token).Scan(&user.ID, &user.Email, &user.CreatedAt, &expiresAt)

	if err == sql.ErrNoRows {
		return User{}, fmt.Errorf("invalid session")
	} else if err != nil {
		return User{}, fmt.Errorf("failed to query session: %w", err)
	}

	// Check if it's expired
	if time.Now().After(expiresAt) {
		// Delete expired session
		_, _ = DB.Exec("DELETE FROM sessions WHERE token = ?", token)
		return User{}, fmt.Errorf("session expired")
	}

	return user, nil
}

// DeleteSession removes a session by token
func DeleteSession(token string) error {
	_, err := DB.Exec("DELETE FROM sessions WHERE token = ?", token)
	if err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}
	return nil
}

// CleanupExpiredData removes expired sessions and magic links
func CleanupExpiredData() error {
	// Delete expired sessions
	_, err := DB.Exec("DELETE FROM sessions WHERE expires_at < ?", time.Now())
	if err != nil {
		return fmt.Errorf("failed to delete expired sessions: %w", err)
	}

	// Delete expired magic links
	_, err = DB.Exec("DELETE FROM magic_links WHERE expires_at < ?", time.Now())
	if err != nil {
		return fmt.Errorf("failed to delete expired magic links: %w", err)
	}

	return nil
}

// Device represents a device in the database
type Device struct {
	ID         int64
	UserID     int64
	Hostname   string
	DeviceType string
	CreatedAt  time.Time
}

// GetDevices retrieves all devices for a specific user
func GetDevices(userID int64) ([]Device, error) {
	rows, err := DB.Query(`
		SELECT id, user_id, hostname, device_type, created_at
		FROM devices
		WHERE user_id = ?
		ORDER BY created_at DESC
	`, userID)

	if err != nil {
		return nil, fmt.Errorf("failed to query devices: %w", err)
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var device Device

		err := rows.Scan(
			&device.ID,
			&device.UserID,
			&device.Hostname,
			&device.DeviceType,
			&device.CreatedAt,
		)

		if err != nil {
			return nil, fmt.Errorf("failed to scan device row: %w", err)
		}

		devices = append(devices, device)
	}

	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("error iterating device rows: %w", err)
	}

	return devices, nil
}

// InsertSampleDevices adds sample devices for a user if they don't have any
func InsertSampleDevices(userID int64) error {
	// Check if user already has devices
	var count int
	err := DB.QueryRow("SELECT COUNT(*) FROM devices WHERE user_id = ?", userID).Scan(&count)
	if err != nil {
		return fmt.Errorf("failed to check existing devices: %w", err)
	}

	// If user already has devices, don't add samples
	if count > 0 {
		return nil
	}

	// Sample device data
	sampleDevices := []struct {
		hostname   string
		deviceType string
	}{
		{"maxm", "linux"},
	}

	// Insert sample devices
	for _, device := range sampleDevices {
		_, err := DB.Exec(`
			INSERT INTO devices (user_id, hostname, device_type)
			VALUES (?, ?, ?)
		`, userID, device.hostname, device.deviceType)

		if err != nil {
			return fmt.Errorf("failed to insert sample device: %w", err)
		}
	}

	return nil
}
