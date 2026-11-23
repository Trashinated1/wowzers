package main

import (
	"archive/zip"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

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

// dzRestBase is a variable so tests can point it at a mock server.
var dzRestBase = "https://api.deezer.com"

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

// dzRestPlaylistFull is /playlist/{id} — full multi-artist track listing plus
// owner and covers, used for the album-style playlist view and zip downloads.
type dzRestPlaylistFull struct {
	ID    json.Number `json:"id"`
	Title string      `json:"title"`
	Owner struct {
		Name string `json:"name"`
	} `json:"user"`
	Public        bool   `json:"public"`
	NbTracks      int    `json:"nb_tracks"`
	PictureXL     string `json:"picture_xl"`
	PictureBig    string `json:"picture_big"`
	PictureMedium string `json:"picture_medium"`
	Tracks        struct {
		Data []dzRestTrack `json:"data"`
	} `json:"tracks"`
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

func pickPicture(xl, big, medium string) string {
	if xl != "" {
		return xl
	}
	if big != "" {
		return big
	}
	return medium
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
	return http.ListenAndServe(s.addr, s.withCORS(s.routes()))
}

// routes builds the HTTP handler: static web assets plus the JSON API.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("GET /", http.FileServer(http.FS(webRoot)))

	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/charts", s.handleCharts)
	mux.HandleFunc("GET /api/favorites", s.handleFavorites)
	mux.HandleFunc("GET /api/playlist/{id}", s.handlePlaylist)
	mux.HandleFunc("GET /api/artist/{id}", s.handleArtist)
	mux.HandleFunc("GET /api/album/{id}", s.handleAlbum)
	mux.HandleFunc("GET /api/lyrics/{id}", s.handleLyrics)
	mux.HandleFunc("GET /api/stream/{id}", s.handleStream)
	mux.HandleFunc("GET /api/cover/{id}", s.handleCover)
	mux.HandleFunc("GET /api/download/{id}", s.handleDownload)
	mux.HandleFunc("GET /api/download/album/{id}", s.handleAlbumDownload)
	mux.HandleFunc("GET /api/download/playlist/{id}", s.handlePlaylistDownload)
	mux.HandleFunc("GET /api/download/zip", s.handleZipDownload)
	mux.HandleFunc("POST /api/import/csv", s.handleImportCSV)
	return mux
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

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id := r.PathValue("id")

	// Full REST listing first: real playlist title, owner and cover plus
	// the complete contributor list per track, so the playlist view gets
	// the same rich header the album view has.
	if b, err := restGet("/playlist/" + url.PathEscape(id)); err == nil {
		var pl dzRestPlaylistFull
		if json.Unmarshal(b, &pl) == nil && len(pl.Tracks.Data) > 0 {
			ts := make([]Track, 0, len(pl.Tracks.Data))
			for _, t := range pl.Tracks.Data {
				ts = append(ts, trackFromRest(t))
			}
			nb := pl.NbTracks
			if nb == 0 {
				nb = len(ts)
			}
			title := strings.TrimSpace(pl.Title)
			if title == "" {
				title = "Playlist"
			}
			writeJSON(w, map[string]any{
				"title":    title,
				"owner":    pl.Owner.Name,
				"cover":    pickPicture(pl.PictureXL, pl.PictureBig, pl.PictureMedium),
				"tracks":   ts,
				"nbTracks": nb,
			})
			return
		}
	}

	// SDK fallback (private playlists are not in the public REST API).
	if s.client == nil {
		http.Error(w, "playlist unavailable", 502)
		return
	}
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

// prepareTrack resolves a StreamPlan at the current quality, falling back to
// 128kbps once if the preferred quality is rejected. Only the prepare step
// (and our fallback policy) is serialized behind s.mu; the SDK client is
// internally synchronized, so downloads can run concurrently afterwards.
func (s *Server) prepareTrack(id string) (*dz.StreamPlan, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	plan, err := s.client.PrepareStream(id)
	if err != nil {
		// FALLBACK: retry at 128kbps if the preferred quality fails.
		log.Printf("stream %s: prepare failed — falling back to 128kbps", id)
		s.client.SetQuality(dz.QualityNormal)
		plan, err = s.client.PrepareStream(id)
	}
	return plan, err
}

// fetchAudio downloads a plan's decrypted audio into memory.
func fetchAudio(plan *dz.StreamPlan, id string) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := dz.DownloadTrack(plan, buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	plan, err := s.prepareTrack(id)
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}

	// Download into memory to support Range requests (fixes broken pipes).
	// Runs WITHOUT s.mu so playback stays responsive during zip downloads.
	audio, err := fetchAudio(plan, id)
	if err != nil {
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
	http.ServeContent(w, r, "", time.Now(), bytes.NewReader(audio))
}

// errTrackNotFound means neither the REST metadata nor the SDK had the track.
var errTrackNotFound = errors.New("track not found")

// trackFile is a fully assembled, tagged audio download.
type trackFile struct {
	Data   []byte // tagged audio bytes (MP3 with ID3v2, FLAC with Vorbis+Picture)
	Ext    string // ".mp3" or ".flac" — follows the format Deezer delivered
	Title  string
	Artist string
	Album  string
}

// buildTrackFile assembles one downloadable track:
//  1. metadata via public REST /track/{id} (contributors, track_position,
//     release_date, album cover) with an SDK fallback;
//  2. audio bytes via the OpenDeezer SDK grabber (the same PrepareStream +
//     DownloadTrack pipeline used for playback — never re-encoded);
//  3. cover art via the CDN (memory-cached, best-effort);
//  4. metadata embedded on top of the untouched audio.
func (s *Server) buildTrackFile(id string) (trackFile, error) {
	tf := trackFile{}

	// 1. Metadata.
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
		return tf, errTrackNotFound
	}
	if artist == "" {
		artist = "Unknown Artist"
	}
	if album == "" {
		album = "Unknown Album"
	}
	tf.Title, tf.Artist, tf.Album = title, artist, album

	// 2. Audio bytes via the SDK (auth + decryption), with 128kbps fallback.
	plan, err := s.prepareTrack(id)
	if err != nil {
		return tf, err
	}
	audio, aerr := fetchAudio(plan, id)
	if aerr != nil {
		log.Printf("download %s: %v", id, aerr)
		plan, err = s.prepareTrack(id) // prepareTrack downgrades to 128 on failure
		if err != nil {
			return tf, err
		}
		if audio, aerr = fetchAudio(plan, id); aerr != nil {
			return tf, aerr
		}
	}

	// 3. Cover art (best-effort — a missing image never fails the download).
	var art []byte
	if coverURL != "" {
		art = fetchArtworkCached(coverURL)
		if len(art) == 0 {
			log.Printf("download %s: artwork fetch failed (non-fatal)", id)
		}
	}

	// 4. Tag with metadata. On any tagging error the raw audio bytes are
	//    served untouched rather than a corrupted file.
	isFLAC := strings.Contains(strings.ToUpper(plan.Format), "FLAC")
	tf.Ext = ".mp3"
	var final []byte
	var tagErr error
	if isFLAC {
		tf.Ext = ".flac"
		final, tagErr = tagFLACBytes(audio, title, artist, album, year, trackNum, art)
	} else {
		final, tagErr = tagMP3Bytes(audio, title, artist, album, year, trackNum, art)
	}
	if tagErr != nil {
		log.Printf("download %s: tagging failed (serving raw audio): %v", id, tagErr)
		final = audio
	}
	tf.Data = final
	return tf, nil
}

// contentDisposition builds an RFC 5987 attachment header for a filename.
func contentDisposition(fname string) string {
	asciiName := strings.Map(func(r rune) rune {
		if r < 32 || r > 126 {
			return '_'
		}
		return r
	}, fname)
	return `attachment; filename="` + asciiName + `"; filename*=UTF-8''` + url.PathEscape(fname)
}

// handleDownload serves a single tagged MP3/FLAC file with the cover art and
// full metadata (title, every credited artist, album, year, track number)
// embedded, ready to be saved straight into a music library.
func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	tf, err := s.buildTrackFile(r.PathValue("id"))
	if err != nil {
		if errors.Is(err, errTrackNotFound) {
			http.Error(w, "track not found", 404)
			return
		}
		http.Error(w, err.Error(), 502)
		return
	}

	fname := sanitizeFilename(tf.Artist+" - "+tf.Title) + tf.Ext
	w.Header().Set("Content-Type", contentTypeFor(tf.Ext))
	w.Header().Set("Content-Disposition", contentDisposition(fname))
	w.Header().Set("Content-Length", strconv.Itoa(len(tf.Data)))
	w.Write(tf.Data)
}

