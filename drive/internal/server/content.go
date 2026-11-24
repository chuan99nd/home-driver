package server

import (
	"archive/zip"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	_ "image/gif" // thumbnail decoders
	"image/jpeg"
	_ "image/png"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/chuan99nd/home-driver/drive/internal/access"
)

// User content is never served with a type the browser would execute on our
// origin. The tables below are an allowlist: anything not listed is sent as
// application/octet-stream with Content-Disposition: attachment.

// inlineTypes may be displayed in the browser as-is.
var inlineTypes = map[string]struct{ ctype, kind string }{
	".jpg": {"image/jpeg", "image"}, ".jpeg": {"image/jpeg", "image"}, ".png": {"image/png", "image"},
	".gif": {"image/gif", "image"}, ".webp": {"image/webp", "image"}, ".avif": {"image/avif", "image"},
	".bmp": {"image/bmp", "image"}, ".ico": {"image/x-icon", "image"},
	".mp4": {"video/mp4", "video"}, ".m4v": {"video/mp4", "video"}, ".webm": {"video/webm", "video"},
	".mov": {"video/quicktime", "video"}, ".ogv": {"video/ogg", "video"},
	".mp3": {"audio/mpeg", "audio"}, ".m4a": {"audio/mp4", "audio"}, ".aac": {"audio/aac", "audio"},
	".ogg": {"audio/ogg", "audio"}, ".oga": {"audio/ogg", "audio"}, ".opus": {"audio/ogg", "audio"},
	".wav": {"audio/wav", "audio"}, ".flac": {"audio/flac", "audio"},
	".pdf": {"application/pdf", "pdf"},
}

// textExts are shown as text/plain, whatever they contain. This includes
// HTML and SVG: they are displayed as source, never rendered.
var textExts = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".log": true, ".csv": true, ".tsv": true, ".json": true,
	".xml": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true, ".conf": true, ".cfg": true,
	".env": true, ".html": true, ".htm": true, ".xhtml": true, ".svg": true, ".css": true, ".js": true,
	".mjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".go": true, ".py": true, ".rb": true, ".rs": true,
	".java": true, ".kt": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true, ".cs": true,
	".php": true, ".sh": true, ".bash": true, ".zsh": true, ".sql": true, ".lua": true, ".swift": true,
	".tex": true, ".srt": true, ".vtt": true, ".diff": true, ".patch": true, ".mod": true, ".sum": true,
}

// downloadTypes are friendlier content types for attachments only.
var downloadTypes = map[string]string{
	".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar", ".7z": "application/x-7z-compressed",
	".doc": "application/msword", ".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".xls": "application/vnd.ms-excel", ".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt": "application/vnd.ms-powerpoint", ".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
	".heic": "image/heic", ".heif": "image/heif", ".mkv": "video/x-matroska", ".avi": "video/x-msvideo",
}

func extOf(name string) string { return strings.ToLower(path.Ext(name)) }

// kindOf tells the UI how a file can be previewed.
func kindOf(name string) string {
	ext := extOf(name)
	if t, ok := inlineTypes[ext]; ok {
		return t.kind
	}
	if textExts[ext] {
		return "text"
	}
	switch ext {
	case ".zip", ".gz", ".tar", ".7z", ".rar", ".bz2", ".xz":
		return "archive"
	case ".doc", ".docx", ".odt", ".rtf", ".xls", ".xlsx", ".ods", ".ppt", ".pptx", ".odp":
		return "doc"
	case ".heic", ".heif", ".tif", ".tiff", ".raw", ".dng":
		return "image-file"
	case ".mkv", ".avi", ".wmv", ".flv":
		return "video-file"
	}
	return "file"
}

func canThumb(name string) bool {
	switch extOf(name) {
	case ".jpg", ".jpeg", ".png", ".gif":
		return true
	}
	return false
}

