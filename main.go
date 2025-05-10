package main

import (
	"context"
	"database/sql"
	"embed"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/SherClockHolmes/webpush-go"
	"github.com/go-co-op/gocron/v2"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"google.golang.org/genai"
	"html/template"
	"io"
	"log"
	"mime"
	_ "modernc.org/sqlite"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type apiKey struct {
	openWeatherMap string
}

type location struct {
	lat      float32
	lon      float32
	ianaName string
}

type pageTemplate struct {
	summary *template.Template
}

type summaryTemplateData struct {
	Summary  string
	Location string
}

type updateSubscription struct {
	Subscription webpush.Subscription `json:"subscription"`
	Locations    []string             `json:"locations"`
}

type registeredSubscription struct {
	ID           uuid.UUID             `json:"id"`
	Subscription *webpush.Subscription `json:"-"`
	Locations    []string              `json:"locations"`
}

type state struct {
	ctx      context.Context
	db       *sql.DB
	genai    *genai.Client
	apiKey   apiKey
	template pageTemplate

	summaries    sync.Map
	summaryChans map[string]chan string

	subscriptions      map[string][]registeredSubscription
	subscriptionsMutex sync.Mutex

	vapidPublicKey  string
	vapidPrivateKey string
}

//go:embed web
var webDir embed.FS

var prompt = "Provide a summaries of the weather below, mentioning the location. Provide suggestions on how to cope with the weather. Use celsius and fahrenheit for temperature. Do not add anything else. Respond in one line."

var supportedLocations = map[string]location{
	"london": {51.507351, -0.127758, "Europe/London"},
	"sf":     {37.774929, -122.419418, "America/Los_Angeles"},
	"sj":     {37.338207, -121.886330, "America/Los_Angeles"},
	"la":     {34.052235, -118.243683, "America/Los_Angeles"},
	"nyc":    {40.712776, -74.005974, "America/New_York"},
	"tokyo":  {35.689487, 139.691711, "Asia/Tokyo"},
}

func main() {
	err := godotenv.Load()
	if err != nil {
		log.Fatalln("Please create a .env file using the provided template!")
	}

	db, err := initDB()
	if err != nil {
		log.Fatalf("failed to initialize db: %e\n", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	genaiClient, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  os.Getenv("GEMINI_API_KEY"),
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		log.Fatalf("failed to initialize gemini client: %e\n", err)
	}

	summaryHTML, _ := webDir.ReadFile("web/summary.html")
	summaryPageTemplate, _ := template.New("summary.html").Parse(string(summaryHTML))

	state := state{
		ctx: ctx,
		db:  db,
		apiKey: apiKey{
			openWeatherMap: os.Getenv("OPEN_WEATHER_MAP_API_KEY"),
		},
		template: pageTemplate{
			summary: summaryPageTemplate,
		},
		summaries:    sync.Map{},
		summaryChans: map[string]chan string{},
		genai:        genaiClient,

		subscriptions: map[string][]registeredSubscription{},

		vapidPublicKey:  os.Getenv("VAPID_PUBLIC_KEY_BASE64"),
		vapidPrivateKey: os.Getenv("VAPID_PRIVATE_KEY_BASE64"),
	}

	var schedulers []gocron.Scheduler

	for locKey, loc := range supportedLocations {
		l, err := time.LoadLocation(loc.ianaName)
		if err != nil {
			log.Fatal(err)
		}

		s, err := gocron.NewScheduler(gocron.WithLocation(l))
		if err != nil {
			log.Fatal(err)
		}

		_, err = s.NewJob(
			gocron.DurationJob(time.Minute),
			gocron.NewTask(updateSummaries, &state, locKey, &loc),
			gocron.WithStartAt(gocron.WithStartImmediately()),
		)
		if err != nil {
			log.Fatal(err)
		}

		schedulers = append(schedulers, s)
		c := make(chan string)

		state.subscriptions[locKey] = []registeredSubscription{}
		state.summaryChans[locKey] = c

		go listenForSummaryUpdates(&state, locKey)

		s.Start()
	}

	loadSubscriptions(&state)

	http.HandleFunc("/", handleHTTPRequest(&state))
	http.ListenAndServe(":8080", nil)

	for _, s := range schedulers {
		s.Shutdown()
	}
}

func handleHTTPRequest(state *state) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		path := strings.TrimPrefix(request.URL.Path, "/")

		if path == "" {
			if request.Method == "" || request.Method == "GET" {
				index, _ := webDir.ReadFile("web/index.html")
				writer.Write(index)
			} else {
				writer.WriteHeader(http.StatusMethodNotAllowed)
			}
		} else if path == "vapid" {
			if request.Method == "" || request.Method == "GET" {
				writer.Write([]byte(state.vapidPublicKey))
			} else {
				writer.WriteHeader(http.StatusMethodNotAllowed)
			}
		} else if strings.HasPrefix(path, "registrations") {
			if path == "registrations" && request.Method == "POST" {
				defer request.Body.Close()

				update := updateSubscription{}
				err := json.NewDecoder(request.Body).Decode(&update)
				if err != nil {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}

				reg, err := registerSubscription(state, &update)
				if err != nil {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}

				err = json.NewEncoder(writer).Encode(reg)
				if err != nil {
					writer.WriteHeader(http.StatusBadRequest)
				}
			} else if request.Method == "PATCH" {
				parts := strings.Split(path, "/")
				if len(parts) < 2 {
					writer.WriteHeader(http.StatusMethodNotAllowed)
					return
				}

				regID, err := uuid.Parse(parts[1])
				if err != nil {
					writer.WriteHeader(http.StatusNotFound)
					return
				}

				defer request.Body.Close()

				update := updateSubscription{}
				err = json.NewDecoder(request.Body).Decode(&update)
				if err != nil {
					writer.WriteHeader(http.StatusBadRequest)
					return
				}

				reg, err := updateRegisteredSubscription(state, regID, &update)
				if err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						writer.WriteHeader(http.StatusNotFound)
					} else {
						writer.WriteHeader(http.StatusInternalServerError)
					}
					return
				}

				json.NewEncoder(writer).Encode(reg)
			} else {
				writer.WriteHeader(http.StatusMethodNotAllowed)
			}

		} else {
			if request.Method != "" && request.Method != "GET" {
				writer.WriteHeader(http.StatusMethodNotAllowed)
				return
			}

			summary, ok := state.summaries.Load(path)
			if ok {
				state.template.summary.Execute(writer, summaryTemplateData{summary.(string), path})
			} else {
				f, err := webDir.ReadFile("web/" + path)
				if err != nil {
					writer.WriteHeader(404)
				} else {
					m := mime.TypeByExtension(filepath.Ext(path))
					if m != "" {
						writer.Header().Set("Content-Type", m)
					}
					writer.Write(f)
				}
			}
		}
	}
}

