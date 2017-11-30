package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"text/template"
	"time"

	feed "github.com/mattn/heroku/vim-jp/Godeps/_workspace/src/github.com/jteeuwen/go-pkg-rss"
	"github.com/mattn/heroku/vim-jp/Godeps/_workspace/src/github.com/lib/pq"
)

var (
	mutex sync.Mutex
	re    = regexp.MustCompile(`^[0-9]+\.[0-9]+`)
)

type Status struct {
	Events []Event `json:"events"`
}

type Event struct {
	Id      int      `json:"event_id"`
	Message *Message `json:"message"`
}

type Message struct {
	Id              string `json:"id"`
	Room            string `json:"room"`
	PublicSessionId string `json:"public_session_id"`
	IconUrl         string `json:"icon_url"`
	Type            string `json:"type"`
	SpeakerId       string `json:"speaker_id"`
	Nickname        string `json:"nickname"`
	Text            string `json:"text"`
}

func handleEvents(events []Event) string {
	results := ""
	for _, event := range events {
		tokens := strings.SplitN(event.Message.Text, " ", 2)
		if len(tokens) == 1 && tokens[0] == "!heroku" {
			results += ""
		}
	}
	return results
}

type FeedItem struct {
	Id          string    `json:"id"`
	Title       string    `json:"title"`
	Link        string    `json:"link"`
	Description string    `json:"description"`
	CreatedAt   time.Time `json:"created"`
}

type GitRef struct {
	Ref    string `json:"ref"`
	URL    string `json:"url"`
	Object struct {
		Type string `json:"type"`
		Sha  string `json:"sha"`
		URL  string `json:"url"`
	} `json:"object"`
}

func updatePatches(db *sql.DB) {
	mutex.Lock()
	defer mutex.Unlock()

	log.Println("Updating patches")

	sql := "insert into patches(title, description) values ($1, $2)"
	reConetnt := regexp.MustCompile(`<[^>]*>`)

	err := feed.New(5, true, nil,
		func(feed *feed.Feed, ch *feed.Channel, items []*feed.Item) {
			for _, item := range items {
				description := strings.TrimSpace(reConetnt.ReplaceAllString(item.Content.Text, "")) + "\n"
				title := strings.Trim(strings.Split(strings.Split(description, "\n")[0], " ")[1], " :")
				if !re.MatchString(title) {
					continue
				}
				if _, err := db.Exec(sql, title, description); err != nil {
					log.Println(err)
				}
			}
		},
	).Fetch("https://github.com/vim/vim/commits/master.atom", nil)
	if err != nil {
		log.Println(err)
	}
}

func feedItems(db *sql.DB, count int) ([]FeedItem, error) {
	mutex.Lock()
	defer mutex.Unlock()

	sql := "select title, description, created_at from patches order by created_at desc limit $1"
	rows, err := db.Query(sql, count)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	re := regexp.MustCompile(`^patch[^\n]+\n`)
	items := make([]FeedItem, 0)
	for rows.Next() {
		var title, description string
		var created_at time.Time
		err = rows.Scan(&title, &description, &created_at)
		if err != nil {
			return nil, err
		}
		if title == "8.0" {
			title = "8.0.0000"
		}
		title = re.ReplaceAllString(title, "")
		items = append(items, FeedItem{
			Id:          title,
			Title:       title,
			Link:        "https://github.com/vim/vim/releases/tag/v" + title,
			Description: description,
			CreatedAt:   created_at,
		})
	}
	return items, nil
}

func main() {
	cs, err := pq.ParseURL(os.Getenv("HEROKU_POSTGRESQL_BLUE_URL"))
	if err != nil {
		log.Fatal(err)
	}
	db, err := sql.Open("postgres", cs)
	if err != nil {
		log.Fatal(err)
	}

	_, err = db.Exec("create table patches ( id serial primary key, name varchar default null unique, title varchar default null, description varchar default null, created_at timestamp default now());")
	if err != nil {
		log.Println(err)
	}
	t, err := template.ParseFiles(filepath.Join(filepath.Dir(os.Args[0]), "feed.rss"))

	http.Handle("/", http.FileServer(http.Dir(filepath.Join(filepath.Dir(os.Args[0]), "public"))))

	http.HandleFunc("/slack", func(w http.ResponseWriter, r *http.Request) {
		v := regexp.MustCompile(`^!patch\s+(\S+)`).FindStringSubmatch(r.FormValue("text"))
		if len(v) > 2 {
			resp, err := http.Get("https://api.github.com/repos/vim/vim/git/refs/tags/v" + v[1])
			if err != nil {
				log.Println(err)
				return
			}
			defer resp.Body.Close()

			var ref GitRef
			err = json.NewDecoder(resp.Body).Decode(&ref)
			if err != nil {
				log.Println(err)
				return
			}
			json.NewEncoder(w).Encode(&struct {
				Text string `json:"text"`
			}{
				Text: "https://github.com/vim/vim/commit/" + ref.Object.Sha,
			})
		}
	})

	http.HandleFunc("/lingr", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		var status Status
		err := json.NewDecoder(r.Body).Decode(&status)
		if err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		results := handleEvents(status.Events)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if len(results) > 0 {
			results = strings.TrimRight(results, "\n")
			if runes := []rune(results); len(runes) > 1000 {
				results = string(runes[0:999])
			}
			fmt.Fprintln(w, results)
		}
	})

	http.HandleFunc("/patches/", func(w http.ResponseWriter, r *http.Request) {
		count, err := strconv.Atoi(r.FormValue("count"))
		if err != nil || count < 1 {
			count = 10
		}
		items, err := feedItems(db, count)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		t.Execute(w, items)
	})

	http.HandleFunc("/vimmers", func(w http.ResponseWriter, r *http.Request) {
		res, err := http.Get("https://raw.githubusercontent.com/vim-jp/vim-jp.github.com/master/vimmers/vimmers.json")
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer res.Body.Close()
		callback := r.FormValue("callback")
		w.Header().Set("Content-Type", "application/json")
		if callback != "" {
			b, err := ioutil.ReadAll(res.Body)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			fmt.Fprintf(w, "%s(%s)", callback, string(b))
		} else {
			io.Copy(w, res.Body)
		}
	})

	http.HandleFunc("/patches/json", func(w http.ResponseWriter, r *http.Request) {
		count, err := strconv.Atoi(r.FormValue("count"))
		if err != nil || count < 1 {
			count = 10
		}
		items, err := feedItems(db, count)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		b, err := json.Marshal(items)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		callback := r.URL.Query().Get("callback")
		w.Header().Set("Content-Type", "application/json")
		if callback != "" {
			fmt.Fprintf(w, "%s(%s)", callback, string(b))
		} else {
			w.Write(b)
		}
	})

	http.HandleFunc("/patches/pull", func(w http.ResponseWriter, r *http.Request) {
		updatePatches(db)
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte("OK"))
	})

	go func() {
		for {
			time.Sleep(10 * time.Minute)
			updatePatches(db)
		}
	}()

	fmt.Println("listening...")
	if err := http.ListenAndServe(":"+os.Getenv("PORT"), nil); err != nil {
		panic(err)
	}
}