// contentDisposition builds the header value with an ASCII fallback and the
// RFC 5987 / RFC 6266 UTF-8 form.
func contentDisposition(disposition, name string) string {
	var ascii strings.Builder
	for _, c := range name {
		if c < 0x20 || c > 0x7e || c == '"' || c == '\\' || c == '%' || c == ';' {
			ascii.WriteByte('_')
		} else {
			ascii.WriteRune(c)
		}
	}
	return disposition + `; filename="` + ascii.String() + `"; filename*=UTF-8''` + rfc5987(name)
}

func rfc5987(s string) string {
	// attr-char of RFC 5987: escape everything else.
	return strings.NewReplacer("+", "%20", "*", "%2A", "%7E", "~").Replace(url.QueryEscape(s))
}

// contentHeaders sets the security headers for user content.
func contentHeaders(w http.ResponseWriter, pdf bool) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	h.Set("X-Frame-Options", "SAMEORIGIN")
	csp := "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'self'"
	if pdf {
		// The browser's built-in PDF viewer does not run under a CSP sandbox.
		csp += "; object-src 'self'; img-src 'self' data:"
	} else {
		csp += "; sandbox"
	}
	h.Set("Content-Security-Policy", csp)
	h.Set("Cache-Control", "private, no-cache")
}

// serveFile streams a file node. The caller has authorized the node.
// http.ServeContent provides Range, If-Range and If-None-Match handling.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request, n *access.Node) {
	s.serveFileAs(w, r, n, r.URL.Query().Get("download") == "1")
}

