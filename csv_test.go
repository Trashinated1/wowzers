package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// mockDeezer serves fake public-REST responses for offline handler tests and
// returns its base URL. The caller must restore dzRestBase afterwards
// (use t.Cleanup).
func mockDeezer(t *testing.T, extra func(mux *http.ServeMux)) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	// /search (track search for the top-level search handler) — one exact
	// match (second result) plus a decoy with the wrong album first, so the
	// matcher must iterate past it.
	mux.HandleFunc("/search", func(w http.ResponseWriter, r *http.Request) {
		q := strings.ToLower(r.URL.Query().Get("q"))
		if strings.Contains(q, "verity") {
			w.Write([]byte(`{"data":[
                                {"id":"100","title":"It's Me, It's Verity","duration":200,
                                 "artist":{"id":9,"name":"Horror Skunx"},
                                 "album":{"id":9,"title":"Totally Wrong Album"}},
                                {"id":"101","title":"It's Me, It's Verity","duration":200,
                                 "artist":{"id":9,"name":"Horror Skunx"},
                                 "album":{"id":9,"title":"It's me, It's Verity"}}
                        ],"total":2}`))
			return
		}
		w.Write([]byte(`{"data":[],"total":0}`))
	})

	// /search/track — one exact match (second result) plus a decoy with the
	// wrong album first, so the matcher must iterate past it.
	mux.HandleFunc("/search/track", func(w http.ResponseWriter, r *http.Request) {
		q := strings.ToLower(r.URL.Query().Get("q"))
		if strings.Contains(q, "verity") {
			w.Write([]byte(`{"data":[
                                {"id":"100","title":"It's Me, It's Verity","duration":200,
                                 "artist":{"id":9,"name":"Horror Skunx"},
                                 "album":{"id":9,"title":"Totally Wrong Album"}},
                                {"id":"101","title":"It's Me, It's Verity","duration":200,
                                 "artist":{"id":9,"name":"Horror Skunx"},
                                 "album":{"id":9,"title":"It's me, It's Verity"}}
                        ],"total":2}`))
			return
		}
		w.Write([]byte(`{"data":[],"total":0}`))
	})

	// /track/isrc:{isrc} — always resolves to a track whose album differs.
	mux.HandleFunc("/track/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/track/")
		if strings.HasPrefix(path, "isrc:") {
			w.Write([]byte(`{"id":"200","title":"It's Me, It's Verity","duration":200,
                                "artist":{"id":9,"name":"Horror Skunx"},
                                "album":{"id":99,"title":"Different Album"}}`))
			return
		}
		if path == "555" {
			w.Write([]byte(`{"id":"555","title":"Cover Song","duration":100,
                                "artist":{"id":1,"name":"Cover Artist"},
                                "album":{"id":50,"title":"Cover Album",
                                        "cover_xl":"` + mockCDNBase + `/cover.jpg"}}`))
			return
		}
		// Unknown id — Deezer's error envelope.
		w.Write([]byte(`{"error":{"type":"DataException","message":"no data","code":800}}`))
	})

	if extra != nil {
		extra(mux)
	}
	return httptest.NewServer(mux)
}

var mockCDNBase = "http://mock-cdn.invalid"

func TestNormalizeForMatch(t *testing.T) {
	eq := func(a, b string) {
		if normalizeForMatch(a) != normalizeForMatch(b) {
			t.Errorf("normalize(%q)=%q != normalize(%q)=%q", a, normalizeForMatch(a), b, normalizeForMatch(b))
		}
	}
	ne := func(a, b string) {
		if normalizeForMatch(a) == normalizeForMatch(b) {
			t.Errorf("normalize(%q) should differ from %q", a, b)
		}
	}
	// Capitalization, punctuation and whitespace NEVER cause a mismatch:
	// everything that is not a letter or digit is dropped.
	eq("It's me, It's Verity", "Its Me Its Verity")
	eq("BUTCHER VANITY", "butcher vanity")
	eq("AC/DC", "acdc")
	eq("AC/DC", "AC DC")
	eq("PT.1", "PT 1")
	eq("B*Witched", "BWitched")
	eq("  Multiple   Spaces  ", "multiple spaces")
	eq("A Fever You Can't Sweat Out", "A Fever You Cant Sweat Out")
	eq("hit the core!", "hit the core")
	// Diacritics fold.
	eq("Beyoncé", "beyonce")
	eq("Motörhead", "motorhead")
	// Japanese text is preserved (letters, no marks).
	eq("きゅうくらりん", "きゅうくらりん")
	// But different CONTENT stays different — strictness preserved.
	ne("keep me going (BIRDBRAIN)", "keep me going")
	ne("Constant Companions (Deluxe Edition)", "Constant Companions")
	ne("Part 1", "Part One")
	ne("My Ordinary Life", "My Extraordinary Life")
}

