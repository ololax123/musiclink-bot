// musiclink-bot — two-way Apple Music <-> Spotify link translator for Signal.
//
// Listens to a signal-cli instance and, when someone posts an Apple Music or
// Spotify link, replies in the same chat (quoting the message) with the link
// for the other service.
//
// Backends (SIGNAL_BACKEND):
//
//	rest     bbernhard/signal-cli-rest-api (default)
//	         MODE=json-rpc        -> websocket receive (REST_RECEIVE=ws, default)
//	         MODE=normal/native   -> polling           (REST_RECEIVE=poll)
//	jsonrpc  plain signal-cli `daemon --tcp HOST:PORT`
package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
)

type Config struct {
	Backend        string
	SignalURL      string
	SignalNumber   string
	RestReceive    string
	SignalHost     string
	SignalPort     int
	Directions     string
	AllowedChats   map[string]bool
	TranslateOwn   bool
	Country        string
	ReplyAsQuote   bool
	ReplyNotFound  bool
	LoopTTLSeconds int
	SpotifyID      string
	SpotifySecret  string
	Debug          bool
}

var cfg Config

func getenv(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && v != "" {
		return v
	}
	return def
}

func loadConfig() Config {
	port, _ := strconv.Atoi(getenv("SIGNAL_PORT", "7583"))
	ttl, _ := strconv.Atoi(getenv("LOOP_TTL", "3600"))
	allowed := map[string]bool{}
	for _, c := range strings.Split(getenv("ALLOWED_CHATS", ""), ",") {
		if c = strings.TrimSpace(c); c != "" {
			allowed[c] = true
		}
	}
	return Config{
		Backend:        strings.ToLower(getenv("SIGNAL_BACKEND", "rest")),
		SignalURL:      strings.TrimRight(getenv("SIGNAL_URL", "http://signal-cli-rest-api:8080"), "/"),
		SignalNumber:   getenv("SIGNAL_NUMBER", ""),
		RestReceive:    strings.ToLower(getenv("REST_RECEIVE", "ws")),
		SignalHost:     getenv("SIGNAL_HOST", "signal-cli"),
		SignalPort:     port,
		Directions:     strings.ToLower(getenv("DIRECTIONS", "both")),
		AllowedChats:   allowed,
		TranslateOwn:   getenv("TRANSLATE_OWN", "1") == "1",
		Country:        strings.ToUpper(getenv("COUNTRY", "FI")),
		ReplyAsQuote:   getenv("REPLY_AS_QUOTE", "1") == "1",
		ReplyNotFound:  getenv("REPLY_NOT_FOUND", "0") == "1",
		LoopTTLSeconds: ttl,
		SpotifyID:      getenv("SPOTIFY_CLIENT_ID", ""),
		SpotifySecret:  getenv("SPOTIFY_CLIENT_SECRET", ""),
		Debug:          strings.EqualFold(getenv("LOG_LEVEL", "INFO"), "DEBUG"),
	}
}

func debugf(format string, a ...any) {
	if cfg.Debug {
		log.Printf("DEBUG "+format, a...)
	}
}

type Transport interface {
	Send(info *MsgInfo, message string) error
	Run(ctx context.Context) error
}

func main() {
	log.SetFlags(log.LstdFlags)
	cfg = loadConfig()
	log.Printf("musiclink-bot: backend=%s directions=%s country=%s", cfg.Backend, cfg.Directions, cfg.Country)

	var t Transport
	switch cfg.Backend {
	case "rest":
		if cfg.SignalNumber == "" {
			log.Fatal("SIGNAL_NUMBER is required for the rest backend")
		}
		t = &RestTransport{}
	case "jsonrpc":
		t = &JSONRPCTransport{}
	default:
		log.Fatalf("unknown SIGNAL_BACKEND %q (use rest or jsonrpc)", cfg.Backend)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := t.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
	log.Print("shutting down")
}
