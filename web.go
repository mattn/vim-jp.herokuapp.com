package main

import (
	"crypto/sha1"
	"database/sql"
	"encoding/json"
	"fmt"
	"github.com/PuerkitoBio/goquery"
	"github.com/bmizerany/pq"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"text/template"
	"time"
)

const uri = "http://ftp.vim.org/vim/patches/7.4/"

var mutex sync.Mutex

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
	Id          string `json:"id"`
	Title       string `json:"title"`
	Link        string `json:"link"`
	Description string `json:"description"`
	CreatedAt   time.Time `json:"created"`
}

func updatePatches(db *sql.DB) {
	mutex.Lock()
	defer mutex.Unlock()

	log.Println("Updating patches")
	doc, err := goquery.NewDocument(uri)
	if err != nil {
		log.Printf("Failed to parse page: %s\n", err.Error())
		return
	}
	lines := strings.Split(doc.Find("pre").Text(), "\n")
	s, e := -1, -1
	sp := regexp.MustCompile(`^\s+SIZE\s+NAME\s+FIXES$`)
	ep := regexp.MustCompile(`^\s+\d`)
	for n, line := range lines {
		if s == -1 && sp.MatchString(line) {
			s = n
		} else if s != -1 && e == -1 && !ep.MatchString(line) {
			e = n
			break
		}
	}
	lines = lines[s+1 : e]

	tp := regexp.MustCompile(`^\s*\d+\s+(\S+)\s+(.*)$`)

	sql := "insert into patches(name, title, description) values ($1, $2, $3)"
	secret := os.Getenv("VIM_JP_PATCHES_SECRET")
	for _, line := range lines {
		tx, err := db.Begin()
		if err != nil {
			log.Printf("Failed to begin transaction: %s\n", err.Error())
			return
		}
		parts := tp.FindAllStringSubmatch(line, 1)[0]
		_, err = tx.Exec(sql, parts[1], parts[2], "")
		if err == nil {
			tx.Commit()
			log.Println("Posting notification")
			sha1h := sha1.New()
			fmt.Fprint(sha1h, "vim_jp"+secret)
			params := make(url.Values)
			params.Set("room", "vim")
			params.Set("bot", "vim_jp")
			params.Set("text", fmt.Sprintf("%s\n%s", parts[1], parts[2]))
			params.Set("bot_verifier", fmt.Sprintf("%x", sha1h.Sum(nil)))
			r, err := http.Get("http://lingr.com/api/room/say?" + params.Encode())
			if err == nil {
				r.Body.Close()
			} else {
				log.Printf("Failed to post notify: %s", err.Error())
			}
		} else {
			tx.Rollback()
			log.Printf("DB: %s\n", err.Error())
		}
	}
}

func feedItems(db *sql.DB) ([]FeedItem, error) {
	mutex.Lock()
	defer mutex.Unlock()

	sql := "select name, title, created_at from patches order by created_at desc limit 10"
	rows, err := db.Query(sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]FeedItem, 0)
	for rows.Next() {
		var name, title string
		var created_at time.Time
		err = rows.Scan(&name, &title, &created_at)
		if err != nil {
			return nil, err
		}
		items = append(items, FeedItem{
			Id:          name,
			Title:       name,
			Link:        fmt.Sprintf("%s%s", uri, name),
			Description: title,
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
		items, err := feedItems(db)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/rss+xml")
		t.Execute(w, items)
	})

	http.HandleFunc("/vimmers", func(w http.ResponseWriter, r *http.Request) {
		res, err := http.Get("https://raw.github.com/vim-jp/vim-jp.github.com/master/vimmers/vimmers.json")
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
		items, err := feedItems(db)
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
		w.Write([]byte("OK: " + uri))
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
