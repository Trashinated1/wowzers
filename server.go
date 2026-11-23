package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/bogem/id3v2/v2"
	"github.com/go-flac/flacpicture"
	"github.com/go-flac/flacvorbis"
	flac "github.com/go-flac/go-flac"

	dz "github.com/Cycl0o0/OpenDeezer/v3/sdk/deezer"
)

// ---------- public types ----------

type Track struct {
	ID         string `json:"id"`
	Title      string `json:"title"`
	Artist     string `json:"artist"`
	ArtistID   string `json:"artistId"`
	Album      string `json:"album"`
	AlbumID    string `json:"albumId"`
	DurationMS int64  `json:"durationMs"`
	Cover      string `json:"cover"`
	Explicit   bool   `json:"explicit,omitempty"`
}

type Playlist struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	NbTracks int    `json:"nbTracks"`
	Cover    string `json:"cover"`
}

type Artist struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	NbFans  int    `json:"nbFans"`
	NbAlbum int    `json:"nbAlbums"`
	Cover   string `json:"cover"`
}

type Lyrics struct {
	Synced   []LyricLine `json:"synced,omitempty"`
	Unsynced []string    `json:"unsynced,omitempty"`
}

type LyricLine struct {
	TimeMS int64  `json:"timeMs"`
	Text   string `json:"text"`
}

// ---------- Deezer public REST DTOs (full multi-artist shape) ----------
//
// The OpenDeezer SDK's browse DTOs only parse the primary `artist` field and
// drop the `contributors` array, so tracks credited to several artists lose
// every credit except the first one (and on some listings the row renders
// with no artist at all). These DTOs hit the same public REST endpoints
// directly and keep the complete contributor list.

const dzRestBase = "https://api.deezer.com"
const dzUserAgent = "Mozilla/5.0 OpenDeezer/0.1"

var dzHTTP = &http.Client{Timeout: 30 * time.Second}

type dzRestArtist struct {
	ID   json.Number `json:"id"`
	Name string      `json:"name"`
}

type dzRestAlbum struct {
	ID          json.Number `json:"id"`
	Title       string      `json:"title"`
	CoverXL     string      `json:"cover_xl"`
	CoverBig    string      `json:"cover_big"`
	CoverMedium string      `json:"cover_medium"`
}

type dzRestTrack struct {
	ID           json.Number    `json:"id"`
	Title        string         `json:"title"`
	TitleShort   string         `json:"title_short"`
	Duration     json.Number    `json:"duration"`
	Artist       dzRestArtist   `json:"artist"`
	Contributors []dzRestArtist `json:"contributors"`
	Album        dzRestAlbum    `json:"album"`
	ExplicitLyrc bool           `json:"explicit_lyrics"`
	TrackPos     int            `json:"track_position"`
	ReleaseDate  string         `json:"release_date"`
}

// dzRestTrackFull is /track/{id} — includes album cover_xl, track position
// and release date, which the search listing omits.
type dzRestTrackFull struct {
	dzRestTrack
}

type dzRestAlbumFull struct {
	ID           json.Number    `json:"id"`
	Title        string         `json:"title"`
	Artist       dzRestArtist   `json:"artist"`
	Contributors []dzRestArtist `json:"contributors"`
	CoverXL      string         `json:"cover_xl"`
	CoverBig     string         `json:"cover_big"`
	CoverMedium  string         `json:"cover_medium"`
	ReleaseDate  string         `json:"release_date"`
	Tracks       struct {
		Data []dzRestTrack `json:"data"`
	} `json:"tracks"`
}

type dzRestArtistFull struct {
	ID            json.Number `json:"id"`
	Name          string      `json:"name"`
	NbFan         int         `json:"nb_fan"`
	PictureXL     string      `json:"picture_xl"`
	PictureBig    string      `json:"picture_big"`
	PictureMedium string      `json:"picture_medium"`
	NbAlbum       int         `json:"nb_album"`
}

