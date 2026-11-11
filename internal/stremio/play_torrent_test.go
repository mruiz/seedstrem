package stremio

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/kipsilabs/seedstrem/internal/downloader/fake"
	"github.com/kipsilabs/seedstrem/internal/metainfo"
	"github.com/kipsilabs/seedstrem/internal/prowlarr"
)

// An infohash-only release has a tracker-less magnet; play must fetch the
// remembered .torrent and add that instead, or a private torrent never
// gets metadata.
func TestPlayFetchesDeferredTorrentFile(t *testing.T) {
	h := newHarness(t)

	torrentBytes := []byte("d8:announce26:http://tr.example/announce4:infod4:name4:testee")
	hash, _, _, err := metainfo.FromTorrent(torrentBytes)
	if err != nil {
		t.Fatal(err)
	}
	tf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(torrentBytes) }))
	defer tf.Close()

	h.fakeDC.Put(&fake.Torrent{Hash: hash, State: "Paused", Files: []fake.File{{Name: "test.mkv", Size: 1 << 30}}})
	h.handler.torrentFiles.putURL(hash, tf.URL)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	magnet := "magnet:?xt=urn:btih:" + hash + "&dn=test"
	resp, err := client.Get(h.server.URL + "/stremio/play/" + hash + "?magnet=" + url.QueryEscape(magnet))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	var viaFile bool
	for _, c := range h.fakeDC.Calls() {
		if strings.HasPrefix(c, "add magnet=") {
			t.Errorf("added tracker-less magnet instead of the .torrent: %v", h.fakeDC.Calls())
		}
		viaFile = viaFile || strings.HasPrefix(c, "add torrentfile="+hash)
	}
	if !viaFile {
		t.Errorf(".torrent not added: %v", h.fakeDC.Calls())
	}
}

// A magnet-less release is listed under a key with no magnet; play must
// download its .torrent only then, and only once across HEAD and GET.
func TestPlayResolvesMagnetLessRelease(t *testing.T) {
	h := newHarness(t)

	torrentBytes := []byte("d8:announce26:http://tr.example/announce4:infod4:name4:testee")
	hash, _, _, err := metainfo.FromTorrent(torrentBytes)
	if err != nil {
		t.Fatal(err)
	}
	var fetches atomic.Int32
	tf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches.Add(1)
		w.Write(torrentBytes)
	}))
	defer tf.Close()

	h.fakeDC.Put(&fake.Torrent{Hash: hash, State: "Paused", Files: []fake.File{{Name: "test.mkv", Size: 1 << 30}}})
	res := prowlarr.Result{Title: "test", DownloadURL: tf.URL}
	key := playKey(res)
	if len(key) != 64 {
		t.Fatalf("playKey = %q, want a 64-char key distinct from an infohash", key)
	}
	h.handler.torrentFiles.putURL(key, res.DownloadURL)

	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, _ := http.NewRequest(method, h.server.URL+"/stremio/play/"+key, nil)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound {
			t.Fatalf("%s play = %d, want 302", method, resp.StatusCode)
		}
	}
	if n := fetches.Load(); n != 1 {
		t.Errorf(".torrent fetches = %d, want 1", n)
	}
	var viaFile bool
	for _, c := range h.fakeDC.Calls() {
		viaFile = viaFile || strings.HasPrefix(c, "add torrentfile="+hash)
	}
	if !viaFile {
		t.Errorf(".torrent not added: %v", h.fakeDC.Calls())
	}

	if resp, err := client.Get(h.server.URL + "/stremio/play/" + strings.Repeat("0", 64)); err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown key: resp=%v err=%v, want 400", resp, err)
	}
}

// Listing never downloads a .torrent: a magnet-less release is offered
// under a key whose play fetches it.
func TestStreamListsMagnetLessWithoutFetching(t *testing.T) {
	h := newHarness(t)
	var fetches atomic.Int32
	prow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/indexer":
			w.Write([]byte(`[{"id":1,"name":"idx1","protocol":"torrent","enable":true,
				"capabilities":{"movieSearchParams":["Q","ImdbId"]}}]`))
		case "/dl.torrent":
			fetches.Add(1)
			http.NotFound(w, r)
		default:
			w.Write([]byte(`[{"title":"The Matrix 1999 2160p","downloadUrl":"http://` + r.Host + `/dl.torrent","size":20000000000,"seeders":5,"protocol":"torrent","indexer":"idx1"}]`))
		}
	}))
	defer prow.Close()
	base := h.handler.settings()
	h.handler.settings = func() Settings {
		s := base
		s.Prowlarr.URL = prow.URL
		return s
	}

	resp, err := http.Get(h.server.URL + "/stremio/stream/movie/tt1375666.json")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var sr streamResponse
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if len(sr.Streams) != 1 {
		t.Fatalf("want 1 stream, got %+v", sr.Streams)
	}
	key := playKey(prowlarr.Result{DownloadURL: prow.URL + "/dl.torrent"})
	if u := sr.Streams[0].URL; !strings.Contains(u, "/stremio/play/"+key) || strings.Contains(u, "magnet=") || strings.Contains(u, "dl.torrent") {
		t.Errorf("play URL = %q, want the key, no magnet and no download link", u)
	}
	if n := fetches.Load(); n != 0 {
		t.Errorf("listing fetched %d .torrent(s), want 0", n)
	}
	if got := h.handler.torrentFiles.getURL(key); got != prow.URL+"/dl.torrent" {
		t.Errorf("download link not remembered for play: %q", got)
	}
}
