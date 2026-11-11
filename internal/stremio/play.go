package stremio

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/kipsilabs/seedstrem/internal/metainfo"
	"github.com/kipsilabs/seedstrem/internal/prowlarr"
	"github.com/kipsilabs/seedstrem/internal/torrents"
)

// play handles GET|HEAD /play/{infohash} — the resolve half. It adds the
// magnet to qBittorrent, waits for metadata, selects the matching file,
// and 302-redirects to the /dl streaming endpoint.
func (h *Handler) play(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := h.settings()
	infohash := strings.ToLower(chi.URLParam(r, "infohash"))
	magnet := r.URL.Query().Get("magnet")

	var torrentFile []byte
	if magnet == "" {
		// A magnet-less release was listed unresolved under a key (see
		// playKey): only now, at play, is its .torrent downloaded.
		res, ok := h.resolveAtPlay(r, s, infohash)
		if !ok {
			http.Error(w, "missing magnet", http.StatusBadRequest)
			return
		}
		infohash, magnet, torrentFile = res.InfoHash, res.MagnetURL, res.TorrentFile
	} else if hash, _, err := metainfo.FromMagnet(magnet); err != nil || hash != infohash {
		// Guard: the magnet must match the path infohash.
		http.Error(w, "magnet does not match infohash", http.StatusBadRequest)
		return
	}

	sel := torrents.Selector{
		// Content identity (empty when the stream URL predates it) — used
		// only to persist what this torrent was added for, so later stream
		// requests can surface it as already-owned.
		Source:     r.URL.Query().Get("src"),
		ContentRef: r.URL.Query().Get("cid"),
		// Prowlarr indexer this release came from (empty for stream URLs
		// that predate it) — persisted so cleanup can apply the indexer's
		// own seed time.
		Indexer:   r.URL.Query().Get("ix"),
		UserAgent: r.UserAgent(),
	}
	if r.URL.Query().Get("series") == "1" {
		sel.IsSeries = true
		sel.Season, _ = strconv.Atoi(r.URL.Query().Get("s"))
		sel.Episode, _ = strconv.Atoi(r.URL.Query().Get("e"))
	}
	h.logger.Debug("stremio: play resolve",
		"infohash", infohash, "series", sel.IsSeries, "season", sel.Season, "episode", sel.Episode)

	// If we cached the raw .torrent for this infohash while building the
	// stream list, add it directly (skips qBittorrent's metadata fetch);
	// otherwise fall back to the magnet.
	if torrentFile == nil {
		torrentFile = h.torrentFiles.get(infohash)
	}
	if torrentFile == nil {
		torrentFile = h.fetchTorrentFile(r, s, infohash)
	}
	link, err := h.svc.Resolve(ctx, magnet, torrentFile, sel)
	if err != nil {
		switch {
		case errors.Is(err, torrents.ErrMetadataTimeout):
			w.Header().Set("Retry-After", "5")
			http.Error(w, "torrent metadata not ready yet", http.StatusServiceUnavailable)
		case errors.Is(err, torrents.ErrNoFileMatch):
			http.Error(w, "no matching file in torrent", http.StatusNotFound)
		default:
			h.logger.Error("stremio: resolve failed", "infohash", infohash, "error", err)
			http.Error(w, "failed to resolve stream", http.StatusBadGateway)
		}
		return
	}

	target := strings.TrimSuffix(s.ExternalURL, "/") + "/dl/" + link.Token
	h.logger.Debug("stremio: play resolved",
		"infohash", infohash, "file", link.Path, "token", link.Token)
	http.Redirect(w, r, target, http.StatusFound)
}

// resolveAtPlay downloads the .torrent remembered under key (a playKey of
// an unresolved release) and derives its infohash and magnet. The bytes
// are cached under key too, so a HEAD probe followed by the GET, or a
// replay, costs a single grab. ok is false when the key is unknown
// (expired, or the server restarted since the listing) or the fetch fails.
func (h *Handler) resolveAtPlay(r *http.Request, s Settings, key string) (prowlarr.Result, bool) {
	u := h.torrentFiles.getURL(key)
	raw := h.torrentFiles.get(key)
	if u == "" && raw == nil {
		return prowlarr.Result{}, false
	}
	// Resolve drops a release it cannot fetch and never logs the link,
	// which carries the Prowlarr API key.
	res := h.prowlarr(s).Resolve(r.Context(), []prowlarr.Result{{DownloadURL: u, TorrentFile: raw}})
	if len(res) == 0 {
		if r.Context().Err() == nil {
			h.logger.Warn("stremio: .torrent fetch for play failed", "key", key)
		}
		return prowlarr.Result{}, false
	}
	h.torrentFiles.put(key, res[0].TorrentFile)
	h.torrentFiles.put(res[0].InfoHash, res[0].TorrentFile)
	return res[0], true
}

// fetchTorrentFile downloads the .torrent remembered for infohash (an
// infohash-only release whose magnet has no trackers). Best-effort: on
// any failure play falls back to the magnet.
func (h *Handler) fetchTorrentFile(r *http.Request, s Settings, infohash string) []byte {
	u := h.torrentFiles.getURL(infohash)
	if u == "" {
		return nil
	}
	raw, hash, err := h.prowlarr(s).FetchTorrent(r.Context(), u)
	if r.Context().Err() != nil {
		return nil // player hung up; nothing to report
	}
	if err != nil || hash != infohash {
		// The download link carries the Prowlarr API key: never log it.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		h.logger.Warn("stremio: .torrent fetch for play failed, using magnet", "infohash", infohash, "gotHash", hash, "error", err)
		return nil
	}
	h.torrentFiles.put(infohash, raw)
	return raw
}
