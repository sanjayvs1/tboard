package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log"
	"net/http"
	"sync"
	"text/template"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	_ "github.com/lib/pq"
	"github.com/spf13/viper"
)

var db *sqlx.DB

type RateLimiter struct {
	mu       sync.Mutex
	posts    map[string][]time.Time
	limit    int
	interval time.Duration
}

func NewRateLimiter(limit int, interval time.Duration) *RateLimiter {
	return &RateLimiter{
		posts:    make(map[string][]time.Time),
		limit:    limit,
		interval: interval,
	}
}

func (rl *RateLimiter) Allow(userID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-rl.interval)

	if times, exists := rl.posts[userID]; exists {
		var valid []time.Time
		for _, t := range times {
			if t.After(cutoff) {
				valid = append(valid, t)
			}
		}
		rl.posts[userID] = valid
	} else {
		rl.posts[userID] = []time.Time{}
	}

	if len(rl.posts[userID]) >= rl.limit {
		return false
	}

	rl.posts[userID] = append(rl.posts[userID], now)
	return true
}

var rateLimiter = NewRateLimiter(10, time.Hour)

type Post struct {
	ID    int    `json:"id" db:"id" form:"id"`
	Title string `json:"title" db:"title" form:"title"`
	Body  string `json:"body" db:"body" form:"body"`
	User  string `json:"user" db:"user" form:"user"`
}

type Config struct {
	DB_URL string `mapstructure:"DB"`
}

func LoadConfig(path string) (config Config, err error) {
	viper.AddConfigPath(path)
	viper.SetConfigName("app")
	viper.SetConfigType("env")

	viper.AutomaticEnv()

	err = viper.ReadInConfig()
	if err != nil {
		return
	}

	err = viper.Unmarshal(&config)
	return
}

func createPost(c echo.Context) error {
	var post Post

	_, err := c.FormParams()
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"message": "Invalid form data"})
	}

	post.Title = c.FormValue("title")
	post.Body = c.FormValue("body")

	ip := c.RealIP()
	hash := sha256.Sum256([]byte(ip))
	post.User = hex.EncodeToString(hash[:8])

	if post.Title == "" || post.Body == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"message": "Title and body cannot be empty"})
	}

	if !rateLimiter.Allow(post.User) {
		return c.JSON(http.StatusTooManyRequests, map[string]string{"message": "Rate limit exceeded. Maximum 10 posts per hour."})
	}

	query := `INSERT INTO posts (title, body, "user") VALUES ($1, $2, $3) RETURNING id, title, body, "user"`
	err = db.Get(&post, query, post.Title, post.Body, post.User)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error saving post"})
	}

	// Fetch the updated list of posts
	var posts []Post
	err = db.Select(&posts, "SELECT id, title, body, \"user\" FROM posts ORDER BY id DESC")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error fetching updated posts"})
	}

	// Return only the posts list
	return c.Render(http.StatusOK, "posts_list.html", map[string]interface{}{
		"Posts":       posts,
		"CurrentUser": post.User,
	})
}

func updatePost(c echo.Context) error {
	id := c.Param("id")
	var post Post

	_, err := c.FormParams()
	if err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{"message": "Invalid form data"})
	}

	post.Title = c.FormValue("title")
	post.Body = c.FormValue("body")

	var existingPost Post
	err = db.Get(&existingPost, "SELECT \"user\" FROM posts WHERE id = $1", id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error updating post"})
	}
	post.User = existingPost.User

	if post.Title == "" || post.Body == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{"message": "Title and body cannot be empty"})
	}

	query := `UPDATE posts SET title=$1, body=$2 WHERE id=$3 RETURNING id, title, body, "user"`
	err = db.Get(&post, query, post.Title, post.Body, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error updating post"})
	}

	// Fetch the updated list of posts
	var posts []Post
	err = db.Select(&posts, "SELECT id, title, body, \"user\" FROM posts ORDER BY id DESC")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error fetching updated posts"})
	}

	// Get current user
	ip := c.RealIP()
	hash := sha256.Sum256([]byte(ip))
	currentUser := hex.EncodeToString(hash[:8])

	// Return only the posts list
	return c.Render(http.StatusOK, "posts_list.html", map[string]interface{}{
		"Posts":       posts,
		"CurrentUser": currentUser,
	})
}