// joinArtists returns the full multi-artist line plus the primary artist ID.
// Deezer's `contributors` array lists every credited artist in order (primary
// first). When the array is missing or empty, the primary `artist` object is
// used. An empty result maps to "Unknown Artist" so the row always renders.
func joinArtists(primary dzRestArtist, contributors []dzRestArtist) (string, string) {
	names := make([]string, 0, len(contributors)+1)
	primaryID := ""
	if len(contributors) > 0 {
		seen := map[string]bool{}
		for _, a := range contributors {
			n := strings.TrimSpace(a.Name)
			if n == "" || seen[n] {
				continue
			}
			seen[n] = true
			names = append(names, n)
			if primaryID == "" {
				primaryID = a.ID.String()
			}
		}
	}
	if len(names) == 0 && strings.TrimSpace(primary.Name) != "" {
		names = append(names, strings.TrimSpace(primary.Name))
		primaryID = primary.ID.String()
	}
	if len(names) == 0 {
		return "Unknown Artist", ""
	}
	return strings.Join(names, ", "), primaryID
}

func pickCover(a dzRestAlbum) string {
	if a.CoverXL != "" {
		return a.CoverXL
	}
	if a.CoverBig != "" {
		return a.CoverBig
	}
	return a.CoverMedium
}

func trackFromRest(r dzRestTrack) Track {
	durMS, _ := r.Duration.Int64()
	artist, artistID := joinArtists(r.Artist, r.Contributors)
	return Track{
		ID:         r.ID.String(),
		Title:      r.Title,
		Artist:     artist,
		ArtistID:   artistID,
		Album:      r.Album.Title,
		AlbumID:    r.Album.ID.String(),
		DurationMS: durMS * 1000,
		Cover:      pickCover(r.Album),
		Explicit:   r.ExplicitLyrc,
	}
}

// ---------- server ----------

type Server struct {
	addr   string
	mu     sync.Mutex
	client *dz.Client
}

func NewServer(addr, arl, quality string) (*Server, error) {
	s := &Server{addr: addr}
	c := dz.New(arl)
	if err := c.Login(); err != nil {
		return nil, fmt.Errorf("deezer login failed: %w", err)
	}

	// Apply quality setting. "low" and "normal" both map to 128kbps MP3.
	switch quality {
	case "low", "normal":
		c.SetQuality(dz.QualityNormal)
	case "lossless":
		c.SetQuality(dz.QualityLossless)
	default:
		c.SetQuality(dz.QualityHigh)
	}

	s.client = c
	return s, nil
}

func (s *Server) Run() error {
	mux := http.NewServeMux()

	mux.Handle("GET /", http.FileServer(http.FS(webRoot)))

	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/charts", s.handleCharts)
	mux.HandleFunc("GET /api/favorites", s.handleFavorites)
	mux.HandleFunc("GET /api/playlists", s.handlePlaylists)
	mux.HandleFunc("GET /api/playlist/{id}", s.handlePlaylist)
	mux.HandleFunc("GET /api/artist/{id}", s.handleArtist)
	mux.HandleFunc("GET /api/album/{id}", s.handleAlbum)
	mux.HandleFunc("GET /api/lyrics/{id}", s.handleLyrics)
	mux.HandleFunc("GET /api/stream/{id}", s.handleStream)
	mux.HandleFunc("GET /api/download/{id}", s.handleDownload)

	return http.ListenAndServe(s.addr, s.withCORS(mux))
}

func (s *Server) withCORS(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(204)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// ---------- Deezer public REST helpers ----------

// restGet calls a public Deezer REST endpoint (no ARL needed). Failures are
// reported as 200 bodies with an {"error":{...}} envelope, surfaced here as
// Go errors like the SDK's own restGet.
func restGet(path string) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, dzRestBase+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dzUserAgent)
	resp, err := dzHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("deezer rest %s: HTTP %d", path, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env struct {
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error != nil &&
		(env.Error.Type != "" || env.Error.Message != "" || env.Error.Code != 0) {
		msg := env.Error.Message
		if msg == "" {
			msg = env.Error.Type
		}
		return nil, fmt.Errorf("deezer rest %s: %s (code %d)", path, msg, env.Error.Code)
	}
	return b, nil
}

// ---------- generic helpers ----------

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func toMap(v any) map[string]any {
	if v == nil {
		return map[string]any{}
	}
	raw, _ := json.Marshal(v)
	var m map[string]any
	json.Unmarshal(raw, &m)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func strVal(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
			if f, ok := v.(float64); ok {
				return strconv.Itoa(int(f))
			}
		}
	}
	return ""
}