// ---------- zip downloads (albums, playlists, arbitrary id lists) ----------

// maxZipTracks caps one archive so a request can't pin the server forever.
const maxZipTracks = 300

// writeZipEntry appends a stored (uncompressed — MP3/FLAC are already
// compressed) zip entry named "NNN - Artist - Title.ext", de-duplicated.
func writeZipEntry(zw *zip.Writer, pos int, tf trackFile, used map[string]int) error {
	base := fmt.Sprintf("%03d - %s - %s", pos, tf.Artist, tf.Title) + tf.Ext
	name := sanitizeFilename(base)
	if n := used[name]; n > 0 {
		name = sanitizeFilename(fmt.Sprintf("%03d - %s - %s (%d)", pos, tf.Artist, tf.Title, n+1)) + tf.Ext
	}
	used[name]++
	h := &zip.FileHeader{Name: name, Method: zip.Store, Modified: time.Now()}
	fw, err := zw.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = fw.Write(tf.Data)
	return err
}

// serveZip streams ids as one ZIP archive of tagged audio files. The first
// track is fully resolved before any headers are written, so an account or
// network problem yields a clean 502 instead of a broken archive; later
// failures skip their track and keep the rest of the archive intact.
func (s *Server) serveZip(w http.ResponseWriter, ids []string, zipName string) {
	if len(ids) == 0 {
		http.Error(w, "no tracks to download", 400)
		return
	}
	if len(ids) > maxZipTracks {
		ids = ids[:maxZipTracks]
	}

	first, err := s.buildTrackFile(ids[0])
	if err != nil {
		log.Printf("zip %q: first track %s failed: %v", zipName, ids[0], err)
		if errors.Is(err, errTrackNotFound) {
			http.Error(w, "track not found: "+ids[0], 404)
		} else {
			http.Error(w, err.Error(), 502)
		}
		return
	}

	fname := sanitizeFilename(zipName) + ".zip"
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition(fname))

	zw := zip.NewWriter(w)
	used := map[string]int{}
	done := 0
	if werr := writeZipEntry(zw, 1, first, used); werr == nil {
		done = 1
	}
	for i, id := range ids[1:] {
		tf, err := s.buildTrackFile(id)
		if err != nil {
			log.Printf("zip %q: skipping track %s: %v", zipName, id, err)
			continue
		}
		if err := writeZipEntry(zw, i+2, tf, used); err != nil {
			log.Printf("zip %q: entry write failed: %v", zipName, err)
			break
		}
		done++
	}
	log.Printf("zip %q: wrote %d/%d tracks", zipName, done, len(ids))
	zw.Close()
}