func deletePost(c echo.Context) error {
	id := c.Param("id")

	query := `DELETE FROM posts WHERE id=$1`
	_, err := db.Exec(query, id)
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error deleting post"})
	}

	// Fetch the updated list of posts
	var posts []Post
	err = db.Select(&posts, "SELECT id, title, body, \"user\" FROM posts ORDER BY id DESC")
	if err != nil {
		return c.JSON(http.StatusInternalServerError, map[string]string{"message": "Error fetching updated posts"})
	}

	// Get current user
	ip := c.RealIP()
	hash := sha256.Sum256([]byte(ip))
	currentUser := hex.EncodeToString(hash[:8])

	// Return only the posts list
	return c.Render(http.StatusOK, "posts_list.html", map[string]interface{}{
		"Posts":       posts,
		"CurrentUser": currentUser,
	})
}

func getPost(c echo.Context) error {
	id := c.Param("id")
	var post Post

	query := `SELECT id, title, body, "user" FROM posts WHERE id=$1`
	err := db.Get(&post, query, id)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{"message": "Post not found"})
	}

	return c.JSON(http.StatusOK, post)
}

func main() {
	var err error

	config, err := LoadConfig(".")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	dsn := config.DB_URL
	db, err = sqlx.Open("postgres", dsn)
	if err != nil {
		log.Fatal("Error connecting to the database:", err)
	}
	defer db.Close()

	if err := db.Ping(); err != nil {
		log.Fatal("Error pinging the database:", err)
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS posts (
			id SERIAL PRIMARY KEY,
			title TEXT NOT NULL,
			body TEXT NOT NULL,
			"user" TEXT NOT NULL
		);
	`)
	if err != nil {
		log.Fatal("Error creating tables:", err)
	}

	e := echo.New()

	e.Use(middleware.Recover())
	e.Use(middleware.Logger())
	e.Use(middleware.CORS())

	e.Static("/static", "static")

	e.GET("/", func(c echo.Context) error {
		var posts []Post
		err := db.Select(&posts, "SELECT id, title, body, \"user\" FROM posts ORDER BY id DESC")
		if err != nil {
			if err.Error() == "relation \"posts\" does not exist" {
				_, createErr := db.Exec(`
					CREATE TABLE IF NOT EXISTS posts (
						id SERIAL PRIMARY KEY,
						title TEXT NOT NULL,
						body TEXT NOT NULL,
						"user" TEXT NOT NULL
					);
				`)
				if createErr != nil {
					log.Fatal("Error creating posts table:", createErr)
				}
			}

			return c.Render(http.StatusOK, "index.html", map[string]interface{}{
				"Posts":       []Post{},
				"Error":       "Unable to load posts. Please try again later.",
				"CurrentUser": "",
			})
		}

		ip := c.RealIP()
		hash := sha256.Sum256([]byte(ip))
		currentUser := hex.EncodeToString(hash[:8])

		if len(posts) == 0 {
			return c.Render(http.StatusOK, "index.html", map[string]interface{}{
				"Posts":       []Post{},
				"CurrentUser": currentUser,
			})
		}

		return c.Render(http.StatusOK, "index.html", map[string]interface{}{
			"Posts":       posts,
			"CurrentUser": currentUser,
		})
	})

	e.POST("/new_post", createPost)
	e.GET("/post/:id", getPost)
	e.PUT("/post/:id", updatePost)
	e.DELETE("/post/:id", deletePost)

	renderer := &TemplateRenderer{
		templates: template.Must(template.ParseGlob("templates/*.html")),
	}
	e.Renderer = renderer

	e.Logger.Fatal(e.Start(":8080"))
}

type Data struct {
	Message string
}

type TemplateRenderer struct {
	templates *template.Template
}

func (t *TemplateRenderer) Render(w io.Writer, name string, data interface{}, c echo.Context) error {
	return t.templates.ExecuteTemplate(w, name, data)
}
