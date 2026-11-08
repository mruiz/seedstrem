package stremio

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/kipsilabs/seedstrem/internal/downloader/fake"
	"github.com/kipsilabs/seedstrem/internal/metainfo"
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