func TestSplitArtistTokens(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"Lady Gaga, Ariana Grande", []string{"Lady Gaga", "Ariana Grande"}},
		{"Simon & Garfunkel", []string{"Simon", "Garfunkel"}},
		{"Kikuo feat. Hatsune Miku", []string{"Kikuo", "Hatsune Miku"}},
		{"Kikuo feat Hatsune Miku", []string{"Kikuo", "Hatsune Miku"}},
		{"slayr", []string{"slayr"}},
		{"", nil},
	}
	for _, tc := range cases {
		got := splitArtistTokens(tc.in)
		if len(got) != len(tc.want) {
			t.Errorf("splitArtistTokens(%q) = %v, want %v", tc.in, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("splitArtistTokens(%q)[%d] = %q, want %q", tc.in, i, got[i], tc.want[i])
			}
		}
	}
}

func dzRestTrackFixture(id, title, album string, artists ...string) dzRestTrack {
	dt := dzRestTrack{
		ID:     jsonNumber(id),
		Title:  title,
		Album:  dzRestAlbum{ID: jsonNumber("9"), Title: album},
		Artist: dzRestArtist{ID: jsonNumber("1"), Name: artists[0]},
	}
	for _, a := range artists {
		dt.Contributors = append(dt.Contributors, dzRestArtist{ID: jsonNumber("1"), Name: a})
	}
	return dt
}

func jsonNumber(s string) json.Number { return json.Number(s) }

func TestStrictTrackMatch(t *testing.T) {
	// 1. Case/punctuation differences still match (same word sequence).
	row := csvRow{Line: 2, Title: "It's me, It's Verity", Artist: "Horror Skunx", Album: "It's me, It's Verity"}
	dt := dzRestTrackFixture("101", "It's Me, It's Verity", "It's Me, It's Verity", "Horror Skunx")
	if ok, why := strictTrackMatch(row, dt); !ok {
		t.Errorf("exact match rejected: %s", why)
	}

	// 2. CSV artist may be any credited contributor (feat. rows).
	row = csvRow{Line: 3, Title: "ふたつの木馬", Artist: "Kikuo", Album: "きくおミク7"}
	dt = dzRestTrackFixture("102", "ふたつの木馬", "きくおミク7", "Kikuo", "初音ミク")
	if ok, why := strictTrackMatch(row, dt); !ok {
		t.Errorf("contributor artist rejected: %s", why)
	}

	// 3. Multi-artist CSV cell: every named artist must be credited.
	row = csvRow{Line: 4, Title: "Rain On Me", Artist: "Lady Gaga, Ariana Grande", Album: "Chromatica"}
	dt = dzRestTrackFixture("103", "Rain On Me", "Chromatica", "Lady Gaga", "Ariana Grande")
	if ok, why := strictTrackMatch(row, dt); !ok {
		t.Errorf("multi-artist match rejected: %s", why)
	}

	// 4. One of the CSV artists missing from the credits -> artist mismatch.
	row = csvRow{Line: 5, Title: "Rain On Me", Artist: "Lady Gaga, Ariana Grande", Album: "Chromatica"}
	dt = dzRestTrackFixture("104", "Rain On Me", "Chromatica", "Lady Gaga")
	if ok, why := strictTrackMatch(row, dt); ok || why != "artist mismatch" {
		t.Errorf("missing-credit row matched (ok=%v, why=%s)", ok, why)
	}

	// 5. Different album -> rejected (the strictness the user asked for).
	row = csvRow{Line: 6, Title: "Machine Love", Artist: "Jamie Paige", Album: "Constant Companions (Deluxe Edition)"}
	dt = dzRestTrackFixture("105", "Machine Love", "Constant Companions", "Jamie Paige")
	if ok, why := strictTrackMatch(row, dt); ok || why != "album mismatch" {
		t.Errorf("different-album row matched (ok=%v, why=%s)", ok, why)
	}

	// 6. Different title -> rejected.
	row = csvRow{Line: 7, Title: "keep me going (BIRDBRAIN)", Artist: "slayr", Album: "keep me going"}
	dt = dzRestTrackFixture("106", "keep me going", "keep me going", "slayr")
	if ok, why := strictTrackMatch(row, dt); ok || why != "title mismatch" {
		t.Errorf("different-title row matched (ok=%v, why=%s)", ok, why)
	}

	// 7. Empty CSV album cell: album cannot be verified, title+artist decide.
	row = csvRow{Line: 8, Title: "Machine Love", Artist: "Jamie Paige", Album: ""}
	dt = dzRestTrackFixture("107", "Machine Love", "Anything", "Jamie Paige")
	if ok, why := strictTrackMatch(row, dt); !ok {
		t.Errorf("empty-album row rejected: %s", why)
	}

	// 8. Compound artist name split by the separator heuristic still
	//    matches through the whole-cell fallback.
	row = csvRow{Line: 9, Title: "Tag", Artist: "Kiss/Drug Kiss", Album: "Split"}
	dt = dzRestTrackFixture("108", "Tag", "Split", "Kiss/Drug Kiss")
	if ok, why := strictTrackMatch(row, dt); !ok {
		t.Errorf("compound-artist row rejected: %s", why)
	}

	// 9. Punctuation-only title ("!!!") matches the same punctuation-only
	//    Deezer title, but a genuinely missing title never matches.
	row = csvRow{Line: 10, Title: "!!!", Artist: "Chk Chk Chk", Album: "Strange Weather"}
	dt = dzRestTrackFixture("109", "!!!", "Strange Weather", "Chk Chk Chk")
	if ok, why := strictTrackMatch(row, dt); !ok {
		t.Errorf("punctuation-only title rejected: %s", why)
	}
	row = csvRow{Line: 11, Title: "", Artist: "Someone", Album: "Album"}
	dt = dzRestTrackFixture("110", "Song", "Album", "Someone")
	if ok, why := strictTrackMatch(row, dt); ok || why != "title mismatch" {
		t.Errorf("missing-title row matched (ok=%v, why=%s)", ok, why)
	}
}

