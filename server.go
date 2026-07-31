package main

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "log"
    "net/http"
    "strconv"
    "strings"
    "sync"
    "time"

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
    
    // Apply quality setting
    switch quality {
    case "normal":
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

// ---------- helpers ----------

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

func extractCover(m map[string]any, imgType string) string {
    if m == nil {
        return ""
    }
    for _, v := range m {
        if s, ok := v.(string); ok {
            if (strings.HasPrefix(s, "//") || strings.HasPrefix(s, "http")) && (strings.Contains(s, "dzcdn") || strings.Contains(s, ".jpg") || strings.Contains(s, "deezer.com")) {
                if strings.HasPrefix(s, "//") {
                    return "https:" + s
                }
                return s
            }
        }
    }
    md5Keys := []string{"md5_image", "Md5Image", "cover_md5", "CoverMD5", "picture_md5", "PictureMd5", "md5", "Md5"}
    for _, k := range md5Keys {
        if v := strVal(m, k); v != "" {
            return fmt.Sprintf("https://e-cdns-images.dzcdn.net/images/%s/%s/500x500-000000-80-0-0.jpg", imgType, v)
        }
    }
    for _, v := range m {
        if sub, ok := v.(map[string]any); ok {
            if img := extractCover(sub, imgType); img != "" {
                return img
            }
        }
    }
    return ""
}

func toTrack(t dz.Track) Track {
    m := toMap(t)
    tr := Track{
        ID:         fmt.Sprint(t.ID),
        Title:      t.Name,
        DurationMS: t.DurationMS,
        Artist:     t.ArtistLine(),
    }
    if artist, ok := m["artist"].(map[string]any); ok {
        tr.ArtistID = strVal(artist, "id")
    }
    if album, ok := m["album"].(map[string]any); ok {
        tr.Album = strVal(album, "title", "name")
        tr.AlbumID = strVal(album, "id")
    }
    tr.Cover = extractCover(m, "cover")
    return tr
}

// ---------- handlers ----------

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    q := r.URL.Query().Get("q")
    res, err := s.client.Search(q)
    if err != nil {
        http.Error(w, err.Error(), 502)
        return
    }
    out := map[string]any{}
    if res.Tracks != nil {
        ts := make([]Track, 0, len(res.Tracks))
        for _, t := range res.Tracks { ts = append(ts, toTrack(t)) }
        out["tracks"] = ts
    }
    if res.Artists != nil {
        as := make([]Artist, 0, len(res.Artists))
        for _, a := range res.Artists {
            m := toMap(a)
            as = append(as, Artist{
                ID: strVal(m, "id"), Name: strVal(m, "name"),
                NbFans: intVal(m, "nb_fan", "nbFans", "NbFans"),
                Cover:  extractCover(m, "artist"),
            })
        }
        out["artists"] = as
    }
    if res.Albums != nil {
        al := make([]Playlist, 0, len(res.Albums))
        for _, a := range res.Albums {
            m := toMap(a)
            al = append(al, Playlist{
                ID: strVal(m, "id"), Title: strVal(m, "title", "name", "Title", "Name"),
                NbTracks: intVal(m, "nb_tracks", "nbTracks", "NbTracks"),
                Cover:    extractCover(m, "cover"),
            })
        }
        out["albums"] = al
    }
    writeJSON(w, out)
}

func (s *Server) handleCharts(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    res, err := s.client.Charts("0")
    if err != nil { http.Error(w, err.Error(), 502); return }
    ts := make([]Track, 0, len(res.Tracks))
    for _, t := range res.Tracks { ts = append(ts, toTrack(t)) }
    writeJSON(w, map[string]any{"tracks": ts})
}

func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    tracks, err := s.client.Favorites()
    if err != nil { http.Error(w, err.Error(), 502); return }
    ts := make([]Track, 0, len(tracks))
    for _, t := range tracks { ts = append(ts, toTrack(t)) }
    writeJSON(w, ts)
}

func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    pl, err := s.client.Playlists()
    if err != nil { http.Error(w, err.Error(), 502); return }
    out := make([]Playlist, 0, len(pl))
    for _, item := range pl {
        m := toMap(item)
        out = append(out, Playlist{
            ID: strVal(m, "id"), Title: strVal(m, "title", "name", "Title", "Name"),
            NbTracks: intVal(m, "nb_tracks", "nbTracks", "NbTracks"),
            Cover:    extractCover(m, "cover"),
        })
    }
    writeJSON(w, out)
}

func (s *Server) handlePlaylist(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    id := r.PathValue("id")
    pl, err := s.client.PlaylistTracks(id)
    if err != nil { http.Error(w, err.Error(), 502); return }
    
    var cover string
    ts := make([]Track, 0, len(pl))
    for _, t := range pl {
        if cover == "" { cover = extractCover(toMap(t), "cover") }
        ts = append(ts, toTrack(t))
    }
    writeJSON(w, map[string]any{
        "title": "Playlist", "cover": cover, "tracks": ts, "nbTracks": len(ts),
    })
}