func trackIDsFromTracks(ts []dzRestTrack) []string {
	ids := make([]string, 0, len(ts))
	for _, t := range ts {
		if id := t.ID.String(); id != "" && id != "0" {
			ids = append(ids, id)
		}
	}
	return ids
}

// handleAlbumDownload zips every track of an album as tagged audio files.
func (s *Server) handleAlbumDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	title, artist := "Album", ""
	var ids []string
	if b, err := restGet("/album/" + url.PathEscape(id)); err == nil {
		var alb dzRestAlbumFull
		if json.Unmarshal(b, &alb) == nil {
			title = alb.Title
			artist, _ = joinArtists(alb.Artist, alb.Contributors)
			ids = trackIDsFromTracks(alb.Tracks.Data)
		}
	}
	if len(ids) == 0 && s.client != nil {
		if al, serr := s.client.AlbumTracks(id); serr == nil && len(al) > 0 {
			for _, t := range al {
				ids = append(ids, t.ID)
			}
			if title == "Album" {
				title = al[0].AlbumName
			}
			if artist == "" && len(al) > 0 {
				artist = al[0].ArtistLine()
			}
		}
	}
	if len(ids) == 0 {
		http.Error(w, "album has no downloadable tracks", 404)
		return
	}
	name := "Album"
	if artist != "" {
		name = artist + " - " + title
	} else {
		name = title
	}
	s.serveZip(w, ids, name)
}

