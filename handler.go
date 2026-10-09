package main

import (
	"encoding/base64"
	"log"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	Marker   = "⁣" // invisible separator; starts every bot reply
	PrefixSP = "🎧 Spotify: "
	PrefixAM = "🍎 Apple Music: "
	trailing = ".,;:!?)]}>'\""
)

var (
	appleRe   = regexp.MustCompile(`(?i)https?://(?:geo\.|embed\.)?(?:music|itunes)\.apple\.com/[^\s<>"']+`)
	spotifyRe = regexp.MustCompile(`(?i)https?://(?:open\.spotify\.com/(?:intl-[a-z]{2}(?:-[a-z]{2})?/)?(?:track|album|artist)/[A-Za-z0-9]+|spotify\.link/[A-Za-z0-9]+)[^\s<>"']*`)
)

// ---------------------------------------------------------------- envelope types (same for both backends)

type GroupInfo struct {
	GroupID string `json:"groupId"`
}

type DataMessage struct {
	Timestamp int64      `json:"timestamp"`
	Message   string     `json:"message"`
	GroupInfo *GroupInfo `json:"groupInfo"`
}

type SentMessage struct {
	DataMessage
	Destination       string `json:"destination"`
	DestinationNumber string `json:"destinationNumber"`
	DestinationUUID   string `json:"destinationUuid"`
}

type Envelope struct {
	Source       string       `json:"source"`
	SourceNumber string       `json:"sourceNumber"`
	SourceUUID   string       `json:"sourceUuid"`
	Timestamp    int64        `json:"timestamp"`
	DataMessage  *DataMessage `json:"dataMessage"`
	SyncMessage  *struct {
		SentMessage *SentMessage `json:"sentMessage"`
	} `json:"syncMessage"`
}

type MsgInfo struct {
	Text        string
	GroupID     string // internal signal-cli group id ("" for 1:1)
	Peer        string
	QuoteTS     int64
	QuoteAuthor string
	Own         bool
}

func firstNonEmpty(s ...string) string {
	for _, v := range s {
		if v != "" {
			return v
		}
	}
	return ""
}

func extract(e *Envelope) *MsgInfo {
	src := firstNonEmpty(e.SourceNumber, e.SourceUUID, e.Source)
	if dm := e.DataMessage; dm != nil {
		info := &MsgInfo{Text: dm.Message, Peer: src, QuoteAuthor: src,
			QuoteTS: firstTS(dm.Timestamp, e.Timestamp)}
		if dm.GroupInfo != nil {
			info.GroupID = dm.GroupInfo.GroupID
		}
		return info
	}
	if e.SyncMessage != nil && e.SyncMessage.SentMessage != nil {
		sm := e.SyncMessage.SentMessage
		info := &MsgInfo{Text: sm.Message, QuoteAuthor: src, Own: true,
			Peer:    firstNonEmpty(sm.DestinationNumber, sm.DestinationUUID, sm.Destination),
			QuoteTS: firstTS(sm.Timestamp, e.Timestamp)}
		if sm.GroupInfo != nil {
			info.GroupID = sm.GroupInfo.GroupID
		}
		if info.GroupID == "" && info.Peer == "" {
			return nil
		}
		return info
	}
	return nil
}

func firstTS(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

// restGroupID: signal-cli-rest-api addresses groups as "group." + base64(internal id).
func restGroupID(internal string) string {
	return "group." + base64.StdEncoding.EncodeToString([]byte(internal))
}

func chatAllowed(info *MsgInfo) bool {
	if len(cfg.AllowedChats) == 0 {
		return true
	}
	if cfg.AllowedChats[info.Peer] && info.GroupID == "" {
		return true
	}
	return info.GroupID != "" && (cfg.AllowedChats[info.GroupID] || cfg.AllowedChats[restGroupID(info.GroupID)])
}

func findLinks(re *regexp.Regexp, text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range re.FindAllString(text, -1) {
		m = strings.TrimRight(m, trailing)
		if !seen[m] {
			seen[m] = true
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------- loop protection

var posted = struct {
	sync.Mutex
	m map[string]time.Time
}{m: map[string]time.Time{}}

func normURL(u string) string {
	for _, sep := range []string{"?si=", "&si="} {
		if i := strings.Index(u, sep); i >= 0 {
			u = u[:i]
		}
	}
	return strings.ToLower(strings.TrimRight(u, "/"))
}

func rememberPosted(u string) {
	posted.Lock()
	defer posted.Unlock()
	posted.m[normURL(u)] = time.Now().Add(time.Duration(cfg.LoopTTLSeconds) * time.Second)
}

func wasPosted(u string) bool {
	posted.Lock()
	defer posted.Unlock()
	now := time.Now()
	for k, exp := range posted.m {
		if exp.Before(now) {
			delete(posted.m, k)
		}
	}
	_, ok := posted.m[normURL(u)]
	return ok
}

func isBotMessage(text string) bool {
	t := strings.TrimLeft(text, " \t\r\n")
	return strings.HasPrefix(t, Marker) ||
		strings.HasPrefix(t, strings.TrimSpace(PrefixSP)) ||
		strings.HasPrefix(t, strings.TrimSpace(PrefixAM))
}

// ---------------------------------------------------------------- handler

func handle(e *Envelope, t Transport) {
	info := extract(e)
	if info == nil || info.Text == "" {
		return
	}
	if info.Own && !cfg.TranslateOwn {
		return
	}
	if !chatAllowed(info) {
		debugf("ignoring chat %s/%s", info.GroupID, info.Peer)
		return
	}
	if isBotMessage(info.Text) {
		debugf("skipping bot message")
		return
	}

	allAM := findLinks(appleRe, info.Text)
	allSP := findLinks(spotifyRe, info.Text)
	if len(allAM) > 0 && len(allSP) > 0 {
		return // already has both
	}

	type job struct{ url, to, prefix string }
	var jobs []job
	if cfg.Directions == "both" || cfg.Directions == "am2sp" {
		for _, u := range allAM {
			if !wasPosted(u) {
				jobs = append(jobs, job{u, "spotify", PrefixSP})
			}
		}
	}
	if cfg.Directions == "both" || cfg.Directions == "sp2am" {
		for _, u := range allSP {
			if !wasPosted(u) {
				jobs = append(jobs, job{u, "apple", PrefixAM})
			}
		}
	}
	if len(jobs) == 0 {
		return
	}

	var lines, newURLs []string
	for _, j := range jobs {
		res := translateFn(j.url, j.to)
		log.Printf("%s -> %s", j.url, res)
		if res != "" {
			lines = append(lines, j.prefix+res)
			newURLs = append(newURLs, res)
		} else if cfg.ReplyNotFound {
			lines = append(lines, j.prefix+"no match found 🤷")
		}
	}
	if len(lines) == 0 {
		return
	}
	for _, u := range newURLs {
		rememberPosted(u)
	}
	if err := t.Send(info, Marker+strings.Join(lines, "\n")); err != nil {
		log.Printf("send failed: %v", err)
	}
}