func intVal(m map[string]any, keys ...string) int {
	if m == nil {
		return 0
	}
	for _, k := range keys {
		if v, ok := m[k]; ok && v != nil {
			if f, ok := v.(float64); ok {
				return int(f)
			}
			if i, ok := v.(int); ok {
				return i
			}
			if s, ok := v.(string); ok {
				if n, err := strconv.Atoi(s); err == nil {
					return n
				}
			}
		}
	}
	return 0
}

// ---------- SDK-based track mapping (favorites / playlists / gw lists) ----------
//
// dz.Track has no JSON tags, so marshaling yields capitalized keys
// ("Artists", "AlbumName", "ArtworkURL"). The old code checked lowercase
// keys that never existed, silently dropping album/cover/artistID. Read the
// struct fields directly instead.

func trackFromSDK(t dz.Track) Track {
	tr := Track{
		ID:         t.ID,
		Title:      t.Name,
		DurationMS: t.DurationMS,
		Artist:     t.ArtistLine(),
		Album:      t.AlbumName,
		Cover:      t.ArtworkURL,
		Explicit:   t.Explicit,
	}
	if len(t.Artists) > 0 {
		tr.ArtistID = t.Artists[0].ID
	}
	if tr.Artist == "" {
		tr.Artist = "Unknown Artist"
	}
	return tr
}

// ---------- handlers: browse (direct REST, multi-artist aware) ----------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	q := strings.TrimSpace(r.URL.Query().Get("q"))
	enc := url.QueryEscape(q)

	tracks := make([]Track, 0, 40)
	albums := make([]Playlist, 0, 20)
	artists := make([]Artist, 0, 20)

	// Tracks — full contributor list per track.
	if b, err := restGet("/search?q=" + enc + "&limit=40"); err == nil {
		var res struct {
			Data []dzRestTrack `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, t := range res.Data {
				tracks = append(tracks, trackFromRest(t))
			}
		}
	} else if s.client != nil {
		// Fallback: SDK search (primary artist only).
		log.Printf("rest search failed (%v) — falling back to SDK search", err)
		if res, serr := s.client.Search(q); serr == nil && res.Tracks != nil {
			for _, t := range res.Tracks {
				tracks = append(tracks, trackFromSDK(t))
			}
		}
	}

	// Albums.
	if b, err := restGet("/search/album?q=" + enc + "&limit=20"); err == nil {
		var res struct {
			Data []struct {
				ID     json.Number  `json:"id"`
				Title  string       `json:"title"`
				Artist dzRestArtist `json:"artist"`
				dzRestAlbum
				NbTracks int `json:"nb_tracks"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, a := range res.Data {
				albums = append(albums, Playlist{
					ID:       a.ID.String(),
					Title:    a.Title,
					NbTracks: a.NbTracks,
					Cover:    pickCover(a.dzRestAlbum),
				})
			}
		}
	}

	// Artists.
	if b, err := restGet("/search/artist?q=" + enc + "&limit=20"); err == nil {
		var res struct {
			Data []dzRestArtistFull `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, a := range res.Data {
				cover := a.PictureXL
				if cover == "" {
					cover = a.PictureBig
				}
				if cover == "" {
					cover = a.PictureMedium
				}
				artists = append(artists, Artist{
					ID:     a.ID.String(),
					Name:   a.Name,
					NbFans: a.NbFan,
					Cover:  cover,
				})
			}
		}
	}

	writeJSON(w, map[string]any{"tracks": tracks, "albums": albums, "artists": artists})
}

func (s *Server) handleCharts(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tracks := make([]Track, 0, 40)
	if b, err := restGet("/chart/0/tracks?limit=40"); err == nil {
		var res struct {
			Data []dzRestTrack `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, t := range res.Data {
				tracks = append(tracks, trackFromRest(t))
			}
		}
	} else if s.client != nil {
		if res, serr := s.client.Charts("0"); serr == nil && res.Tracks != nil {
			for _, t := range res.Tracks {
				tracks = append(tracks, trackFromSDK(t))
			}
		}
	}
	writeJSON(w, map[string]any{"tracks": tracks})
}

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := r.PathValue("id")
	b, err := restGet("/album/" + url.PathEscape(id))
	if err != nil {
		// Fallback to the SDK (loses album header metadata but still lists tracks).
		if s.client != nil {
			if al, serr := s.client.AlbumTracks(id); serr == nil {
				ts := make([]Track, 0, len(al))
				for _, t := range al {
					ts = append(ts, trackFromSDK(t))
				}
				writeJSON(w, map[string]any{"tracks": ts, "nbTracks": len(ts)})
				return
			}
		}
		http.Error(w, err.Error(), 502)
		return
	}

	var alb dzRestAlbumFull
	if err := json.Unmarshal(b, &alb); err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	albumArtist, _ := joinArtists(alb.Artist, alb.Contributors)
	ts := make([]Track, 0, len(alb.Tracks.Data))
	for _, t := range alb.Tracks.Data {
		// Album-track listings omit the `album` object — inject this album's
		// metadata into every track so covers and album names are complete.
		t.Album = dzRestAlbum{ID: alb.ID, Title: alb.Title,
			CoverXL: alb.CoverXL, CoverBig: alb.CoverBig, CoverMedium: alb.CoverMedium}
		ts = append(ts, trackFromRest(t))
	}

	writeJSON(w, map[string]any{
		"title":    alb.Title,
		"cover":    pickCover(dzRestAlbum{CoverXL: alb.CoverXL, CoverBig: alb.CoverBig, CoverMedium: alb.CoverMedium}),
		"artist":   albumArtist,
		"tracks":   ts,
		"nbTracks": len(ts),
		"year":     yearOf(alb.ReleaseDate),
	})
}

