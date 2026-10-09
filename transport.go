package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// receivedItem is what signal-cli-rest-api delivers (websocket frame or poll array element).
type receivedItem struct {
	Envelope *Envelope `json:"envelope"`
	Account  string    `json:"account"`
}

func dispatch(e *Envelope, t Transport) {
	if e == nil {
		return
	}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("panic handling message: %v", r)
			}
		}()
		handle(e, t)
	}()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// ================================================================ REST (bbernhard/signal-cli-rest-api)

type RestTransport struct {
	post func(u string, body []byte) error // overridable in tests
}

func (r *RestTransport) Send(info *MsgInfo, message string) error {
	recipient := info.Peer
	if info.GroupID != "" {
		recipient = restGroupID(info.GroupID)
	}
	body := map[string]any{
		"number":     cfg.SignalNumber,
		"recipients": []string{recipient},
		"message":    message,
	}
	if cfg.ReplyAsQuote && info.QuoteTS != 0 && info.QuoteAuthor != "" {
		body["quote_timestamp"] = info.QuoteTS
		body["quote_author"] = info.QuoteAuthor
	}
	b, _ := json.Marshal(body)
	post := r.post
	if post == nil {
		post = func(u string, body []byte) error {
			return doJSON("POST", u, map[string]string{"Content-Type": "application/json"}, bytes.NewReader(body), nil)
		}
	}
	return post(cfg.SignalURL+"/v2/send", b)
}

func (r *RestTransport) Run(ctx context.Context) error {
	if cfg.RestReceive == "poll" {
		return r.runPoll(ctx)
	}
	return r.runWS(ctx)
}

func (r *RestTransport) runWS(ctx context.Context) error {
	wsURL := strings.Replace(strings.Replace(cfg.SignalURL, "https://", "wss://", 1), "http://", "ws://", 1)
	wsURL += "/v1/receive/" + url.PathEscape(cfg.SignalNumber)
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		log.Printf("connecting websocket %s", wsURL)
		conn, _, err := websocket.Dial(ctx, wsURL, nil)
		if err != nil {
			log.Printf("websocket: %v", err)
		} else {
			conn.SetReadLimit(4 << 20)
			log.Print("connected, listening…")
			backoff = 2 * time.Second
			for {
				_, data, err := conn.Read(ctx)
				if err != nil {
					log.Printf("websocket closed: %v", err)
					break
				}
				var item receivedItem
				if json.Unmarshal(data, &item) != nil {
					debugf("non-JSON frame: %.200s", data)
					continue
				}
				dispatch(item.Envelope, r)
			}
			conn.CloseNow()
		}
		if !sleepCtx(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, time.Minute)
	}
	return ctx.Err()
}

func (r *RestTransport) runPoll(ctx context.Context) error {
	u := cfg.SignalURL + "/v1/receive/" + url.PathEscape(cfg.SignalNumber) + "?timeout=10"
	log.Printf("polling %s", u)
	client := &http.Client{Timeout: 40 * time.Second}
	for ctx.Err() == nil {
		req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
		resp, err := client.Do(req)
		if err != nil {
			log.Printf("poll: %v", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		var items []receivedItem
		data, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("poll: HTTP %d %.200s", resp.StatusCode, data)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if err := json.Unmarshal(data, &items); err != nil {
			debugf("poll decode: %v", err)
		}
		for _, it := range items {
			dispatch(it.Envelope, r)
		}
	}
	return ctx.Err()
}

// ================================================================ native signal-cli JSON-RPC over TCP

type JSONRPCTransport struct {
	mu   sync.Mutex
	conn net.Conn
	id   int
}

func (j *JSONRPCTransport) Send(info *MsgInfo, message string) error {
	params := map[string]any{"message": message}
	if info.GroupID != "" {
		params["groupId"] = info.GroupID
	} else {
		params["recipient"] = []string{info.Peer}
	}
	if cfg.ReplyAsQuote && info.QuoteTS != 0 && info.QuoteAuthor != "" {
		params["quoteTimestamp"] = info.QuoteTS
		params["quoteAuthor"] = info.QuoteAuthor
	}
	if cfg.SignalNumber != "" {
		params["account"] = cfg.SignalNumber
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.conn == nil {
		return fmt.Errorf("not connected")
	}
	j.id++
	line, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": "send", "params": params, "id": strconv.Itoa(j.id)})
	_, err := j.conn.Write(append(line, '\n'))
	return err
}

func (j *JSONRPCTransport) Run(ctx context.Context) error {
	addr := net.JoinHostPort(cfg.SignalHost, strconv.Itoa(cfg.SignalPort))
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		log.Printf("connecting to signal-cli %s", addr)
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			log.Printf("connect: %v", err)
		} else {
			j.mu.Lock()
			j.conn = conn
			j.mu.Unlock()
			log.Print("connected, listening…")
			backoff = 2 * time.Second
			go func() { <-ctx.Done(); conn.Close() }()

			sc := bufio.NewScanner(conn)
			sc.Buffer(make([]byte, 64*1024), 8<<20)
			for sc.Scan() {
				var msg struct {
					Method string `json:"method"`
					Params struct {
						Envelope *Envelope `json:"envelope"`
					} `json:"params"`
					Error json.RawMessage `json:"error"`
				}
				if json.Unmarshal(sc.Bytes(), &msg) != nil {
					continue
				}
				if msg.Method == "receive" {
					dispatch(msg.Params.Envelope, j)
				} else if len(msg.Error) > 0 {
					log.Printf("signal-cli error: %s", msg.Error)
				}
			}
			log.Printf("connection closed: %v", sc.Err())
			j.mu.Lock()
			j.conn = nil
			j.mu.Unlock()
			conn.Close()
		}
		if !sleepCtx(ctx, backoff) {
			break
		}
		backoff = min(backoff*2, time.Minute)
	}
	return ctx.Err()
}
