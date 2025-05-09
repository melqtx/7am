package main

import (
	"context"
	"embed"
	_ "embed"
	"fmt"
	"github.com/coder/websocket"
	"github.com/go-co-op/gocron/v2"
	"github.com/joho/godotenv"
	"golang.org/x/time/rate"
	"google.golang.org/genai"
	"html/template"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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

type state struct {
	ctx             context.Context
	apiKey          apiKey
	template        pageTemplate
	summaries       sync.Map
	summaryChans    map[string]chan string
	genai           *genai.Client
	subscriberCount atomic.Int64
}

type summaryTemplateData struct {
	Summary  string
	Location string
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
		apiKey: apiKey{
			openWeatherMap: os.Getenv("OPEN_WEATHER_MAP_API_KEY"),
		},
		template: pageTemplate{
			summary: summaryPageTemplate,
		},
		summaries:    sync.Map{},
		summaryChans: map[string]chan string{},
		genai:        genaiClient,
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
			gocron.NewTask(updateSummaries, &state, locKey, &loc))
		if err != nil {
			log.Fatal(err)
		}

		schedulers = append(schedulers, s)
		state.summaryChans[locKey] = make(chan string)

		s.Start()
	}

	http.HandleFunc("/", handleHTTPRequest(&state))
	http.ListenAndServe(":8080", nil)

	for _, s := range schedulers {
		s.Shutdown()
	}
}

func handleHTTPRequest(state *state) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		path := strings.TrimPrefix(request.URL.Path, "/")

		switch path {
		case "":
			index, _ := webDir.ReadFile("web/index.html")
			writer.Write(index)

		case "ws":
			conn, err := websocket.Accept(writer, request, nil)
			if err != nil {
				log.Printf("error accepting incoming ws connection: %e\n", err)
			}
			defer conn.CloseNow()

			log.Println("accepted incoming websocket connection")

			locKey := request.URL.Query().Get("location")
			if c, ok := state.summaryChans[locKey]; ok {
				state.subscriberCount.Add(1)
				sendSummaryUpdates(state, c, conn)
				state.subscriberCount.Add(-1)
			}

		default:
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

func sendSummaryUpdates(state *state, c <-chan string, conn *websocket.Conn) {
	l := rate.NewLimiter(rate.Every(time.Millisecond*100), 10)
	ctx, cancel := context.WithCancel(state.ctx)
	defer cancel()

	for {
		err := l.Wait(ctx)
		if err != nil {
			return
		}

		select {
		case summary := <-c:
			log.Println("summary updated. sending updates via sockets...")

			w, err := conn.Writer(ctx, websocket.MessageText)
			if err != nil {
				return
			}
			_, err = w.Write([]byte(summary))
			if err != nil {
				return
			}
			w.Close()
		case <-ctx.Done():
			return
		}
	}
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
	if state.subscriberCount.Load() > 0 {
		c <- summary
	}

	log.Printf("updated summary for %v successfully\n", locKey)
}