func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id := url.PathEscape(r.PathValue("id"))

	var prof dzRestArtistFull
	if b, err := restGet("/artist/" + id); err == nil {
		json.Unmarshal(b, &prof)
	}
	if prof.Name == "" && s.client != nil {
		// Fallback to the SDK profile (no contributors available there).
		if page, serr := s.client.ArtistProfile(id); serr == nil {
			top := make([]Track, 0, len(page.Top))
			for _, t := range page.Top {
				top = append(top, trackFromSDK(t))
			}
			alb := make([]Playlist, 0, len(page.Albums))
			for _, a := range page.Albums {
				alb = append(alb, Playlist{ID: a.ID, Title: a.Name, Cover: a.ArtworkURL})
			}
			cover := page.Artist.ArtworkURL
			writeJSON(w, map[string]any{
				"artist":    Artist{ID: page.Artist.ID, Name: page.Artist.Name, NbFans: page.Artist.NbFans, Cover: cover},
				"topTracks": top,
				"albums":    alb,
			})
			return
		}
	}

	cover := prof.PictureXL
	if cover == "" {
		cover = prof.PictureBig
	}
	if cover == "" {
		cover = prof.PictureMedium
	}

	// Top tracks — full contributor parsing.
	top := make([]Track, 0, 25)
	if b, err := restGet("/artist/" + id + "/top?limit=25"); err == nil {
		var res struct {
			Data []dzRestTrack `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, t := range res.Data {
				top = append(top, trackFromRest(t))
			}
		}
	}

	// Albums.
	albums := make([]Playlist, 0, 50)
	if b, err := restGet("/artist/" + id + "/albums?limit=50"); err == nil {
		var res struct {
			Data []struct {
				ID          json.Number `json:"id"`
				Title       string      `json:"title"`
				CoverXL     string      `json:"cover_xl"`
				CoverBig    string      `json:"cover_big"`
				CoverMedium string      `json:"cover_medium"`
				ReleaseDate string      `json:"release_date"`
			} `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, a := range res.Data {
				albums = append(albums, Playlist{
					ID:    a.ID.String(),
					Title: a.Title,
					Cover: pickCover(dzRestAlbum{CoverXL: a.CoverXL, CoverBig: a.CoverBig, CoverMedium: a.CoverMedium}),
				})
			}
		}
	}

	writeJSON(w, map[string]any{
		"artist":    Artist{ID: prof.ID.String(), Name: prof.Name, NbFans: prof.NbFan, NbAlbum: prof.NbAlbum, Cover: cover},
		"topTracks": top,
		"albums":    albums,
	})
}

