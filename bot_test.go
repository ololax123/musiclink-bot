package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

const (
	amURL = "https://music.apple.com/fi/album/x/1440650428?i=1440650711"
	spURL = "https://open.spotify.com/track/7tFiyTwD0nx5a1eklYtX2J"
	bot   = "+358999000000"
)

type fakeTransport struct {
	mu   sync.Mutex
	sent []struct {
		info *MsgInfo
		msg  string
	}
}

func (f *fakeTransport) Send(info *MsgInfo, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sent = append(f.sent, struct {
		info *MsgInfo
		msg  string
	}{info, msg})
	return nil
}
func (f *fakeTransport) Run(context.Context) error { return nil }

func setup(t *testing.T) *fakeTransport {
	t.Helper()
	t.Setenv("SIGNAL_NUMBER", bot)
	cfg = loadConfig()
	posted.Lock()
	posted.m = map[string]time.Time{}
	posted.Unlock()
	translateFn = func(u, to string) string {
		switch {
		case u == amURL && to == "spotify":
			return spURL
		case u == spURL && to == "apple":
			return amURL
		}
		return ""
	}
	return &fakeTransport{}
}

func groupMsg(text string) *Envelope {
	return &Envelope{SourceNumber: "+358401111111", Timestamp: 1,
		DataMessage: &DataMessage{Timestamp: 1, Message: text, GroupInfo: &GroupInfo{GroupID: "GRP="}}}
}

func TestAppleToSpotify(t *testing.T) {
	f := setup(t)
	handle(groupMsg("listen "+amURL+" !"), f)
	if len(f.sent) != 1 || !strings.Contains(f.sent[0].msg, spURL) || f.sent[0].info.GroupID != "GRP=" {
		t.Fatalf("unexpected: %+v", f.sent)
	}
}

func TestSpotifyToApple(t *testing.T) {
	f := setup(t)
	handle(groupMsg(spURL), f)
	if len(f.sent) != 1 || !strings.Contains(f.sent[0].msg, amURL) {
		t.Fatalf("unexpected: %+v", f.sent)
	}
}

func TestNoLoop(t *testing.T) {
	f := setup(t)
	handle(groupMsg(amURL), f)
	reply := f.sent[0].msg

	echo := &Envelope{SourceNumber: bot}
	echo.SyncMessage = &struct {
		SentMessage *SentMessage `json:"sentMessage"`
	}{&SentMessage{DataMessage: DataMessage{Timestamp: 2, Message: reply, GroupInfo: &GroupInfo{GroupID: "GRP="}}}}

	handle(echo, f)                                            // echoed bot reply
	handle(groupMsg(strings.ReplaceAll(reply, Marker, "")), f) // marker stripped, prefix kept
	handle(groupMsg(spURL), f)                                 // bare copy of bot's link
	handle(groupMsg(spURL+" "+amURL), f)                       // both links present
	if len(f.sent) != 1 {
		t.Fatalf("loop protection failed, %d replies", len(f.sent))
	}
}

func TestDirectionsAndAllowlist(t *testing.T) {
	f := setup(t)
	cfg.Directions = "am2sp"
	handle(groupMsg(spURL), f)
	cfg.Directions = "both"
	cfg.AllowedChats = map[string]bool{"group.T1RIRVI=": true}
	handle(groupMsg(amURL), f) // GRP= not allowed
	if len(f.sent) != 0 {
		t.Fatalf("expected no replies, got %d", len(f.sent))
	}
	cfg.AllowedChats = map[string]bool{restGroupID("GRP="): true}
	handle(groupMsg(amURL), f)
	if len(f.sent) != 1 {
		t.Fatal("allowed group (REST id form) should get a reply")
	}
}

