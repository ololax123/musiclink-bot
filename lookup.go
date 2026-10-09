package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var httpClient = &http.Client{Timeout: 20 * time.Second}

type httpError struct{ Code int }

func (e *httpError) Error() string { return fmt.Sprintf("HTTP %d", e.Code) }

// getJSON performs a request and decodes the JSON response into out (if non-nil).
func doJSON(method, u string, headers map[string]string, body io.Reader, out any) error {
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "musiclink-bot/3.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		io.Copy(io.Discard, resp.Body)
		return &httpError{resp.StatusCode}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

func resolveRedirect(u string) string {
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("User-Agent", "musiclink-bot/3.0")
	resp, err := httpClient.Do(req)
	if err != nil {
		log.Printf("could not resolve %s: %v", u, err)
		return u
	}
	resp.Body.Close()
	return resp.Request.URL.String()
}

// ---------------------------------------------------------------- Odesli

// odesli returns the link for platform ("spotify" or "appleMusic").
func odesli(link, platform string) string {
	q := url.Values{"url": {link}, "userCountry": {cfg.Country}}
	u := "https://api.song.link/v1-alpha.1/links?" + q.Encode()
	for attempt := 1; attempt <= 3; attempt++ {
		var d struct {
			LinksByPlatform map[string]struct {
				URL string `json:"url"`
			} `json:"linksByPlatform"`
		}
		err := doJSON("GET", u, nil, nil, &d)
		if err == nil {
			return d.LinksByPlatform[platform].URL
		}
		if he, ok := err.(*httpError); ok {
			if he.Code == 429 {
				time.Sleep(time.Duration(7*attempt) * time.Second)
				continue
			}
			if he.Code != 400 && he.Code != 404 {
				log.Printf("odesli: %v", err)
			}
			return ""
		}
		log.Printf("odesli: %v", err)
		return ""
	}
	return ""
}

// ---------------------------------------------------------------- Spotify API (optional fallback)

var spTok struct {
	sync.Mutex
	value string
	exp   time.Time
}

func spotifyToken() (string, error) {
	spTok.Lock()
	defer spTok.Unlock()
	if spTok.value != "" && time.Now().Before(spTok.exp.Add(-time.Minute)) {
		return spTok.value, nil
	}
	auth := base64.StdEncoding.EncodeToString([]byte(cfg.SpotifyID + ":" + cfg.SpotifySecret))
	var d struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	err := doJSON("POST", "https://accounts.spotify.com/api/token",
		map[string]string{"Authorization": "Basic " + auth, "Content-Type": "application/x-www-form-urlencoded"},
		strings.NewReader("grant_type=client_credentials"), &d)
	if err != nil {
		return "", err
	}
	spTok.value, spTok.exp = d.AccessToken, time.Now().Add(time.Duration(d.ExpiresIn)*time.Second)
	return spTok.value, nil
}

func spotifyAPI(path string, params url.Values, out any) error {
	tok, err := spotifyToken()
	if err != nil {
		return err
	}
	return doJSON("GET", "https://api.spotify.com/v1/"+path+"?"+params.Encode(),
		map[string]string{"Authorization": "Bearer " + tok}, nil, out)
}

var (
	reSingleEP  = regexp.MustCompile(`\s+-\s+(Single|EP)$`)
	reFeat      = regexp.MustCompile(`(?i)\s*[\(\[](feat\.|ft\.|with )[^\)\]]*[\)\]]`)
	reRemaster  = regexp.MustCompile(`(?i)\s+-\s+(Remaster(ed)?|\d{4} Remaster).*$`)
	reSpotifyID = regexp.MustCompile(`/(track|album|artist)/([A-Za-z0-9]+)`)
)

func cleanTitle(s string) string {
	s = reSingleEP.ReplaceAllString(s, "")
	s = reFeat.ReplaceAllString(s, "")
	s = reRemaster.ReplaceAllString(s, "")
	return strings.TrimSpace(strings.ReplaceAll(s, `"`, ""))
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// appleKindID returns ("track"|"album"|"artist", id) for an Apple Music URL.
func appleKindID(link string) (string, string) {
	p, err := url.Parse(link)
	if err != nil {
		return "", ""
	}
	if i := p.Query().Get("i"); isDigits(i) {
		return "track", i
	}
	var parts []string
	for _, s := range strings.Split(p.Path, "/") {
		if s != "" {
			parts = append(parts, s)
		}
	}
	if len(parts) == 0 || !isDigits(parts[len(parts)-1]) {
		return "", ""
	}
	last := parts[len(parts)-1]
	for _, kv := range [][2]string{{"song", "track"}, {"album", "album"}, {"artist", "artist"}} {
		for _, s := range parts {
			if s == kv[0] {
				return kv[1], last
			}
		}
	}
	return "", ""
}

func spotifyKindID(link string) (string, string) {
	m := reSpotifyID.FindStringSubmatch(link)
	if m == nil {
		return "", ""
	}
	return m[1], m[2]
}

type itunesResult map[string]any

func (r itunesResult) str(k string) string { s, _ := r[k].(string); return s }

func itunes(path string, params url.Values) (itunesResult, error) {
	var d struct {
		Results []itunesResult `json:"results"`
	}
	if err := doJSON("GET", "https://itunes.apple.com/"+path+"?"+params.Encode(), nil, nil, &d); err != nil {
		return nil, err
	}
	if len(d.Results) == 0 {
		return nil, nil
	}
	return d.Results[0], nil
}

func fallbackAM2SP(link string) string {
	if cfg.SpotifyID == "" || cfg.SpotifySecret == "" {
		return ""
	}
	kind, id := appleKindID(link)
	if kind == "" {
		return ""
	}
	meta, err := itunes("lookup", url.Values{"id": {id}, "country": {strings.ToLower(cfg.Country)}})
	if err != nil || meta == nil {
		return ""
	}
	artist := cleanTitle(meta.str("artistName"))
	var q string
	switch kind {
	case "track":
		q = fmt.Sprintf(`track:"%s" artist:"%s"`, cleanTitle(meta.str("trackName")), artist)
	case "album":
		q = fmt.Sprintf(`album:"%s" artist:"%s"`, cleanTitle(meta.str("collectionName")), artist)
	default:
		q = fmt.Sprintf(`artist:"%s"`, artist)
	}
	var res map[string]struct {
		Items []struct {
			ExternalURLs struct {
				Spotify string `json:"spotify"`
			} `json:"external_urls"`
		} `json:"items"`
	}
	err = spotifyAPI("search", url.Values{"q": {q}, "type": {kind}, "limit": {"1"}, "market": {cfg.Country}}, &res)
	if err != nil {
		log.Printf("AM->SP fallback: %v", err)
		return ""
	}
	if items := res[kind+"s"].Items; len(items) > 0 {
		return items[0].ExternalURLs.Spotify
	}
	return ""
}

func fallbackSP2AM(link string) string {
	if cfg.SpotifyID == "" || cfg.SpotifySecret == "" {
		return ""
	}
	kind, id := spotifyKindID(link)
	if kind == "" {
		return ""
	}
	var meta struct {
		Name    string `json:"name"`
		Artists []struct {
			Name string `json:"name"`
		} `json:"artists"`
	}
	if err := spotifyAPI(kind+"s/"+id, url.Values{"market": {cfg.Country}}, &meta); err != nil {
		log.Printf("SP->AM fallback: %v", err)
		return ""
	}
	var term, entity, key string
	switch kind {
	case "artist":
		term, entity, key = meta.Name, "musicArtist", "artistLinkUrl"
	case "track":
		term, entity, key = artistName(meta.Artists)+" "+cleanTitle(meta.Name), "song", "trackViewUrl"
	default:
		term, entity, key = artistName(meta.Artists)+" "+cleanTitle(meta.Name), "album", "collectionViewUrl"
	}
	r, err := itunes("search", url.Values{"term": {term}, "entity": {entity},
		"country": {strings.ToLower(cfg.Country)}, "limit": {"1"}})
	if err != nil || r == nil || r.str(key) == "" {
		return ""
	}
	out := strings.Replace(r.str(key), "itunes.apple.com", "music.apple.com", 1)
	for _, sep := range []string{"&uo=", "?uo="} {
		if i := strings.Index(out, sep); i >= 0 {
			out = out[:i]
		}
	}
	return out
}

func artistName(a []struct {
	Name string `json:"name"`
}) string {
	if len(a) == 0 {
		return ""
	}
	return a[0].Name
}

// ---------------------------------------------------------------- translate (cached)

var cache sync.Map // key: link+"\x00"+to

// translateFn is a variable so tests can replace it.
var translateFn = translate

// translate converts link to the other service; to is "spotify" or "apple".
func translate(link, to string) string {
	key := link + "\x00" + to
	if v, ok := cache.Load(key); ok {
		return v.(string)
	}
	var res string
	if to == "spotify" {
		res = odesli(link, "spotify")
		if res == "" {
			res = fallbackAM2SP(link)
		}
	} else {
		src := link
		if strings.Contains(src, "spotify.link/") {
			src = resolveRedirect(src)
		}
		res = odesli(src, "appleMusic")
		if res == "" {
			res = fallbackSP2AM(src)
		}
	}
	if res != "" {
		cache.Store(key, res)
	}
	return res
}
