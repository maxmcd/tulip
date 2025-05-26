package main

import (
	"bytes"
	"database/sql"
	"embed"
	"fmt"
	"html/template"
	"io/ioutil"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
)

//go:embed tmpl/*.html
var tmplFS embed.FS

// Post represents a blog post with frontmatter
type Post struct {
	Title    string    `yaml:"title"`
	Date     time.Time `yaml:"date"`
	Content  template.HTML
	Slug     string
	FileName string
}

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

	// Load blog posts
	posts, err := loadPosts("./blog")
	if err != nil {
		slog.Error("Failed to load posts", "error", err)
	}

	// Parse templates
	tmpl, err := template.ParseFS(tmplFS, "tmpl/*.html")
	if err != nil {
		slog.Error("Failed to parse templates", "error", err)
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

		// Homepage
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			data := struct {
				Count int
			}{
				Count: count,
			}
			if err := tmpl.ExecuteTemplate(w, "home.html", data); err != nil {
				slog.ErrorContext(ctx, "Failed to execute template", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
			return
		}

		// Blog index
		if r.URL.Path == "/blog" || r.URL.Path == "/blog/" {
			w.Header().Set("Content-Type", "text/html")
			data := struct {
				IsIndex bool
				Posts   []Post
				Count   int
				Title   string
			}{
				IsIndex: true,
				Posts:   posts,
				Count:   count,
				Title:   "Blog",
			}
			if err := tmpl.ExecuteTemplate(w, "blog.html", data); err != nil {
				slog.ErrorContext(ctx, "Failed to execute template", "error", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
			}
			return
		}

		// Blog post
		if strings.HasPrefix(r.URL.Path, "/blog/") {
			slug := strings.TrimPrefix(r.URL.Path, "/blog/")
			for _, post := range posts {
				if post.Slug == slug {
					w.Header().Set("Content-Type", "text/html")
					data := struct {
						IsIndex bool
						Post    Post
						Count   int
						Title   string
					}{
						IsIndex: false,
						Post:    post,
						Count:   count,
						Title:   post.Title,
					}
					if err := tmpl.ExecuteTemplate(w, "blog.html", data); err != nil {
						slog.ErrorContext(ctx, "Failed to execute template", "error", err)
						http.Error(w, "Internal server error", http.StatusInternalServerError)
					}
					return
				}
			}
		}

		// 404 for anything else
		http.NotFound(w, r)
	})

	// Start server
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	slog.Info("Server starting", "port", port)
	slog.Error("Server stopped", "error", http.ListenAndServe(":"+port, nil))
}

// loadPosts reads all markdown files from the blog directory
func loadPosts(dir string) ([]Post, error) {
	// Create blog directory if it doesn't exist
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		if err := os.Mkdir(dir, 0755); err != nil {
			return nil, fmt.Errorf("failed to create blog directory: %w", err)
		}
	}

	// Find all markdown files
	files, err := filepath.Glob(filepath.Join(dir, "*.md"))
	if err != nil {
		return nil, fmt.Errorf("failed to glob files: %w", err)
	}

	var posts []Post
	for _, file := range files {
		content, err := ioutil.ReadFile(file)
		if err != nil {
			slog.Error("Failed to read post", "file", file, "error", err)
			continue
		}

		post, err := parsePost(content, file)
		if err != nil {
			slog.Error("Failed to parse post", "file", file, "error", err)
			continue
		}

		posts = append(posts, post)
	}

	// Sort posts by date, newest first
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].Date.After(posts[j].Date)
	})

	return posts, nil
}

// parsePost extracts frontmatter and converts markdown to HTML
func parsePost(content []byte, filename string) (Post, error) {
	// Check for frontmatter delimiter
	parts := bytes.SplitN(content, []byte("---\n"), 3)
	if len(parts) < 3 {
		return Post{}, fmt.Errorf("invalid frontmatter format in %s", filename)
	}

	// Parse frontmatter
	var post Post
	if err := yaml.Unmarshal(parts[1], &post); err != nil {
		return Post{}, fmt.Errorf("failed to parse frontmatter: %w", err)
	}

	// Convert markdown to HTML
	var buf bytes.Buffer
	if err := goldmark.Convert(parts[2], &buf); err != nil {
		return Post{}, fmt.Errorf("failed to convert markdown: %w", err)
	}

	// Set slug from filename
	base := filepath.Base(filename)
	post.Slug = strings.TrimSuffix(base, filepath.Ext(base))
	post.FileName = filename
	post.Content = template.HTML(buf.String())

	return post, nil
}