// ---------- handlers: auth-gated (SDK) ----------

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tracks, err := s.client.Favorites()
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	ts := make([]Track, 0, len(tracks))
	for _, t := range tracks {
		ts = append(ts, trackFromSDK(t))
	}
	writeJSON(w, ts)
}

func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pl, err := s.client.Playlists()
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	// SDK Playlist fields: Name, TrackCount, ArtworkURL.
	out := make([]Playlist, 0, len(pl))
	for _, item := range pl {
		out = append(out, Playlist{
			ID:       item.ID,
			Title:    item.Name,
			NbTracks: item.TrackCount,
			Cover:    item.ArtworkURL,
		})
	}
	writeJSON(w, out)
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	pl, err := s.client.PlaylistTracks(id)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	var cover string
	ts := make([]Track, 0, len(pl))
	for _, t := range pl {
		if cover == "" {
			cover = t.ArtworkURL
		}
		ts = append(ts, trackFromSDK(t))
	}
	writeJSON(w, map[string]any{
		"title": "Playlist", "cover": cover, "tracks": ts, "nbTracks": len(ts),
	})
}

func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")
	lyr, err := s.client.Lyrics(id)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	out := Lyrics{Unsynced: []string{}}
	if lyr.IsSynced() {
		for _, l := range lyr.Synced {
			out.Synced = append(out.Synced, LyricLine{TimeMS: l.TimeMS, Text: l.Text})
		}
	} else if strings.TrimSpace(lyr.Plain) != "" {
		// FIX: the SDK exposes unsynced lyrics as `Plain` — the old code read
		// a nonexistent "unsynced" map key, so plain lyrics never rendered.
		for _, line := range strings.Split(lyr.Plain, "\n") {
			line = strings.TrimRight(line, "\r")
			out.Unsynced = append(out.Unsynced, line)
		}
	}
	writeJSON(w, out)
}

// ---------- handlers: stream + download ----------

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")

	plan, err := s.client.PrepareStream(id)
	if err != nil {
		// FALLBACK: retry at 128kbps if the preferred quality fails.
		log.Printf("stream %s: prepare failed — falling back to 128kbps", id)
		s.client.SetQuality(dz.QualityNormal)
		plan, err = s.client.PrepareStream(id)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
	}

	// Download into memory to support Range requests (fixes broken pipes).
	buf := new(bytes.Buffer)
	if err := dz.DownloadTrack(plan, buf); err != nil {
		log.Printf("stream %s: %v", id, err)
		http.Error(w, "Failed to download track", 500)
		return
	}

	// Dynamically set Content-Type based on the format returned by Deezer.
	label := dz.FormatLabel(plan.Format)
	contentType := "audio/mpeg"
	if strings.Contains(label, "FLAC") {
		contentType = "audio/flac"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Now(), bytes.NewReader(buf.Bytes()))
}