// handlePlaylistDownload zips every track of a Deezer playlist.
func (s *Server) handlePlaylistDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	title := "Playlist"
	var ids []string
	if b, err := restGet("/playlist/" + url.PathEscape(id)); err == nil {
		var pl dzRestPlaylistFull
		if json.Unmarshal(b, &pl) == nil {
			title = pl.Title
			ids = trackIDsFromTracks(pl.Tracks.Data)
		}
	}
	if len(ids) == 0 && s.client != nil {
		if ts, serr := s.client.PlaylistTracks(id); serr == nil && len(ts) > 0 {
			for _, t := range ts {
				ids = append(ids, t.ID)
			}
		}
	}
	if len(ids) == 0 {
		http.Error(w, "playlist has no downloadable tracks", 404)
		return
	}
	s.serveZip(w, ids, title+" - Wowzers")
}

// handleZipDownload zips an arbitrary list of track ids (?ids=1,2,3), used by
// local playlists and the queue.
func (s *Server) handleZipDownload(w http.ResponseWriter, r *http.Request) {
	raw := strings.TrimSpace(r.URL.Query().Get("ids"))
	if raw == "" {
		http.Error(w, "missing ids parameter", 400)
		return
	}
	parts := strings.Split(raw, ",")
	ids := make([]string, 0, len(parts))
	seen := map[string]bool{}
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" || p == "0" || seen[p] {
			continue
		}
		if _, err := strconv.Atoi(p); err != nil {
			continue
		}
		seen[p] = true
		ids = append(ids, p)
	}
	s.serveZip(w, ids, "Wowzers Tracks")
}

// ---------- cover proxy ----------

// artworkCache is a small in-memory cache of CDN cover bytes; covers repeat
// across tracks of one album, so exact LRU is overkill.
var artworkCache = struct {
	sync.Mutex
	m map[string][]byte
}{m: map[string][]byte{}}

const artworkCacheMax = 256

func fetchArtworkCached(artURL string) []byte {
	if artURL == "" {
		return nil
	}
	artworkCache.Lock()
	if b, ok := artworkCache.m[artURL]; ok {
		artworkCache.Unlock()
		return b
	}
	artworkCache.Unlock()

	b, err := fetchArtwork(artURL)
	if err != nil || len(b) == 0 {
		return nil
	}

	artworkCache.Lock()
	if len(artworkCache.m) >= artworkCacheMax {
		n := artworkCacheMax / 4 // evict an arbitrary quarter
		for k := range artworkCache.m {
			delete(artworkCache.m, k)
			n--
			if n <= 0 {
				break
			}
		}
	}
	artworkCache.m[artURL] = b
	artworkCache.Unlock()
	return b
}

// resolveCoverURL finds the best album-cover URL for a track (or album) id.
func (s *Server) resolveCoverURL(id string) string {
	esc := url.PathEscape(id)
	if b, err := restGet("/track/" + esc); err == nil {
		var m struct {
			dzRestTrack
		}
		if json.Unmarshal(b, &m) == nil {
			if u := pickCover(m.Album); u != "" {
				return u
			}
		}
	}
	if b, err := restGet("/album/" + esc); err == nil {
		var alb dzRestAlbumFull
		if json.Unmarshal(b, &alb) == nil {
			if u := pickCover(dzRestAlbum{CoverXL: alb.CoverXL, CoverBig: alb.CoverBig, CoverMedium: alb.CoverMedium}); u != "" {
				return u
			}
		}
	}
	if s.client != nil {
		if t, terr := s.client.Track(id); terr == nil && t.ArtworkURL != "" {
			return t.ArtworkURL
		}
	}
	return ""
}

// handleCover serves GET /api/cover/{id} — the album artwork for a track or
// album id, proxied through the server so the browser never depends on CDN
// hotlink behavior.
func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	u := s.resolveCoverURL(r.PathValue("id"))
	if u == "" {
		http.Error(w, "cover not found", 404)
		return
	}
	art := fetchArtworkCached(u)
	if len(art) == 0 {
		http.Error(w, "cover fetch failed", 502)
		return
	}
	mime := sniffImageMIME(art)
	if mime == "" {
		mime = "image/jpeg"
	}
	w.Header().Set("Content-Type", mime)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(art)))
	w.Write(art)
}