// serveFileAs is serveFile with the attachment decision made by the caller.
func (s *Server) serveFileAs(w http.ResponseWriter, r *http.Request, n *access.Node, download bool) {
	if n.IsDir {
		s.fail(w, http.StatusBadRequest, "is_folder", "this is a folder; download it as a zip")
		return
	}
	f, err := s.blobs.Open(n.BlobSHA)
	if err != nil {
		s.internal(w, r, fmt.Errorf("open blob for node %s: %w", n.ID, err))
		return
	}
	defer f.Close()

	ext := extOf(n.Name)
	ctype, disposition := "application/octet-stream", "attachment"
	if t, ok := inlineTypes[ext]; ok {
		ctype = t.ctype
		if !download {
			disposition = "inline"
		}
	} else if textExts[ext] {
		// Shown as plain text when previewed; as an opaque download otherwise.
		if !download {
			ctype, disposition = "text/plain; charset=utf-8", "inline"
		}
	} else if t, ok := downloadTypes[ext]; ok {
		ctype = t
	}
	contentHeaders(w, ctype == "application/pdf")
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", contentDisposition(disposition, n.Name))
	w.Header().Set("ETag", `"`+n.BlobSHA+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) handleContent(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.serveFile(w, r, res.Node)
}

func (s *Server) handleThumb(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.serveThumb(w, r, res.Node)
}

func (s *Server) handleZip(w http.ResponseWriter, r *http.Request, u *User) {
	res, ok := s.authz(w, r, u.principal(), r.PathValue("id"), access.Viewer)
	if !ok {
		return
	}
	s.serveZip(w, r, res.Node)
}

// ---- zip ----

type zipEntry struct {
	path  string
	sha   string // "" for a folder
	mtime int64
}

// collectTree lists everything live under a folder. The caller has
// authorized the folder; descendants inherit that access.
func (s *Server) collectTree(ctx context.Context, root *access.Node) ([]zipEntry, error) {
	type dir struct{ id, path string }
	queue := []dir{{root.ID, ""}}
	var out []zipEntry
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, name, is_dir, COALESCE(blob_sha, ''), updated_at FROM nodes WHERE parent_id = ? AND trashed_at IS NULL ORDER BY name`, d.id)
		if err != nil {
			return nil, fmt.Errorf("walk folder: %w", err)
		}
		for rows.Next() {
			var id, name, sha string
			var isDir bool
			var mtime int64
			if err := rows.Scan(&id, &name, &isDir, &sha, &mtime); err != nil {
				rows.Close()
				return nil, fmt.Errorf("walk folder: %w", err)
			}
			p := d.path + name
			if isDir {
				out = append(out, zipEntry{path: p + "/", mtime: mtime})
				queue = append(queue, dir{id, p + "/"})
			} else {
				out = append(out, zipEntry{path: p, sha: sha, mtime: mtime})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("walk folder: %w", err)
		}
	}
	return out, nil
}

// serveZip streams a folder as an uncompressed zip. The file list is read
// first so no database connection is held while streaming.
func (s *Server) serveZip(w http.ResponseWriter, r *http.Request, n *access.Node) {
	if !n.IsDir {
		s.fail(w, http.StatusBadRequest, "not_a_folder", "only folders can be downloaded as a zip")
		return
	}
	entries, err := s.collectTree(r.Context(), n)
	if err != nil {
		s.internal(w, r, err)
		return
	}
	contentHeaders(w, false)
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", contentDisposition("attachment", n.Name+".zip"))
	zw := zip.NewWriter(w)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.path, Method: zip.Store, Modified: time.Unix(e.mtime, 0)}
		fw, err := zw.CreateHeader(hdr)
		if err == nil && e.sha != "" {
			var f *os.File
			if f, err = s.blobs.Open(e.sha); err == nil {
				_, err = io.Copy(fw, f)
				f.Close()
			}
		}
		if err != nil {
			// Headers are sent; the only honest signal left is a broken stream.
			s.log.Error("zip download aborted", "node", n.ID, "err", err)
			panic(http.ErrAbortHandler)
		}
	}
	if err := zw.Close(); err != nil {
		s.log.Debug("zip close", "err", err)
	}
}

// ---- thumbnails ----

const (
	thumbSize      = 320
	thumbMaxPixels = 80 << 20 // refuse to decode absurd images
	thumbMaxBytes  = 100 << 20
)

func (s *Server) thumbPath(sha string) string {
	return filepath.Join(s.thumbs, sha[:2], sha+".jpg")
}

// serveThumb serves a small JPEG for an image, generating and caching it on
// first use. Thumbnails are a cache keyed by content hash; they are safe to
// delete at any time.
func (s *Server) serveThumb(w http.ResponseWriter, r *http.Request, n *access.Node) {
	if n.IsDir || !canThumb(n.Name) || n.Size > thumbMaxBytes {
		s.fail(w, http.StatusNotFound, "no_thumbnail", "no thumbnail")
		return
	}
	p := s.thumbPath(n.BlobSHA)
	f, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		select {
		case s.thumbSem <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		err = s.makeThumb(n.BlobSHA, p)
		<-s.thumbSem
		if err != nil {
			s.log.Debug("thumbnail failed", "node", n.ID, "err", err)
			s.fail(w, http.StatusNotFound, "no_thumbnail", "no thumbnail")
			return
		}
		f, err = os.Open(p)
	}
	if err != nil {
		s.internal(w, r, fmt.Errorf("open thumbnail: %w", err))
		return
	}
	defer f.Close()
	contentHeaders(w, false)
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("ETag", `"t-`+n.BlobSHA+`"`)
	http.ServeContent(w, r, "", time.Time{}, f)
}

func (s *Server) makeThumb(sha, dst string) error {
	f, err := s.blobs.Open(sha)
	if err != nil {
		return err
	}
	defer f.Close()
	cfg, format, err := image.DecodeConfig(f)
	if err != nil {
		return fmt.Errorf("read image header: %w", err)
	}
	if cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > thumbMaxPixels {
		return fmt.Errorf("image too large: %dx%d", cfg.Width, cfg.Height)
	}
	orient := 1
	if format == "jpeg" {
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
		orient = exifOrientation(f)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	src, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("decode image: %w", err)
	}
	thumb := shrink(src, thumbSize, orient)
	if err := os.MkdirAll(filepath.Dir(dst), 0o750); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "thumb-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := jpeg.Encode(tmp, thumb, &jpeg.Options{Quality: 80}); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), dst)
}

// shrink scales src so its longer side is at most max, averaging up to 3x3
// samples per output pixel, and applies the EXIF orientation.
func shrink(src image.Image, max, orient int) image.Image {
	b := src.Bounds()
	sw, sh := b.Dx(), b.Dy()
	scale := 1.0
	if sw > max || sh > max {
		if sw >= sh {
			scale = float64(max) / float64(sw)
		} else {
			scale = float64(max) / float64(sh)
		}
	}
	tw, th := int(float64(sw)*scale+0.5), int(float64(sh)*scale+0.5)
	if tw < 1 {
		tw = 1
	}
	if th < 1 {
		th = 1
	}
	ow, oh := tw, th
	if orient >= 5 { // orientations 5-8 swap the axes
		ow, oh = th, tw
	}
	dst := image.NewRGBA(image.Rect(0, 0, ow, oh))
	const taps = 3
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			var r, g, bl, n uint32
			for j := 0; j < taps; j++ {
				for i := 0; i < taps; i++ {
					sx := b.Min.X + int((float64(x)+(float64(i)+0.5)/taps)/scale)
					sy := b.Min.Y + int((float64(y)+(float64(j)+0.5)/taps)/scale)
					if sx >= b.Max.X {
						sx = b.Max.X - 1
					}
					if sy >= b.Max.Y {
						sy = b.Max.Y - 1
					}
					cr, cg, cb, ca := src.At(sx, sy).RGBA()
					// Composite on white for transparent images.
					r += cr + (0xffff - ca)
					g += cg + (0xffff - ca)
					bl += cb + (0xffff - ca)
					n++
				}
			}
			c := color.RGBA{uint8(r / n >> 8), uint8(g / n >> 8), uint8(bl / n >> 8), 0xff}
			dx, dy := x, y
			switch orient {
			case 2:
				dx = tw - 1 - x
			case 3:
				dx, dy = tw-1-x, th-1-y
			case 4:
				dy = th - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = th-1-y, x
			case 7:
				dx, dy = th-1-y, tw-1-x
			case 8:
				dx, dy = y, tw-1-x
			}
			dst.SetRGBA(dx, dy, c)
		}
	}
	return dst
}

// exifOrientation reads the EXIF orientation tag (1-8) of a JPEG, returning
// 1 when there is none or the data is malformed.
func exifOrientation(r io.Reader) int {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:2]); err != nil || hdr[0] != 0xff || hdr[1] != 0xd8 {
		return 1
	}
	for {
		if _, err := io.ReadFull(r, hdr[:]); err != nil || hdr[0] != 0xff {
			return 1
		}
		size := int(binary.BigEndian.Uint16(hdr[2:])) - 2
		if size < 0 || hdr[1] == 0xda { // start of scan: no EXIF before image data
			return 1
		}
		if hdr[1] != 0xe1 {
			if _, err := io.CopyN(io.Discard, r, int64(size)); err != nil {
				return 1
			}
			continue
		}
		seg := make([]byte, size)
		if _, err := io.ReadFull(r, seg); err != nil {
			return 1
		}
		if len(seg) < 14 || string(seg[:6]) != "Exif\x00\x00" {
			continue
		}
		t := seg[6:]
		var bo binary.ByteOrder = binary.BigEndian
		if string(t[:2]) == "II" {
			bo = binary.LittleEndian
		}
		off := int(bo.Uint32(t[4:8]))
		if off < 8 || off+2 > len(t) {
			return 1
		}
		count := int(bo.Uint16(t[off:]))
		for i := 0; i < count; i++ {
			e := off + 2 + i*12
			if e+12 > len(t) {
				return 1
			}
			if bo.Uint16(t[e:]) == 0x0112 {
				if v := int(bo.Uint16(t[e+8:])); v >= 1 && v <= 8 {
					return v
				}
				return 1
			}
		}
		return 1
	}
}