// handleDownload streams the track as a tagged MP3/FLAC file with the cover
// art and full metadata (title, every credited artist, album, year, track
// number) embedded, ready to be saved straight into a music library.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")

	// 1. Full metadata via public REST /track/{id} (includes contributors,
	//    track_position, release_date and the album's cover_xl).
	title, artist, album, year := "", "", "", ""
	trackNum := 0
	coverURL := ""
	if b, err := restGet("/track/" + url.PathEscape(id)); err == nil {
		var m struct {
			dzRestTrack
		}
		if json.Unmarshal(b, &m) == nil {
			title = m.Title
			artist, _ = joinArtists(m.Artist, m.Contributors)
			album = m.Album.Title
			coverURL = pickCover(m.Album)
			trackNum = m.TrackPos
			year = yearOf(m.ReleaseDate)
		}
	}
	// Fallback metadata via the SDK (primary artist only).
	if title == "" && s.client != nil {
		if t, terr := s.client.Track(id); terr == nil {
			title = t.Name
			artist = t.ArtistLine()
			album = t.AlbumName
			coverURL = t.ArtworkURL
		}
	}
	if title == "" {
		http.Error(w, "track not found", 404)
		return
	}
	if artist == "" {
		artist = "Unknown Artist"
	}
	if album == "" {
		album = "Unknown Album"
	}

	// 2. Audio bytes via the SDK (auth + decryption), with 128kbps fallback.
	plan, err := s.client.PrepareStream(id)
	if err != nil {
		s.client.SetQuality(dz.QualityNormal)
		plan, err = s.client.PrepareStream(id)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
	}
	audio := new(bytes.Buffer)
	if err := dz.DownloadTrack(plan, audio); err != nil {
		log.Printf("download %s: %v", id, err)
		s.client.SetQuality(dz.QualityNormal)
		plan, err = s.client.PrepareStream(id)
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}
		audio = new(bytes.Buffer)
		if err := dz.DownloadTrack(plan, audio); err != nil {
			http.Error(w, "download failed", 500)
			return
		}
	}

	// 3. Cover art (best-effort — a missing image never fails the download).
	var art []byte
	if coverURL != "" {
		if ab, aerr := fetchArtwork(coverURL); aerr == nil && len(ab) > 0 {
			art = ab
		} else if aerr != nil {
			log.Printf("download %s: artwork fetch failed (non-fatal): %v", id, aerr)
		}
	}

	// 4. Tag with metadata. On any tagging error the raw audio bytes are
	//    served untouched rather than a corrupted file.
	isFLAC := strings.Contains(strings.ToUpper(plan.Format), "FLAC")
	ext := ".mp3"
	var final []byte
	var tagErr error
	if isFLAC {
		ext = ".flac"
		final, tagErr = tagFLACBytes(audio.Bytes(), title, artist, album, year, trackNum, art)
	} else {
		final, tagErr = tagMP3Bytes(audio.Bytes(), title, artist, album, year, trackNum, art)
	}
	if tagErr != nil {
		log.Printf("download %s: tagging failed (serving raw audio): %v", id, tagErr)
		final = audio.Bytes()
	}

	// 5. Serve as an attachment with a descriptive filename.
	fname := sanitizeFilename(artist+" - "+title) + ext
	asciiName := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '_'
		}
		return r
	}, fname)

	w.Header().Set("Content-Type", contentTypeFor(ext))
	w.Header().Set("Content-Disposition",
		`attachment; filename="`+asciiName+`"; filename*=UTF-8''`+url.PathEscape(fname))
	w.Header().Set("Content-Length", strconv.Itoa(len(final)))
	w.Write(final)
}

// ---------- tagging (MP3 / FLAC) ----------

func contentTypeFor(ext string) string {
	if strings.EqualFold(ext, ".flac") {
		return "audio/flac"
	}
	return "audio/mpeg"
}

// sniffImageMIME detects the image format from magic bytes so embedded cover
// art is always labeled correctly (Deezer covers are JPEG, but PNG is
// tolerated). Returns "" for anything unsupported.
func sniffImageMIME(art []byte) string {
	if len(art) >= 3 && art[0] == 0xFF && art[1] == 0xD8 && art[2] == 0xFF {
		return "image/jpeg"
	}
	if len(art) >= 8 && art[0] == 0x89 && art[1] == 'P' && art[2] == 'N' && art[3] == 'G' {
		return "image/png"
	}
	return ""
}