// ---------- TuneMyMusic CSV import (strict title/artist/album matching) ----------

// maxCSVRows caps one import: matching hits the public Deezer API once or
// twice per row, so imports beyond this size would only trip rate limits.
const maxCSVRows = 10000

// maxMatchConcurrency keeps row-matching requests gentle on the public API.
const maxMatchConcurrency = 5

// maxReportedSkipped caps the skipped-rows list in the JSON report.
const maxReportedSkipped = 500

// skippedRow is a CSV row that had NO strictly-matching Deezer track.
type skippedRow struct {
	Row    int    `json:"row"`
	Title  string `json:"title"`
	Artist string `json:"artist"`
	Album  string `json:"album"`
	Reason string `json:"reason"`
}

// importedPlaylist groups the import result for one "Playlist name" column.
type importedPlaylist struct {
	Name    string  `json:"name"`
	Matched int     `json:"matched"`
	Skipped int     `json:"skipped"`
	Tracks  []Track `json:"tracks"`
}

// importReport is the JSON response of POST /api/import/csv.
type importReport struct {
	TotalRows        int                `json:"totalRows"`
	MatchedRows      int                `json:"matchedRows"`
	SkippedRows      int                `json:"skippedRows"`
	SkippedTruncated bool               `json:"skippedTruncated,omitempty"`
	Playlists        []importedPlaylist `json:"playlists"`
	Skipped          []skippedRow       `json:"skipped"`
}

// csvRow is one data line of a TuneMyMusic export.
type csvRow struct {
	Line     int
	Title    string
	Artist   string
	Album    string
	Playlist string
	RowType  string
	ISRC     string
}

// normalizeForMatch reduces a name to its comparable core: lowercase, with
// diacritics folded and EVERY character that is not a letter or digit dropped
// — capitalization, punctuation and whitespace differences can never cause a
// mismatch ("It's Me, It's Verity" == "its me its verity", "AC/DC" ==
// "ACDC" == "AC DC", "PT.1" == "PT 1"). Comparisons stay STRICT on content:
// the full letter/digit sequence must be identical, so
// "keep me going (BIRDBRAIN)" != "keep me going" and
// "Constant Companions (Deluxe Edition)" != "Constant Companions".
func normalizeForMatch(s string) string {
	s = norm.NFD.String(s)
	var out []rune
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) { // combining mark (folded accent)
			continue
		}
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out = append(out, unicode.ToLower(r))
		}
	}
	return string(out)
}

// artistFeatSplit separates featuring markers ("feat.", "ft.", "featuring")
// that Spotify exports spell in various ways.
var artistFeatSplit = regexp.MustCompile(`(?i)\b(?:featuring|feat|ft)\b\.?`)

// splitArtistTokens splits a CSV "Artist name" cell into individual artists,
// dropping separators (, & / ; |) and featuring markers.
func splitArtistTokens(s string) []string {
	s = artistFeatSplit.ReplaceAllString(strings.TrimSpace(s), ",")
	parts := strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == '&' || r == '/' || r == ';' || r == '|'
	})
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// csvArtistsCredited reports whether EVERY artist named in the CSV row is
// credited on the Deezer track (main artist or any contributor). The whole
// cell is also tried unsplit, so compound names like "Kiss/Drug Kiss" match
// even when the separator splitting would break them apart.
func csvArtistsCredited(csvArtist string, dt dzRestTrack) bool {
	credited := map[string]bool{}
	add := func(a dzRestArtist) {
		if n := normalizeForMatch(a.Name); n != "" {
			credited[n] = true
		}
	}
	add(dt.Artist)
	for _, a := range dt.Contributors {
		add(a)
	}
	if normalizeForMatch(csvArtist) != "" && credited[normalizeForMatch(csvArtist)] {
		return true
	}
	tokens := splitArtistTokens(csvArtist)
	if len(tokens) == 0 {
		return false
	}
	for _, t := range tokens {
		if !credited[normalizeForMatch(t)] {
			return false
		}
	}
	return true
}