func TestParseTuneMyMusicCSV(t *testing.T) {
	csvData := "\xef\xbb\xbfTrack name,Artist name,Album,Playlist name,Type,ISRC,Spotify - id\n" +
		"keep me going (BIRDBRAIN),slayr,keep me going (BIRDBRAIN),I guess bro,Playlist,USSM12604194,2julCvWIxzXfX757t5Bnsn\n" +
		"\n" +
		"DARLING DANCE,Kairikibear,DARLING SYNDROME,,Track,tcjpp2182322,32cvZTXsJi4EzkvVDMH4Ij\n"
	rows, err := parseTuneMyMusicCSV([]byte(csvData))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want 2 (blank line skipped)", len(rows))
	}
	r0 := rows[0]
	if r0.Line != 2 || r0.Title != "keep me going (BIRDBRAIN)" || r0.Artist != "slayr" ||
		r0.Playlist != "I guess bro" || r0.RowType != "Playlist" || r0.ISRC != "USSM12604194" {
		t.Errorf("row0 = %+v", r0)
	}
	r1 := rows[1]
	if r1.Line != 4 || r1.Playlist != "" || r1.ISRC != "TCJPP2182322" {
		t.Errorf("row1 = %+v (ISRC must be upper-cased)", r1)
	}

	// Not a TuneMyMusic export -> clear error.
	if _, err := parseTuneMyMusicCSV([]byte("a,b,c\n1,2,3\n")); err == nil ||
		!strings.Contains(err.Error(), "Track name") {
		t.Errorf("wrong-header error = %v", err)
	}
}