// tagMP3Bytes prepends an ID3v2.4 frame block (title, all artists, album,
// year, track number, embedded front cover) to raw MP3 audio bytes.
func tagMP3Bytes(mp3 []byte, title, artist, album, year string, trackNum int, art []byte) ([]byte, error) {
	tag := id3v2.NewEmptyTag()
	tag.SetDefaultEncoding(id3v2.EncodingUTF8)
	tag.SetTitle(title)
	tag.SetArtist(artist)
	tag.SetAlbum(album)
	if year != "" {
		tag.SetYear(year)
	}
	if trackNum > 0 {
		id := tag.CommonID("Track number/Position in set")
		tag.AddTextFrame(id, id3v2.EncodingUTF8, strconv.Itoa(trackNum))
	}
	if len(art) > 0 {
		if mime := sniffImageMIME(art); mime != "" {
			tag.AddAttachedPicture(id3v2.PictureFrame{
				Encoding:    id3v2.EncodingUTF8,
				MimeType:    mime,
				PictureType: id3v2.PTFrontCover,
				Description: "Cover",
				Picture:     art,
			})
		}
	}
	if !tag.HasFrames() {
		return mp3, nil
	}
	var out bytes.Buffer
	if _, err := tag.WriteTo(&out); err != nil {
		return nil, fmt.Errorf("id3v2 write: %w", err)
	}
	out.Write(mp3)
	return out.Bytes(), nil
}

// tagFLACBytes rewrites the FLAC metadata blocks with vorbis comments and an
// embedded front cover, preserving StreamInfo (which must remain first).
func tagFLACBytes(data []byte, title, artist, album, year string, trackNum int, art []byte) ([]byte, error) {
	f, err := flac.ParseBytes(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("flac parse: %w", err)
	}

	newMeta := make([]*flac.MetaDataBlock, 0, len(f.Meta)+2)
	if len(f.Meta) > 0 {
		newMeta = append(newMeta, f.Meta[0]) // StreamInfo first
	}
	for _, m := range f.Meta[1:] {
		if m.Type != flac.VorbisComment && m.Type != flac.Picture {
			newMeta = append(newMeta, m)
		}
	}

	cmts := flacvorbis.New()
	cmts.Add(flacvorbis.FIELD_TITLE, title)
	cmts.Add(flacvorbis.FIELD_ARTIST, artist)
	cmts.Add(flacvorbis.FIELD_ALBUM, album)
	if trackNum > 0 {
		cmts.Add(flacvorbis.FIELD_TRACKNUMBER, strconv.Itoa(trackNum))
	}
	if year != "" {
		cmts.Add(flacvorbis.FIELD_DATE, year)
	}
	cm := cmts.Marshal()
	newMeta = append(newMeta, &cm)

	if len(art) > 0 {
		if mime := sniffImageMIME(art); mime != "" {
			// Prefer NewFromImageData: it decodes the image to fill in the
			// width/height/depth fields players like to read. If decoding
			// fails (unusual encoder output), embed the picture anyway with
			// zero dimensions rather than losing the cover art.
			if p, perr := flacpicture.NewFromImageData(flacpicture.PictureTypeFrontCover, "Cover", art, mime); perr == nil {
				pm := p.Marshal()
				newMeta = append(newMeta, &pm)
			} else {
				log.Printf("download: artwork decode failed (%v) — embedding unmeasured", perr)
				p := flacpicture.MetadataBlockPicture{
					PictureType: flacpicture.PictureTypeFrontCover,
					Description: "Cover",
					MIME:        mime,
					ImageData:   art,
				}
				pm := p.Marshal()
				newMeta = append(newMeta, &pm)
			}
		}
	}

	f.Meta = newMeta
	return f.Marshal(), nil
}

// fetchArtwork downloads embedded cover art bytes (best-effort).
func fetchArtwork(artURL string) ([]byte, error) {
	if artURL == "" {
		return nil, nil
	}
	req, err := http.NewRequest(http.MethodGet, artURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dzUserAgent)
	resp, err := dzHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("artwork CDN returned %s", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// ---------- misc helpers ----------

func yearOf(releaseDate string) string {
	if len(releaseDate) >= 4 {
		return releaseDate[:4]
	}
	return ""
}

var filenameSanitizer = regexp.MustCompile(`[\\/:*?"<>|\r\n\t]+`)

func sanitizeFilename(s string) string {
	s = filenameSanitizer.ReplaceAllString(strings.TrimSpace(s), "_")
	s = strings.Trim(s, ". ")
	if !utf8.ValidString(s) || s == "" {
		s = "track"
	}
	if len(s) > 180 {
		s = s[:180]
	}
	return s
}