// strictTrackMatch verifies that a Deezer track is EXACTLY the track the CSV
// row describes: title and album identical after normalization (which ignores
// capitalization, punctuation and whitespace) and every CSV artist credited.
// An empty album cell cannot be verified and is ignored.
func strictTrackMatch(row csvRow, dt dzRestTrack) (bool, string) {
	// A truly missing title never matches; a punctuation-only one ("!!!")
	// still can when the Deezer title normalizes the same way.
	if strings.TrimSpace(row.Title) == "" ||
		normalizeForMatch(row.Title) != normalizeForMatch(dt.Title) {
		return false, "title mismatch"
	}
	if !csvArtistsCredited(row.Artist, dt) {
		return false, "artist mismatch"
	}
	if strings.TrimSpace(row.Album) != "" &&
		normalizeForMatch(row.Album) != normalizeForMatch(dt.Album.Title) {
		return false, "album mismatch"
	}
	return true, ""
}

// parseTuneMyMusicCSV parses a TuneMyMusic/Spotify export. Columns are
// located by header name, so extra columns and different spellings
// ("Track name", "Track Name") are tolerated.
func parseTuneMyMusicCSV(data []byte) ([]csvRow, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")) // UTF-8 BOM
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1 // tolerate ragged rows
	r.LazyQuotes = true

	get := func(rec []string, ci int) string {
		if ci < 0 || ci >= len(rec) {
			return ""
		}
		return strings.TrimSpace(rec[ci])
	}

	var header []string
	var headerCols [6]int
	rows := make([]csvRow, 0, 64)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("invalid csv: %w", err)
		}
		// True 1-based CSV line number (encoding/csv silently skips
		// blank lines, so counting records would drift).
		line, _ := r.FieldPos(0)
		if header == nil {
			header = rec
			idx := map[string]int{}
			for i, h := range header {
				idx[strings.ToLower(strings.TrimSpace(h))] = i
			}
			col := func(name string) int {
				if i, ok := idx[name]; ok {
					return i
				}
				for k, i := range idx {
					if strings.Contains(k, name) {
						return i
					}
				}
				return -1
			}
			if col("track") < 0 || col("artist") < 0 {
				return nil, fmt.Errorf("not a TuneMyMusic export: no \"Track name\"/\"Artist name\" columns")
			}
			headerCols = [6]int{col("track"), col("artist"), col("album"), col("playlist"), col("type"), col("isrc")}
			continue
		}
		row := csvRow{
			Line:     line,
			Title:    get(rec, headerCols[0]),
			Artist:   get(rec, headerCols[1]),
			Album:    get(rec, headerCols[2]),
			Playlist: get(rec, headerCols[3]),
			RowType:  get(rec, headerCols[4]),
			ISRC:     strings.ToUpper(get(rec, headerCols[5])),
		}
		if row.Title == "" && row.Artist == "" && row.Album == "" {
			continue // fully blank line
		}
		rows = append(rows, row)
	}
	if header == nil {
		return nil, fmt.Errorf("csv has no data rows")
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("csv has no usable rows")
	}
	if len(rows) > maxCSVRows {
		rows = rows[:maxCSVRows]
	}
	return rows, nil
}

// matchRow resolves one CSV row to a Deezer track. An ISRC hit is held to the
// SAME strict name check; if the names differ the search index is consulted
// before the row is reported as skipped (with the exact reason).
func (s *Server) matchRow(row csvRow) (dzRestTrack, string) {
	isrcWhy := ""
	if row.ISRC != "" {
		if b, err := restGet("/track/isrc:" + url.PathEscape(row.ISRC)); err == nil {
			var dt dzRestTrack
			if json.Unmarshal(b, &dt) == nil && dt.ID.String() != "" && dt.ID.String() != "0" {
				if ok, why := strictTrackMatch(row, dt); ok {
					return dt, "isrc"
				} else {
					isrcWhy = fmt.Sprintf("ISRC %s resolves to %q but %s", row.ISRC, dt.Title, why)
				}
			}
		}
	}
	q := url.QueryEscape(strings.TrimSpace(row.Artist + " " + row.Title))
	if b, err := restGet("/search/track?q=" + q + "&limit=10"); err == nil {
		var res struct {
			Data []dzRestTrack `json:"data"`
		}
		if json.Unmarshal(b, &res) == nil {
			for _, dt := range res.Data {
				if ok, _ := strictTrackMatch(row, dt); ok {
					return dt, "search"
				}
			}
		}
	}
	if isrcWhy != "" {
		return dzRestTrack{}, isrcWhy + "; search found no strict match either"
	}
	return dzRestTrack{}, "no Deezer track matched title + artist + album"
}