func TestHandleImportCSV(t *testing.T) {
	oldBase := dzRestBase
	ts := mockDeezer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("POST /api/import/csv", (&Server{}).handleImportCSV)
	})
	dzRestBase = ts.URL
	t.Cleanup(func() { dzRestBase = oldBase; ts.Close() })

	csvData := "Track name,Artist name,Album,Playlist name,Type,ISRC,Spotify - id\n" +
		// matches via search past the wrong-album decoy
		"It's me It's Verity,Horror Skunx,It's me It's Verity,I guess bro,Playlist,QT9AF2600026,aaa\n" +
		// ISRC resolves but album differs; search empty -> skipped with reason
		"Ghost Song,Nobody At All,Ghost Album,Other List,Track,QT9ZZ0000000,bbb\n" +
		// nothing at all -> skipped
		"Mystery Track,Nobody At All,,I guess bro,Playlist,,ccc\n"

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, _ := mw.CreateFormFile("file", "library.csv")
	fw.Write([]byte(csvData))
	mw.Close()

	req, _ := http.NewRequest("POST", ts.URL+"/api/import/csv?sync=0", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		t.Fatalf("status = %d", res.StatusCode)
	}

	var report importReport
	if err := json.NewDecoder(res.Body).Decode(&report); err != nil {
		t.Fatal(err)
	}
	if report.TotalRows != 3 || report.MatchedRows != 1 || report.SkippedRows != 2 {
		t.Errorf("report counters = %d/%d matched, %d skipped", report.MatchedRows, report.TotalRows, report.SkippedRows)
	}
	if len(report.Playlists) != 1 {
		t.Fatalf("playlists = %d, want 1 (only playlists with matches)", len(report.Playlists))
	}
	pl := report.Playlists[0]
	if pl.Name != "I guess bro" || pl.Matched != 1 || pl.Skipped != 1 || len(pl.Tracks) != 1 {
		t.Errorf("playlist report = %+v", pl)
	}
	if pl.Tracks[0].ID != "101" {
		t.Errorf("matched track id = %s, want 101 (search must pass the wrong-album decoy)", pl.Tracks[0].ID)
	}
	if len(report.Skipped) != 2 {
		t.Fatalf("skipped = %d entries, want 2", len(report.Skipped))
	}
	if !strings.Contains(report.Skipped[0].Reason, "ISRC QT9ZZ0000000") ||
		!strings.Contains(report.Skipped[0].Reason, "title mismatch") {
		t.Errorf("isrc skip reason = %q", report.Skipped[0].Reason)
	}
	if !strings.Contains(report.Skipped[1].Reason, "no Deezer track matched") {
		t.Errorf("plain skip reason = %q", report.Skipped[1].Reason)
	}

	// Bad upload -> 400.
	res2, err2 := http.DefaultClient.Post(ts.URL+"/api/import/csv", "text/csv",
		strings.NewReader("a,b,c\n1,2,3\n"))
	if err2 != nil {
		t.Fatal(err2)
	}
	defer res2.Body.Close()
	if res2.StatusCode != 400 {
		t.Errorf("wrong-header status = %d, want 400", res2.StatusCode)
	}
}

func TestHandleCoverProxy(t *testing.T) {
	oldBase := dzRestBase
	ts := mockDeezer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("GET /api/cover/{id}", (&Server{}).handleCover)
	})
	dzRestBase = ts.URL
	t.Cleanup(func() { dzRestBase = oldBase; ts.Close() })

	// A real JPEG served by the fake CDN.
	cdnHits := 0
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits++
		w.Write(realJPEG(t))
	}))
	defer cdn.Close()
	oldCDN := mockCDNBase
	mockCDNBase = cdn.URL
	t.Cleanup(func() { mockCDNBase = oldCDN })

	for i := 0; i < 2; i++ {
		res, err := http.Get(ts.URL + "/api/cover/555")
		if err != nil {
			t.Fatal(err)
		}
		body := make([]byte, 4096)
		n, _ := res.Body.Read(body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("cover status = %d", res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); ct != "image/jpeg" {
			t.Errorf("content-type = %q", ct)
		}
		if !bytes.Equal(body[:n], realJPEG(t)) {
			t.Errorf("cover body mismatch (%d bytes)", n)
		}
	}
	if cdnHits != 1 {
		t.Errorf("CDN hit %d times, artwork cache not working", cdnHits)
	}

	// Unknown id -> 404.
	res, err404 := http.Get(ts.URL + "/api/cover/404")
	if err404 != nil {
		t.Fatal(err404)
	}
	defer res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("unknown-id status = %d, want 404", res.StatusCode)
	}
}