func initDB() (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:data.sqlite")
	if err != nil {
		log.Fatalln("failed to initialize database")
	}

	_, err = db.Exec(`
		CREATE TABLE IF NOT EXISTS subscriptions(
			id TEXT PRIMARY KEY,
			locations TEXT NOT NULL,
			subscription_json TEXT NOT NULL
		);
	`)
	if err != nil {
		return nil, err
	}

	return db, nil
}

func loadSubscriptions(state *state) error {
	rows, err := state.db.Query(`SELECT id, locations, subscription_json FROM subscriptions;`)
	if err != nil {
		return err
	}

	for rows.Next() {
		var id string
		var locations string
		var j string

		err := rows.Scan(&id, &locations, &j)
		if err != nil {
			continue
		}

		s := webpush.Subscription{}
		err = json.Unmarshal([]byte(j), &s)
		if err != nil {
			continue
		}

		reg := registeredSubscription{
			ID:           uuid.MustParse(id),
			Locations:    strings.Split(locations, ","),
			Subscription: &s,
		}

		for _, l := range reg.Locations {
			state.subscriptions[l] = append(state.subscriptions[l], reg)
		}
	}

	return nil
}

func updateRegisteredSubscription(state *state, id uuid.UUID, update *updateSubscription) (*registeredSubscription, error) {
	j, err := json.Marshal(update.Subscription)
	if err != nil {
		return nil, err
	}

	_, err = state.db.Exec(
		"UPDATE subscriptions SET subscription_json = ?, locations = ? WHERE id = ?",
		string(j), strings.Join(update.Locations, ","), id,
	)
	if err != nil {
		return nil, err
	}

	return &registeredSubscription{
		ID:           id,
		Subscription: &update.Subscription,
		Locations:    update.Locations,
	}, nil
}

func registerSubscription(state *state, sub *updateSubscription) (*registeredSubscription, error) {
	j, err := json.Marshal(sub.Subscription)
	if err != nil {
		return nil, err
	}

	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}

	_, err = state.db.Exec(
		"INSERT INTO subscriptions (id, locations, subscription_json) VALUES (?, ?, ?);",
		id, strings.Join(sub.Locations, ","), string(j),
	)
	if err != nil {
		return nil, err
	}

	reg := registeredSubscription{
		ID:           id,
		Subscription: &sub.Subscription,
		Locations:    sub.Locations,
	}

	for _, l := range sub.Locations {
		state.subscriptions[l] = append(state.subscriptions[l], reg)
	}

	return &reg, nil
}

func updateSummaries(state *state, locKey string, loc *location) {
	log.Printf("updating summary for %v...\n", locKey)

	resp, err := http.Get(fmt.Sprintf("https://api.openweathermap.org/data/2.5/weather?lat=%f&lon=%f&appid=%v", loc.lat, loc.lon, state.apiKey.openWeatherMap))
	if err != nil {
		log.Printf("error updating summaries for %s: %e\n", locKey, err)
		return
	}

	b, err := io.ReadAll(resp.Body)
	defer resp.Body.Close()
	if err != nil {
		log.Printf("error updating summaries for %s: %e\n", locKey, err)
		return
	}

	result, err := state.genai.Models.GenerateContent(state.ctx, "gemini-2.0-flash", []*genai.Content{{
		Parts: []*genai.Part{
			{Text: prompt},
			{Text: string(b)},
		},
	}}, nil)
	if err != nil {
		log.Printf("error updating summaries for %s: %e\n", locKey, err)
		return
	}

	summary := result.Text()
	c := state.summaryChans[locKey]

	state.summaries.Store(locKey, summary)
	if len(state.subscriptions[locKey]) > 0 {
		c <- summary
	}

	log.Printf("updated summary for %v successfully\n", locKey)
}

func listenForSummaryUpdates(state *state, locKey string) {
	c := state.summaryChans[locKey]
	for {
		select {
		case summary := <-c:
			log.Printf("sending summary for %v to subscribers...\n", locKey)
			for _, sub := range state.subscriptions[locKey] {
				_, err := webpush.SendNotificationWithContext(state.ctx, []byte(summary), sub.Subscription, &webpush.Options{
					VAPIDPublicKey:  state.vapidPublicKey,
					VAPIDPrivateKey: state.vapidPrivateKey,
					TTL:             30,
				})
				if err != nil {
					log.Printf("failed to send notification %e\n", err)
				}
			}

		case <-state.ctx.Done():
			return
		}
	}
}