// handleImportCSV implements POST /api/import/csv for TuneMyMusic exports.
// Every row is resolved to a Deezer track ONLY when title, artist and album
// names strictly match; unmatched rows are skipped and reported. Matched
// tracks are returned as Track objects so the browser can mirror them into
// LOCAL playlists for in-app playback. Nothing is ever written to the
// Deezer account.
func (s *Server) handleImportCSV(w http.ResponseWriter, r *http.Request) {
	var data []byte
	var err error
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<20)
		if err := r.ParseMultipartForm(32 << 20); err != nil {
			http.Error(w, "invalid upload: "+err.Error(), 400)
			return
		}
		file, _, ferr := r.FormFile("file")
		if ferr != nil {
			http.Error(w, `missing "file" field — attach the TuneMyMusic CSV`, 400)
			return
		}
		defer file.Close()
		data, err = io.ReadAll(io.LimitReader(file, 32<<20))
	} else {
		// Raw CSV body (curl --data-binary @file.csv style).
		data, err = io.ReadAll(http.MaxBytesReader(w, r.Body, 32<<20))
	}
	if err != nil {
		http.Error(w, "read failed: "+err.Error(), 400)
		return
	}
	rows, err := parseTuneMyMusicCSV(data)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	log.Printf("csv import: %d rows (local playlists only)", len(rows))
	writeJSON(w, s.runImport(rows))
}

// runImport matches every row, groups the matches per playlist name and
// returns them as Track objects for the browser's local playlists.
func (s *Server) runImport(rows []csvRow) *importReport {
	report := &importReport{
		TotalRows: len(rows),
		Skipped:   []skippedRow{},
		Playlists: []importedPlaylist{},
	}

	// Resolve all rows concurrently; the public REST API is the bottleneck.
	type outcome struct {
		track *Track
		via   string
		why   string
	}
	outcomes := make([]outcome, len(rows))
	sem := make(chan struct{}, maxMatchConcurrency)
	var wg sync.WaitGroup
	for i := range rows {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			dt, via := s.matchRow(rows[i])
			if dt.ID.String() == "" || dt.ID.String() == "0" {
				outcomes[i] = outcome{why: via}
				return
			}
			t := trackFromRest(dt)
			outcomes[i] = outcome{track: &t, via: via}
		}(i)
	}
	wg.Wait()

	// Group per playlist (CSV order preserved, rows de-duplicated) and
	// collect the skip report.
	grouped := map[string][]Track{}
	seenInPl := map[string]map[string]bool{}
	skippedByPl := map[string]int{}
	for i := range rows {
		name := strings.TrimSpace(rows[i].Playlist)
		if name == "" {
			name = "Imported Tracks" // rows exported without a playlist
		}
		o := outcomes[i]
		if o.track == nil {
			if len(report.Skipped) < maxReportedSkipped {
				report.Skipped = append(report.Skipped, skippedRow{
					Row:    rows[i].Line,
					Title:  rows[i].Title,
					Artist: rows[i].Artist,
					Album:  rows[i].Album,
					Reason: o.why,
				})
			}
			skippedByPl[name]++
			continue
		}
		report.MatchedRows++
		if seenInPl[name] == nil {
			seenInPl[name] = map[string]bool{}
		}
		if seenInPl[name][o.track.ID] {
			continue // the same track listed twice in the CSV
		}
		seenInPl[name][o.track.ID] = true
		grouped[name] = append(grouped[name], *o.track)
	}
	if len(report.Skipped) < len(rows)-report.MatchedRows {
		report.SkippedTruncated = true
	}

	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pl := importedPlaylist{
			Name:    name,
			Matched: len(grouped[name]),
			Skipped: skippedByPl[name],
			Tracks:  grouped[name],
		}
		report.Playlists = append(report.Playlists, pl)
	}
	report.SkippedRows = len(rows) - report.MatchedRows
	log.Printf("csv import done: %d/%d matched, %d playlist(s)", report.MatchedRows, report.TotalRows, len(report.Playlists))
	return report
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