func TestHandlePlaylistRESTFirst(t *testing.T) {
	oldBase := dzRestBase
	ts := mockDeezer(t, func(mux *http.ServeMux) {
		mux.HandleFunc("/playlist/", func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/777") {
				w.Write([]byte(`{"id":777,"title":"I guess bro","public":true,"nb_tracks":2,
                                        "user":{"name":"someone"},
                                        "picture_xl":"` + mockCDNBase + `/pl.jpg",
                                        "tracks":{"data":[
                                                {"id":"1","title":"Duet","duration":150,
                                                 "artist":{"id":1,"name":"A"},
                                                 "contributors":[{"id":1,"name":"A"},{"id":2,"name":"B"}],
                                                 "album":{"id":10,"title":"Duet Album","cover_medium":"` + mockCDNBase + `/a.jpg"}},
                                                {"id":"2","title":"Solo","duration":180,
                                                 "artist":{"id":1,"name":"A"},
                                                 "album":{"id":10,"title":"Duet Album"}}
                                        ]}}`))
				return
			}
			w.Write([]byte(`{"error":{"type":"DataException","message":"private","code":800}}`))
		})
		mux.HandleFunc("GET /api/playlist/{id}", (&Server{}).handlePlaylist)
	})
	dzRestBase = ts.URL
	t.Cleanup(func() { dzRestBase = oldBase; ts.Close() })

	res, err := http.Get(ts.URL + "/api/playlist/777")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Title    string  `json:"title"`
		Owner    string  `json:"owner"`
		NbTracks int     `json:"nbTracks"`
		Tracks   []Track `json:"tracks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if out.Title != "I guess bro" || out.Owner != "someone" || out.NbTracks != 2 || len(out.Tracks) != 2 {
		t.Errorf("playlist = %+v", out)
	}
	if out.Tracks[0].Artist != "A, B" {
		t.Errorf("multi-artist line = %q, want \"A, B\"", out.Tracks[0].Artist)
	}

	// Private playlist + no SDK client -> clean 502.
	res2, err502 := http.Get(ts.URL + "/api/playlist/888")
	if err502 != nil {
		t.Fatal(err502)
	}
	defer res2.Body.Close()
	if res2.StatusCode != 502 {
		t.Errorf("private playlist status = %d, want 502", res2.StatusCode)
	}
}

// TestRouteWiring mounts the full route table exactly as Run does and
// exercises the routes that work without an SDK client: static web assets,
// REST-backed search and a clean 404 for a zip download of unknown ids.
func TestRouteWiring(t *testing.T) {
	oldBase := dzRestBase
	ts := mockDeezer(t, nil)
	dzRestBase = ts.URL
	t.Cleanup(func() { dzRestBase = oldBase; ts.Close() })

	s := &Server{} // nil SDK client — auth-gated routes are out of scope here
	srv := httptest.NewServer(s.withCORS(s.routes()))
	defer srv.Close()

	// Static web assets must serve (embedded FS).
	res, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	idx, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != 200 || !strings.Contains(string(idx), "Wowzers") {
		t.Errorf("GET / status=%d, index not served", res.StatusCode)
	}
	res, err = http.Get(srv.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("GET /app.js status=%d", res.StatusCode)
	}

	// Search goes through the mock REST API.
	res, err = http.Get(srv.URL + "/api/search?q=verity")
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Tracks []Track `json:"tracks"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 200 || len(out.Tracks) != 2 {
		t.Errorf("search status=%d tracks=%d, want 2 (decoy + exact)", res.StatusCode, len(out.Tracks))
	}

	// Zip of unknown ids -> clean 404 (no panic with the nil client).
	res, err = http.Get(srv.URL + "/api/download/zip?ids=999")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Errorf("zip unknown-id status=%d, want 404", res.StatusCode)
	}

	// CORS preflight must survive (the PWA is served same-origin, but the
	// wrapper is part of the wiring).
	req, _ := http.NewRequest(http.MethodOptions, srv.URL+"/api/search", nil)
	res, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 204 {
		t.Errorf("OPTIONS status=%d, want 204", res.StatusCode)
	}
}

func TestWriteZipEntryDedup(t *testing.T) {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	used := map[string]int{}
	tf := trackFile{Data: []byte("audio"), Ext: ".mp3", Title: "Same Song", Artist: "Same Artist"}
	// Same position + same name twice exercises the dedup suffix.
	if err := writeZipEntry(zw, 1, tf, used); err != nil {
		t.Fatal(err)
	}
	if err := writeZipEntry(zw, 1, tf, used); err != nil {
		t.Fatal(err)
	}
	zw.Close()

	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 2 {
		t.Fatalf("zip entries = %d, want 2", len(zr.File))
	}
	if zr.File[0].Name == zr.File[1].Name {
		t.Errorf("duplicate zip entry names: %q", zr.File[0].Name)
	}
	if !strings.Contains(zr.File[1].Name, "(2)") {
		t.Errorf("second entry name = %q, want a (2) suffix", zr.File[1].Name)
	}
	data, err := readZipEntry(zr.File[0])
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "audio" {
		t.Errorf("entry data = %q", data)
	}
}

func readZipEntry(f *zip.File) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(rc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
