package main

import (
	"context"
	"database/sql"
	"embed"
	_ "embed"
	"encoding/json"
	"errors"
	"flag"
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

// pageTemplate stores all pre-compiled HTML templates for the application
type pageTemplate struct {
	summary *template.Template
}

// summaryTemplateData stores template data for summary.html
type summaryTemplateData struct {
	Summary  string
	Location string
}

// updateSubscription is the request body for creating/updating registration
type updateSubscription struct {
	Subscription webpush.Subscription `json:"subscription"`
	Locations    []string             `json:"locations"`
}

// registeredSubscription represents a registered webpush subscription.
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

	// summaries maps location keys to their latest weather summary
	summaries sync.Map
	// summaryChans stores a map of location key to the corresponding summary channel
	// which is used to track summary updates
	summaryChans map[string]chan string

	// subscriptions maps location keys to the list of registered subscriptions
	// that are subscribed to updates for the location
	subscriptions map[string][]registeredSubscription
	// subscriptionsMutex syncs writes to subscriptions
	subscriptionsMutex sync.Mutex

	// vapidPublicKey is the base64 url encoded VAPID public key
	vapidPublicKey string
	// vapidPrivateKey is the base64 url encoded VAPID private key
	vapidPrivateKey string
}

//go:embed web
var webDir embed.FS

var prompt = "The current time is 7am. Provide a summary of today's weather in %v below, as well as how to deal with the weather, such as how to dress for the weather, and whether they need an umbrella  Use celsius and fahrenheit for temperature. Mention %v in the summary, but don't add anything else, as the summary will be displayed on a website."

var supportedLocations = map[string]location{
	"london": {51.507351, -0.127758, "Europe/London"},
	"sf":     {37.774929, -122.419418, "America/Los_Angeles"},
	"sj":     {37.338207, -121.886330, "America/Los_Angeles"},
	"la":     {34.052235, -118.243683, "America/Los_Angeles"},
	"nyc":    {40.712776, -74.005974, "America/New_York"},
	"tokyo":  {35.689487, 139.691711, "Asia/Tokyo"},
}

var locationNames = map[string]string{
	"london": "London",
	"sf":     "San Francisco",
	"sj":     "San Jose",
	"la":     "Los Angeles",
	"nyc":    "New York City",
	"tokyo":  "Tokyo",
}

func main() {
	port := flag.Int("port", 8080, "the port that the server should listen on")
	genKeys := flag.Bool("generate-vapid-keys", false, "generate a new vapid key pair, which will be outputted to stdout.")

	flag.Parse()

	if *genKeys {
		generateKeys()
		return
	}

	err := godotenv.Load()
	if err != nil {
		log.Fatalln("please create a .env file using the provided template!")
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

	// schedule periodic updates of weather summary for each supported location
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

		// listen for summary updates, and publish updates to all update subscribers via web push
		go listenForSummaryUpdates(&state, locKey)

		s.Start()
	}

	err = loadSubscriptions(&state)
	if err != nil {
		log.Fatalf("failed to load existing subscriptions: %e\n", err)
	}

	http.HandleFunc("/", handleHTTPRequest(&state))

	log.Printf("server listening on %d...", *port)

	err = http.ListenAndServe(fmt.Sprintf(":%d", *port), nil)

	if err != nil {
		log.Printf("failed to start http server: %e\n", err)
	}

	for _, s := range schedulers {
		s.Shutdown()
	}
}

func generateKeys() {
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("all keys are base64 url encoded.")
	fmt.Printf("public key: %v\n", pub)
	fmt.Printf("private key: %v\n", priv)
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
			} else if request.Method == "PATCH" || request.Method == "DELETE" {
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

				switch request.Method {
				case "PATCH":
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
					} else {
						json.NewEncoder(writer).Encode(reg)
					}

				case "DELETE":
					err = deleteSubscription(state, regID)
					if err != nil {
						if errors.Is(err, sql.ErrNoRows) {
							writer.WriteHeader(http.StatusNotFound)
						} else {
							writer.WriteHeader(http.StatusInternalServerError)
						}
					} else {
						writer.WriteHeader(http.StatusNoContent)
					}
				}

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
					writer.WriteHeader(http.StatusNotFound)
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
		state.subscriptionsMutex.Lock()
		state.subscriptions[l] = append(state.subscriptions[l], reg)
		state.subscriptionsMutex.Unlock()
	}

	return &reg, nil
}

func deleteSubscription(state *state, regID uuid.UUID) error {
	_, err := state.db.Exec("DELETE FROM subscriptions WHERE id = ?", regID)
	return err
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
			{Text: fmt.Sprintf(prompt, locationNames[locKey])},
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

	opts := webpush.Options{
		VAPIDPublicKey:  state.vapidPublicKey,
		VAPIDPrivateKey: state.vapidPrivateKey,
		TTL:             30,
	}

	for {
		select {
		case summary := <-c:
			log.Printf("sending summary for %v to subscribers...\n", locKey)

			var wg sync.WaitGroup
			for _, sub := range state.subscriptions[locKey] {
				wg.Add(1)
				go func() {
					defer wg.Done()
					_, err := webpush.SendNotificationWithContext(state.ctx, []byte(summary), sub.Subscription, &opts)
					if err != nil {
						log.Printf("failed to send summary for %v to sub id %v: %e\n", locKey, sub.ID, err)
					}
				}()
			}

			wg.Wait()

		case <-state.ctx.Done():
			return
		}
	}
}
