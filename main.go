package main

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"
	_ "github.com/mattn/go-sqlite3"
	"github.com/yuin/goldmark"
	"gopkg.in/yaml.v3"
)

//go:embed tmpl/*.html
var tmplFS embed.FS
var tmpl *template.Template

// Post represents a blog post with frontmatter
type Post struct {
	Title    string    `yaml:"title"`
	Date     time.Time `yaml:"date"`
	Content  template.HTML
	Slug     string
	FileName string
}

// PageData is the common data structure for page templates
type PageData struct {
	Posts   []Post
	Post    Post
	Devices []Device
	Meta    PageMeta
}

type PageMeta struct {
	Title string
	Count int
	NoNav bool
	User  *User
}

func main() {
	_ = godotenv.Load() // it's ok if there's no .env

	// Setup structured logging
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	// Initialize database
	if err := InitDB(); err != nil {
		slog.Error("Failed to initialize database", "error", err)
		panic(1)
	}
	defer DB.Close()

	// Run cleanup routine for expired sessions and magic links periodically
	go func() {
		for {
			if err := CleanupExpiredData(); err != nil {
				slog.Error("Failed to cleanup expired data", "error", err)
			}
			time.Sleep(1 * time.Hour)
		}
	}()

	// Load blog posts
	posts, err := loadPosts("./blog")
	if err != nil {
		slog.Error("Failed to load posts", "error", err)
	}

	// Parse templates with a function map for template definitions
	tmpl = template.New("").Funcs(template.FuncMap{
		"formatDate": func(t time.Time) string {
			return t.Format("January 2, 2006")
		},
	})

	// Parse all templates
	tmpl, err = tmpl.ParseFS(tmplFS, "tmpl/*.html")
	if err != nil {
		slog.Error("Failed to parse templates", "error", err)
		panic(1)
	}

	// HTTP handlers with error handling
	http.HandleFunc("/", ErrorHandler(func(w http.ResponseWriter, r *http.Request) error {
		ctx := r.Context()

		// Get current user if logged in
		var user *User
		currentUser, err := getCurrentUser(r)
		if err == nil {
			user = &currentUser
		}

		// Increment counter
		count, err := IncrementCounter()
		if err != nil {
			return fmt.Errorf("failed to update counter: %w", err)
		}

		slog.InfoContext(ctx, "Page view", "count", count, "path", r.URL.Path, "method", r.Method)

		// Homepage
		if r.URL.Path == "/" {
			w.Header().Set("Content-Type", "text/html")
			data := PageData{
				Meta: PageMeta{
					Count: count,
					Title: "My Site",
					NoNav: true, // Homepage has its own layout
					User:  user,
				},
			}
			if err := tmpl.ExecuteTemplate(w, "home.html", data); err != nil {
				return fmt.Errorf("failed to render home page: %w", err)
			}
			return nil
		}

		// Login page
		if r.URL.Path == "/login" {
			if r.Method == http.MethodPost {
				return handleLoginWithError(w, r)
			}

			w.Header().Set("Content-Type", "text/html")
			if err := tmpl.ExecuteTemplate(w, "login.html", LoginPage{
				Status: r.URL.Query().Get("status"),
				Error:  r.URL.Query().Get("error"),
				Meta: PageMeta{
					Title: "Login",
					Count: count,
					User:  user,
				},
			},
			); err != nil {
				return fmt.Errorf("failed to render login page: %w", err)
			}
			return nil
		}

		// Login verification
		if r.URL.Path == "/login/verify" {
			return handleLoginVerifyWithError(w, r)
		}

		// Logout
		if r.URL.Path == "/logout" && r.Method == http.MethodPost {
			return handleLogoutWithError(w, r)
		}

		// Blog index
		if r.URL.Path == "/blog" || r.URL.Path == "/blog/" {
			w.Header().Set("Content-Type", "text/html")
			data := PageData{
				Meta: PageMeta{
					Title: "Blog",
					Count: count,
					User:  user,
				},
				Posts: posts,
			}
			if err := tmpl.ExecuteTemplate(w, "blog.html", data); err != nil {
				return fmt.Errorf("failed to render blog index: %w", err)
			}
			return nil
		}

		// Blog post
		if strings.HasPrefix(r.URL.Path, "/blog/") {
			slug := strings.TrimPrefix(r.URL.Path, "/blog/")
			for _, post := range posts {
				if post.Slug == slug {
					w.Header().Set("Content-Type", "text/html")
					data := PageData{
						Meta: PageMeta{
							Title: post.Title,
							Count: count,
							User:  user,
						},
						Post: post,
					}
					if err := tmpl.ExecuteTemplate(w, "blog.html", data); err != nil {
						return fmt.Errorf("failed to render blog post: %w", err)
					}
					return nil
				}
			}
		}

		// Devices page - protected, only for logged-in users
		if r.URL.Path == "/devices" {
			// Require authentication
			if user == nil {
				http.Redirect(w, r, "/login", http.StatusSeeOther)
				return nil
			}

			// Insert sample devices for new users
			err = InsertSampleDevices(user.ID)
			if err != nil {
				return fmt.Errorf("failed to insert sample devices: %w", err)
			}

			// Get devices for this user
			devices, err := GetDevices(user.ID)
			if err != nil {
				return fmt.Errorf("failed to get devices: %w", err)
			}

			w.Header().Set("Content-Type", "text/html")
			data := PageData{
				Meta: PageMeta{
					Title: "Your Devices",
					Count: count,
					User:  user,
				},
				Devices: devices,
			}

			if err := tmpl.ExecuteTemplate(w, "devices.html", data); err != nil {
				return fmt.Errorf("failed to render devices page: %w", err)
			}
			return nil
		}

		// 404 for anything else
		return NewHTTPError(fmt.Errorf("page not found: %s", r.URL.Path), http.StatusNotFound)
	}))

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
		content, err := os.ReadFile(file)
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

// LoginPage holds data for the login page template
type LoginPage struct {
	Meta      PageMeta
	Status    string
	Error     string
	LoggedIn  bool
	UserEmail string
}

// getLoginPageData extracts query parameters and user data for the login page
func getLoginPageData(r *http.Request, count int, user *User) LoginPage {
	data := LoginPage{
		Status: r.URL.Query().Get("status"),
		Error:  r.URL.Query().Get("error"),
		Meta: PageMeta{
			Title: "Login",
			Count: count,
			User:  user,
		},
	}

	return data
}
