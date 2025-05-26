package main

import (
	"database/sql"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
)

func main() {
	// Setup structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Determine database path
	dbPath := "counter.db"
	if _, exists := os.LookupEnv("RENDER"); exists {
		dbPath = filepath.Join("/data", "counter.db")
	}
	slog.Info("Using database path", "path", dbPath)

	// Open database
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		slog.Error("Failed to open database", "error", err)
		panic(1)
	}
	defer db.Close()

	// Create table if not exists
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS counter (
		id INTEGER PRIMARY KEY,
		count INTEGER
	)`)
	if err != nil {
		slog.Error("Failed to create table", "error", err)
		panic(1)
	}

	// Initialize counter if needed
	var count int
	err = db.QueryRow("SELECT count FROM counter WHERE id = 1").Scan(&count)
	if err == sql.ErrNoRows {
		_, err = db.Exec("INSERT INTO counter (id, count) VALUES (1, 0)")
		if err != nil {
			slog.Error("Failed to initialize counter", "error", err)
			panic(1)
		}
		count = 0
	} else if err != nil {
		slog.Error("Failed to query counter", "error", err)
		panic(1)
	}

	// HTTP handlers
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// Increment counter
		_, err := db.Exec("UPDATE counter SET count = count + 1 WHERE id = 1")
		if err != nil {
			slog.ErrorContext(ctx, "Failed to update counter", "error", err)
			http.Error(w, "Failed to update counter", http.StatusInternalServerError)
			return
		}

		// Get current count
		err = db.QueryRow("SELECT count FROM counter WHERE id = 1").Scan(&count)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to read counter", "error", err)
			http.Error(w, "Failed to read counter", http.StatusInternalServerError)
			return
		}

		slog.InfoContext(ctx, "Page view", "count", count, "path", r.URL.Path, "method", r.Method)

		// Send minimal HTML response
		w.Header().Set("Content-Type", "text/html")
		html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head>
  <meta charset="UTF-8">
  <meta name="viewport" content="width=device-width, initial-scale=1.0">
  <title>Page Counter</title>
  <style>
    body {
      font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, Helvetica, Arial, sans-serif;
      display: flex;
      justify-content: center;
      align-items: center;
      height: 100vh;
      margin: 0;
      background-color: #fff;
    }
    .counter {
      font-size: 5rem;
      color: #333;
    }
  </style>
</head>
<body>
  <div class="counter">%d 🌷</div>
</body>
</html>`, count)
		fmt.Fprint(w, html)
	})

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	slog.Info("Server starting", "port", port)
	slog.Error("Server stopped", "error", http.ListenAndServe(":"+port, nil))
}