func (s *Server) handleArtist(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    id := r.PathValue("id")
    page, err := s.client.ArtistProfile(id)
    if err != nil { http.Error(w, err.Error(), 502); return }

    m := toMap(page)
    artistOut := Artist{
        ID: strVal(toMap(page.Artist), "id"), Name: page.Artist.Name,
        NbFans: page.Artist.NbFans,
        NbAlbum: intVal(toMap(page.Artist), "nb_album", "nbAlbums", "NbAlbum"),
        Cover:   extractCover(toMap(page.Artist), "artist"),
    }

    top := []Track{}
    if tracks, ok := m["tracks"].([]any); ok {
        for _, x := range tracks {
            if tm, ok := x.(map[string]any); ok {
                top = append(top, Track{
                    ID: strVal(tm, "id"), Title: strVal(tm, "title", "name"),
                    Artist: strVal(toMap(tm["artist"]), "name"),
                    DurationMS: int64(intVal(tm, "duration_ms", "duration")),
                    Cover:      extractCover(toMap(tm["album"]), "cover"),
                })
            }
        }
    } else if toptracks, ok := m["toptracks"].([]any); ok {
        for _, x := range toptracks {
            if tm, ok := x.(map[string]any); ok {
                top = append(top, Track{
                    ID: strVal(tm, "id"), Title: strVal(tm, "title", "name"),
                    Artist: strVal(toMap(tm["artist"]), "name"),
                    DurationMS: int64(intVal(tm, "duration_ms", "duration")),
                    Cover:      extractCover(toMap(tm["album"]), "cover"),
                })
            }
        }
    }

    alb := []Playlist{}
    if albums, ok := m["albums"].([]any); ok {
        for _, x := range albums {
            if am, ok := x.(map[string]any); ok {
                alb = append(alb, Playlist{
                    ID: strVal(am, "id"), Title: strVal(am, "title", "name"),
                    NbTracks: intVal(am, "nb_tracks", "nbTracks"),
                    Cover:    extractCover(am, "cover"),
                })
            }
        }
    }

    writeJSON(w, map[string]any{"artist": artistOut, "topTracks": top, "albums": alb})
}

func (s *Server) handleAlbum(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    id := r.PathValue("id")
    al, err := s.client.AlbumTracks(id)
    if err != nil { http.Error(w, err.Error(), 502); return }
    
    var title, cover, artist string
    ts := make([]Track, 0, len(al))
    for _, t := range al {
        m := toMap(t)
        if title == "" {
            if album, ok := m["album"].(map[string]any); ok {
                title = strVal(album, "title", "name")
                cover = extractCover(album, "cover")
            }
            if art, ok := m["artist"].(map[string]any); ok {
                artist = strVal(art, "name")
            }
        }
        ts = append(ts, toTrack(t))
    }
    writeJSON(w, map[string]any{
        "title": title, "cover": cover, "artist": artist, "tracks": ts, "nbTracks": len(ts),
    })
}

func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    id := r.PathValue("id")
    lyr, err := s.client.Lyrics(id)
    if err != nil { http.Error(w, err.Error(), 502); return }
    out := Lyrics{}
    if lyr.IsSynced() {
        for _, l := range lyr.Synced { out.Synced = append(out.Synced, LyricLine{TimeMS: l.TimeMS, Text: l.Text}) }
    } else {
        m := toMap(lyr)
        if unsynced, ok := m["unsynced"].([]any); ok {
            for _, l := range unsynced {
                if s, ok := l.(string); ok { out.Unsynced = append(out.Unsynced, s) }
                if lm, ok := l.(map[string]any); ok { out.Unsynced = append(out.Unsynced, strVal(lm, "text", "line", "content")) }
            }
        }
    }
    writeJSON(w, out)
}

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
    s.mu.Lock()
    defer s.mu.Unlock()
    id := r.PathValue("id")
    plan, err := s.client.PrepareStream(id)
    if err != nil { http.Error(w, err.Error(), 502); return }
    
    // Download into memory to support Range requests (fixes broken pipes)
    buf := new(bytes.Buffer)
    if err := dz.DownloadTrack(plan, buf); err != nil {
        log.Printf("download %s: %v", id, err)
        http.Error(w, "Failed to download track", 500)
        return
    }
    
    // Dynamically set Content-Type based on the format returned by Deezer
    label := dz.FormatLabel(plan.Format)
    contentType := "audio/mpeg"
    if strings.Contains(label, "FLAC") {
        contentType = "audio/flac"
    }
    
    w.Header().Set("Content-Type", contentType)
    w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
    http.ServeContent(w, r, "", time.Now(), bytes.NewReader(buf.Bytes()))
}

var _ io.Reader