func TestLinkParsing(t *testing.T) {
	got := findLinks(spotifyRe, "a https://open.spotify.com/intl-fi/track/XYZ?si=a, b https://spotify.link/abc.")
	want := []string{"https://open.spotify.com/intl-fi/track/XYZ?si=a", "https://spotify.link/abc"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %v", got)
	}
	if k, id := appleKindID(amURL); k != "track" || id != "1440650711" {
		t.Fatalf("got %s %s", k, id)
	}
	if k, id := appleKindID("https://music.apple.com/fi/album/x/1440650428"); k != "album" || id != "1440650428" {
		t.Fatalf("got %s %s", k, id)
	}
	if restGroupID("GRP=") != "group.R1JQPQ==" {
		t.Fatal("bad rest group id")
	}
}

func TestRestSendPayload(t *testing.T) {
	setup(t)
	var gotURL string
	var body map[string]any
	r := &RestTransport{post: func(u string, b []byte) error {
		gotURL = u
		return json.Unmarshal(b, &body)
	}}
	r.Send(&MsgInfo{GroupID: "GRP=", Peer: "+358401", QuoteTS: 5, QuoteAuthor: "+358401"}, "hi")
	if !strings.HasSuffix(gotURL, "/v2/send") || body["recipients"].([]any)[0] != "group.R1JQPQ==" ||
		body["quote_timestamp"].(float64) != 5 || body["number"] != bot {
		t.Fatalf("bad payload %s %v", gotURL, body)
	}
}

// End-to-end: fake signal-cli-rest-api (websocket receive + /v2/send).
func TestRestWebsocketE2E(t *testing.T) {
	setup(t)
	sent := make(chan map[string]any, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/v1/receive/"):
			c, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			frame, _ := json.Marshal(map[string]any{"account": bot, "envelope": groupMsg(amURL)})
			c.Write(r.Context(), websocket.MessageText, frame)
			<-r.Context().Done()
		case r.URL.Path == "/v2/send":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			sent <- b
			w.WriteHeader(201)
		}
	}))
	defer srv.Close()
	cfg.SignalURL = srv.URL

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&RestTransport{}).Run(ctx)
	select {
	case b := <-sent:
		if !strings.Contains(b["message"].(string), spURL) {
			t.Fatalf("bad message %v", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for send")
	}
}

// End-to-end: fake signal-cli `daemon --tcp`.
func TestJSONRPCE2E(t *testing.T) {
	setup(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	cfg.SignalHost = host
	cfg.SignalPort, _ = strconv.Atoi(port)

	got := make(chan map[string]any, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		note, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "receive",
			"params": map[string]any{"envelope": groupMsg(spURL)}})
		c.Write(append(note, '\n'))
		sc := bufio.NewScanner(c)
		if sc.Scan() {
			var m map[string]any
			json.Unmarshal(sc.Bytes(), &m)
			got <- m
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go (&JSONRPCTransport{}).Run(ctx)
	select {
	case m := <-got:
		p := m["params"].(map[string]any)
		if m["method"] != "send" || p["groupId"] != "GRP=" || !strings.Contains(p["message"].(string), amURL) {
			t.Fatalf("bad request %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for send")
	}
}

// Odesli: key goes in the query string; 401 pauses Odesli instead of retrying every lookup.
func TestOdesliKeyAnd401(t *testing.T) {
	setup(t)
	var calls int
	var gotKey string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		gotKey = r.URL.Query().Get("key")
		w.WriteHeader(status)
		if status == http.StatusOK {
			w.Write([]byte(`{"linksByPlatform":{"spotify":{"url":"` + spURL + `"}}}`))
		}
	}))
	defer srv.Close()
	old := odesliBase
	odesliBase = srv.URL
	defer func() { odesliBase = old; odesliBlock.until = time.Time{} }()

	cfg.OdesliKey = "secret123"
	if got := odesli(amURL, "spotify"); got != spURL || gotKey != "secret123" {
		t.Fatalf("got %q key %q", got, gotKey)
	}

	status = http.StatusUnauthorized
	if got := odesli(amURL, "spotify"); got != "" || !odesliBlocked() {
		t.Fatalf("401 should return empty and pause odesli (got %q)", got)
	}
	before := calls
	odesli(amURL, "spotify")
	if calls != before {
		t.Fatal("odesli should not be called while paused")
	}
}
