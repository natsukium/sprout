// todo is a deliberately small web app for the sprout examples: a single-table
// todo list backed by PostgreSQL, server-rendered so there is no separate
// frontend build. It is the "app you are developing" — the podman and k3s
// examples deploy this same binary two different ways.
package main

import (
	"database/sql"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	_ "github.com/lib/pq"
)

var db *sql.DB

var page = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>todo</title>
<style>
  body { font: 16px/1.5 system-ui, sans-serif; max-width: 34rem; margin: 3rem auto; padding: 0 1rem; color: #1a1a1a; }
  h1 { font-size: 1.4rem; }
  form.add { display: flex; gap: .5rem; margin: 1rem 0 1.5rem; }
  form.add input[type=text] { flex: 1; padding: .5rem .6rem; border: 1px solid #ccc; border-radius: 6px; }
  button { padding: .5rem .8rem; border: 1px solid #ccc; border-radius: 6px; background: #f6f6f6; cursor: pointer; }
  ul { list-style: none; padding: 0; }
  li { display: flex; align-items: center; gap: .6rem; padding: .4rem 0; border-bottom: 1px solid #eee; }
  li .title { flex: 1; }
  li.done .title { text-decoration: line-through; color: #999; }
  .muted { color: #888; font-size: .9rem; }
</style>
</head>
<body>
<h1>todo <span class="muted">on {{.Host}}</span></h1>
<form class="add" method="post" action="/add">
  <input type="text" name="title" placeholder="What needs doing?" autofocus required>
  <button type="submit">Add</button>
</form>
<ul>
{{range .Items}}
  <li class="{{if .Done}}done{{end}}">
    <form method="post" action="/toggle"><input type="hidden" name="id" value="{{.ID}}"><button title="toggle">{{if .Done}}✓{{else}}○{{end}}</button></form>
    <span class="title">{{.Title}}</span>
    <form method="post" action="/delete"><input type="hidden" name="id" value="{{.ID}}"><button title="delete">✕</button></form>
  </li>
{{else}}
  <li class="muted">Nothing yet. Add the first item above.</li>
{{end}}
</ul>
</body>
</html>`))

type item struct {
	ID    int
	Title string
	Done  bool
}

func main() {
	dsn := env("DATABASE_URL", "postgres://todo:todo@localhost:5432/todo?sslmode=disable")
	addr := env("LISTEN_ADDR", ":8080")

	var err error
	db, err = sql.Open("postgres", dsn)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	waitForDB()
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS todos (
		id SERIAL PRIMARY KEY,
		title TEXT NOT NULL,
		done BOOLEAN NOT NULL DEFAULT false
	)`); err != nil {
		log.Fatalf("create schema: %v", err)
	}

	http.HandleFunc("/", index)
	http.HandleFunc("/add", add)
	http.HandleFunc("/toggle", toggle)
	http.HandleFunc("/delete", del)
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}

// waitForDB blocks until PostgreSQL accepts a connection, so the app can be
// started at the same time as its database without an ordering dependency.
func waitForDB() {
	for i := 0; ; i++ {
		if err := db.Ping(); err == nil {
			return
		}
		if i == 0 {
			log.Print("waiting for database …")
		}
		time.Sleep(time.Second)
	}
}

func index(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query("SELECT id, title, done FROM todos ORDER BY id")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var items []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Title, &it.Done); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		items = append(items, it)
	}
	host, _ := os.Hostname()
	_ = page.Execute(w, struct {
		Host  string
		Items []item
	}{host, items})
}

func add(w http.ResponseWriter, r *http.Request) {
	title := r.FormValue("title")
	if title != "" {
		if _, err := db.Exec("INSERT INTO todos (title) VALUES ($1)", title); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func toggle(w http.ResponseWriter, r *http.Request) {
	if id, err := strconv.Atoi(r.FormValue("id")); err == nil {
		if _, err := db.Exec("UPDATE todos SET done = NOT done WHERE id = $1", id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func del(w http.ResponseWriter, r *http.Request) {
	if id, err := strconv.Atoi(r.FormValue("id")); err == nil {
		if _, err := db.Exec("DELETE FROM todos WHERE id = $1", id); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
