// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creachadair/taskgroup"
	"github.com/fogleman/gg"
	"github.com/golang/freetype/truetype"
	"github.com/tailscale/tmemes"
	"github.com/tailscale/tmemes/store"
	"golang.org/x/exp/slices"
	"golang.org/x/image/font"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/metrics"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/tsweb"
	"tailscale.com/util/singleflight"
)

type tmemeServer struct {
	db             *store.DB
	srv            *tsnet.Server
	lc             *tailscale.LocalClient
	superUser      map[string]bool // logins of admin users
	allowAnonymous bool

	macroGenerationSingleFlight singleflight.Group[string, string]
	imageFileEtags              sync.Map // :: string(path) → string(quoted etag)

	mu sync.Mutex // guards userProfiles

	userProfiles            map[tailcfg.UserID]tailcfg.UserProfile
	lastUpdatedUserProfiles time.Time
}

// initialize sets up the state of the server and checks the integrity of its
// database to make it ready to serve. Any error it reports is considered
// fatal.
func (s *tmemeServer) initialize(ts *tsnet.Server) error {
	// Populate superusers.
	if *adminUsers != "" {
		s.superUser = make(map[string]bool)
		for _, u := range strings.Split(*adminUsers, ",") {
			s.superUser[u] = true
		}
	}

	// Preload Etag values.
	var numTags int
	for _, t := range s.db.Templates() {
		tag, err := makeFileEtag(t.Path)
		if err != nil {
			return err
		}
		s.imageFileEtags.Store(t.Path, tag)
		numTags++
	}
	for _, m := range s.db.Macros() {
		cachePath, _ := s.db.CachePath(m)
		tag, err := makeFileEtag(cachePath)
		if os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		s.imageFileEtags.Store(cachePath, tag)
		numTags++
	}
	log.Printf("Preloaded %d image Etags", numTags)

	// Set up a metrics server.
	ln, err := ts.Listen("tcp", ":8383")
	if err != nil {
		return err
	}
	go func() {
		defer ln.Close()
		log.Print("Starting debug server on :8383")
		mux := http.NewServeMux()
		tsweb.Debugger(mux)
		http.Serve(ln, mux)
	}()

	// Enable the Slack integration.
	if *enableSlackBot {
		go startSlackBot()
	}
	return nil
}

var (
	serveMetrics = &metrics.LabelMap{Label: "type"}
	macroMetrics = &metrics.LabelMap{Label: "type"}
)

// Preloaded font definition.
var (
	oswaldSemiBold *truetype.Font
)

func init() {
	expvar.Publish("tmemes_serve_metrics", serveMetrics)
	expvar.Publish("tmemes_macro_metrics", macroMetrics)

	// Preload and parse the font definition, so we can reuse it.
	fontBytes, err := staticFS.ReadFile("static/font/Oswald-SemiBold.ttf")
	if err != nil {
		panic(fmt.Sprintf("Loading font: %v", err))
	}
	oswaldSemiBold, err = truetype.Parse(fontBytes)
	if err != nil {
		panic(fmt.Sprintf("Parsing font: %v", err))
	}
}

var errNotFound = errors.New("not found")

// userFromID returns the user profile for the given user ID.  If the user
// profile is not found, it will attempt to fetch the latest user profiles from
// the tsnet server.
func (s *tmemeServer) userFromID(ctx context.Context, id tailcfg.UserID) (*tailcfg.UserProfile, error) {
	s.mu.Lock()
	up, ok := s.userProfiles[id]
	lastUpdated := s.lastUpdatedUserProfiles
	s.mu.Unlock()
	if ok {
		return &up, nil
	}
	if time.Since(lastUpdated) < time.Minute {
		return nil, errNotFound
	}
	st, err := s.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userProfiles = st.User
	up, ok = s.userProfiles[id]
	if !ok {
		return nil, errNotFound
	}
	return &up, nil
}

// newMux constructs a router for the tmemes API.
//
// There are three groups of endpoints:
//
//   - The /api/ endpoints serve JSON metadata for tools to consume.
//   - The /content/ endpoints serve image data.
//   - The rest of the endpoints serve UI components.
func (s *tmemeServer) newMux() *http.ServeMux {
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/api/macro/", s.serveAPIMacro)       // one macro by ID
	apiMux.HandleFunc("/api/macro", s.serveAPIMacro)        // all macros
	apiMux.HandleFunc("/api/template/", s.serveAPITemplate) // one template by ID
	apiMux.HandleFunc("/api/template", s.serveAPITemplate)  // all templates
	apiMux.HandleFunc("/api/vote/", s.serveAPIVote)         // caller's vote by ID
	apiMux.HandleFunc("/api/vote", s.serveAPIVote)          // all caller's votes

	contentMux := http.NewServeMux()
	contentMux.HandleFunc("/content/template/", s.serveContentTemplate)
	contentMux.HandleFunc("/content/macro/", s.serveContentMacro)

	uiMux := http.NewServeMux()
	uiMux.HandleFunc("/macros/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/m/"+r.URL.Path[len("/macros/"):], http.StatusFound)
	})
	uiMux.HandleFunc("/templates/", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/t/"+r.URL.Path[len("/templates/"):], http.StatusFound)
	})
	uiMux.HandleFunc("/t/", s.serveUITemplates)   // view one template by ID
	uiMux.HandleFunc("/t", s.serveUITemplates)    // view all templates
	uiMux.HandleFunc("/create/", s.serveUICreate) // view create page for given template ID
	uiMux.HandleFunc("/m/", s.serveUIMacros)      // view one macro by ID
	uiMux.HandleFunc("/m", s.serveUIMacros)       // view all macros
	uiMux.HandleFunc("/", s.serveUIMacros)        // alias for /macros/
	uiMux.HandleFunc("/upload", s.serveUIUpload)  // template upload view

	mux := http.NewServeMux()
	mux.Handle("/api/", apiMux)
	mux.Handle("/content/", contentMux)
	mux.Handle("/static/", http.FileServer(http.FS(staticFS)))
	mux.Handle("/", uiMux)

	return mux
}

// serveContentTemplate serves template image content.
//
// API: /content/template/:id[.ext]
//
// A file extension is optional, but if .ext is included, it must match the
// stored value.
func (s *tmemeServer) serveContentTemplate(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("content-template", 1)
	const apiPath = "/content/template/"
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Require /id or /id.ext.
	id := strings.TrimPrefix(r.URL.Path, apiPath)
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ext := filepath.Ext(id)
	idInt, err := strconv.Atoi(strings.TrimSuffix(id, ext))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	t, err := s.db.Template(idInt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	// Require that the requested extension match how the file is stored.
	if !strings.HasSuffix(t.Path, ext) {
		http.Error(w, "wrong file extension", http.StatusBadRequest)
		return
	}

	s.serveFileCached(w, r, t.Path, 365*24*time.Hour)
}

// serveContentMacro serves macro image content. If the requested macro is not
// already in the cache, it is rendered and cached before returning.
//
// API: /content/macro/:id[.ext]
//
// A file extension is optional, but if .ext is included, it must match the
// file extension stored with the macro's template.
func (s *tmemeServer) serveContentMacro(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("content-macro", 1)
	const apiPath = "/content/macro/"
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Require /id or /id.ext
	id := strings.TrimPrefix(r.URL.Path, apiPath)
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	ext := filepath.Ext(id)
	idInt, err := strconv.Atoi(strings.TrimSuffix(id, ext))
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}

	m, err := s.db.Macro(idInt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	cachePath, err := s.db.CachePath(m)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// Require that the requested extension (if there is one) match how the file
	// is stored.
	if ext != "" && !strings.HasSuffix(cachePath, ext) {
		http.Error(w, "wrong file extension", http.StatusBadRequest)
		return
	}

	if _, err := os.Stat(cachePath); err == nil {
		macroMetrics.Add("cache-hit", 1)
		s.serveFileCached(w, r, cachePath, 24*time.Hour)
		return
	} else {
		log.Printf("cache file %q not found, generating: %v", cachePath, err)
	}
	if _, err, reused := s.macroGenerationSingleFlight.Do(cachePath, func() (string, error) {
		macroMetrics.Add("cache-miss", 1)
		return cachePath, s.generateMacro(m, cachePath)
	}); err != nil {
		log.Printf("error generating macro %d: %v", m.ID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	} else if reused {
		macroMetrics.Add("cache-reused", 1)
	}

	s.serveFileCached(w, r, cachePath, 24*time.Hour)
}

// serveFileCached is a wrapper for http.ServeFile that populates cache-control
// and etag headers.
func (s *tmemeServer) serveFileCached(w http.ResponseWriter, r *http.Request, path string, maxAge time.Duration) {
	w.Header().Set("Cache-Control", fmt.Sprintf(
		"public, max-age=%d, no-transform", maxAge/time.Second))
	if tag, ok := s.imageFileEtags.Load(path); ok {
		w.Header().Set("Etag", tag.(string))
	}
	http.ServeFile(w, r, path)
}

// generateMacroGIF renders the text specified by m onto the template GIF
// stored in srcFile. On success it writes the generated macro to cachePath.
//
// If srcFile contains multiple frames, it renders the text onto each frame
// according to the timing and position settings defined in its overlay.
func (s *tmemeServer) generateMacroGIF(m *tmemes.Macro, cachePath string, srcFile *os.File) (retErr error) {
	macroMetrics.Add("generate-gif", 1)
	log.Printf("generating GIF for macro %d", m.ID)
	start := time.Now()
	defer func() {
		if retErr != nil {
			log.Printf("error generating GIF for macro %d: %v", m.ID, retErr)
		} else {
			log.Printf("generated GIF for macro %d in %v", m.ID, time.Since(start).Round(time.Millisecond))
		}
	}()
	// Decode the source GIF
	srcGif, err := gif.DecodeAll(srcFile)
	if err != nil {
		return err
	}

	if len(srcGif.Image) == 0 {
		return errors.New("no frames in GIF")
	}

	lineFrames := make([]frames, len(m.TextOverlay))
	for i, tl := range m.TextOverlay {
		lineFrames[i] = newFrames(len(srcGif.Image), tl)
	}

	g, run := taskgroup.New(nil).Limit(runtime.NumCPU())
	bounds := imageBounds(srcGif)

	// Phase 1: Render all the frames into a bounding box big enough to hold all
	// of them, keeping their relative position.
	frames := make([]image.Image, len(srcGif.Image))
	log.Printf("Begin rendering %d frames with %d overlays", len(frames), len(m.TextOverlay))
	rStart := time.Now()
	for i := range srcGif.Image {
		i, frame := i, srcGif.Image[i]
		run(func() error {
			fb := frame.Bounds()
			img := image.NewRGBA(bounds)
			draw.Draw(img, fb, frame, fb.Min, draw.Over)

			dc := gg.NewContext(bounds.Dx(), bounds.Dy())
			for _, f := range lineFrames {
				if !f.visibleAt(i) {
					continue
				} else if err := s.overlayTextOnImage(dc, f.frame(i), bounds); err != nil {
					return err
				}
			}
			text := dc.Image()
			draw.Draw(img, img.Bounds(), text, text.Bounds().Min, draw.Over)
			frames[i] = img
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return err
	}
	log.Printf("Rendering complete [render %v, total %v]",
		time.Since(rStart).Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// Phase 2: Convert the frames back to paletted frames in the GIF.
	eStart := time.Now()
	for i := range srcGif.Image {
		i, frame := i, frames[i]
		run(taskgroup.NoError(func() {
			// Re-generate the frame.
			pt := image.NewPaletted(bounds, makeColorPalette(frame, 8))
			draw.Draw(pt, bounds, frame, frame.Bounds().Min, draw.Over)
			srcGif.Image[i] = pt
		}))
	}
	if err := g.Wait(); err != nil {
		return err
	}
	log.Printf("Re-encoded %d frames [encode %v, total %v]", len(frames),
		time.Since(eStart).Round(time.Millisecond), time.Since(start).Round(time.Millisecond))

	// Save the modified GIF
	dstFile, err := os.Create(cachePath)
	if err != nil {
		return err
	}
	etagHash := sha256.New()
	dst := io.MultiWriter(etagHash, dstFile)
	defer func() {
		if retErr != nil {
			dstFile.Close()
			os.Remove(cachePath)
		} else {
			s.imageFileEtags.Store(cachePath, formatEtag(etagHash))
		}
	}()

	err = gif.EncodeAll(dst, srcGif)
	if err != nil {
		return err
	}
	return dstFile.Close()
}

// fontSizeForImage computes a recommend font size in points for the given image.
func fontSizeForImage(img image.Image) int {
	const typeHeightFraction = 0.15
	points := int(math.Round((float64(img.Bounds().Dy()) * 0.75) * typeHeightFraction))
	return points
}

// fontForSize constructs a new font.Face for the specified point size.
func (s *tmemeServer) fontForSize(points int) font.Face {
	return truetype.NewFace(oswaldSemiBold, &truetype.Options{
		Size: float64(points),
	})
}

// generateMacro renders the text specified by m onto its template image.  On
// success, it writes the generated macro to cachePath.
//
// Note this method will automatically dispatch to generateMacroGIF for
// templates in GIF format.
func (s *tmemeServer) generateMacro(m *tmemes.Macro, cachePath string) (retErr error) {
	tp, err := s.db.TemplatePath(m.TemplateID)
	if err != nil {
		return err
	}

	srcFile, err := os.Open(tp)
	if err != nil {
		return err
	}
	defer srcFile.Close()

	ext := filepath.Ext(tp)
	if ext == ".gif" {
		return s.generateMacroGIF(m, cachePath, srcFile)
	}
	macroMetrics.Add("generate", 1)

	srcImage, _, err := image.Decode(srcFile)
	if err != nil {
		return err
	}

	dc := gg.NewContext(srcImage.Bounds().Dx(), srcImage.Bounds().Dy())
	bounds := srcImage.Bounds()
	for _, tl := range m.TextOverlay {
		if err := s.overlayTextOnImage(dc, newFrames(1, tl).frame(0), bounds); err != nil {
			return err
		}
	}

	alpha := image.NewNRGBA(bounds)
	draw.Draw(alpha, bounds, srcImage, bounds.Min, draw.Src)
	draw.Draw(alpha, bounds, dc.Image(), bounds.Min, draw.Over)
	f, err := os.Create(cachePath)
	if err != nil {
		return err
	}
	etagHash := sha256.New()
	dst := io.MultiWriter(etagHash, f)
	defer func() {
		if retErr != nil {
			f.Close()
			os.Remove(cachePath)
		} else {
			s.imageFileEtags.Store(cachePath, formatEtag(etagHash))
		}
	}()

	switch ext {
	case ".jpg", ".jpeg":
		macroMetrics.Add("generate-jpg", 1)
		if err := jpeg.Encode(dst, alpha, &jpeg.Options{Quality: 90}); err != nil {
			return err
		}
	case ".png":
		macroMetrics.Add("generate-png", 1)
		if err := png.Encode(dst, alpha); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown extension: %v", ext)
	}

	return f.Close()
}

func oneForZero(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

// overlayTextOnImage paints the specified text line on a single image frame.
func (s *tmemeServer) overlayTextOnImage(dc *gg.Context, tl frame, bounds image.Rectangle) error {
	text := strings.TrimSpace(tl.Text)
	if text == "" {
		return nil
	}

	fontSize := fontSizeForImage(bounds)
	font := s.fontForSize(fontSize)
	dc.SetFontFace(font)

	width := oneForZero(tl.Field[0].Width) * float64(bounds.Dx())
	lineSpacing := 1.25
	x := tl.area().X * float64(bounds.Dx())
	y := tl.area().Y * float64(bounds.Dy())
	ax := 0.5
	ay := 1.0
	fontHeight := dc.FontHeight()
	// Replicate part of the DrawStringWrapped logic so that we can draw the
	// text multiple times to create an outline effect.
	lines := dc.WordWrap(text, width)

	for len(lines) > 2 && fontSize > 6 {
		fontSize--
		font = s.fontForSize(fontSize)
		dc.SetFontFace(font)
		lines = dc.WordWrap(text, width)
	}

	// sync h formula with MeasureMultilineString
	h := float64(len(lines)) * fontHeight * lineSpacing
	h -= (lineSpacing - 1) * fontHeight
	y -= 0.5 * h

	for _, line := range lines {
		c := tl.StrokeColor
		dc.SetRGB(c.R(), c.G(), c.B())

		n := 6 // visible outline size
		for dy := -n; dy <= n; dy++ {
			for dx := -n; dx <= n; dx++ {
				if dx*dx+dy*dy >= n*n {
					// give it rounded corners
					continue
				}
				dc.DrawStringAnchored(line, x+float64(dx), y+float64(dy), ax, ay)
			}
		}

		c = tl.Color
		dc.SetRGB(c.R(), c.G(), c.B())

		dc.DrawStringAnchored(line, x, y, ax, ay)
		y += fontHeight * lineSpacing
	}
	return nil
}

func (s *tmemeServer) serveAPIMacro(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("api-macro", 1)
	switch r.Method {
	case "GET":
		s.serveAPIMacroGet(w, r)
	case "POST":
		s.serveAPIMacroPost(w, r)
	case "DELETE":
		s.serveAPIMacroDelete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// checkAccess checks that the caller is logged in and not a tagged node.  If
// so, it returns the whois data for the user. Otherwise, it writes an error
// response to w and returns nil.
func (s *tmemeServer) checkAccess(w http.ResponseWriter, r *http.Request, op string) *apitype.WhoIsResponse {
	whois, err := s.lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if whois == nil {
		http.Error(w, "not logged in", http.StatusUnauthorized)
		return nil
	}
	if whois.Node.IsTagged() {
		http.Error(w, "tagged nodes cannot "+op, http.StatusForbidden)
		return nil
	}
	return whois
}

// serveAPIMacroPost implements the API for creating new image macros.
//
// API: POST /api/macro
//
// The payload must be of type application/json encoding a tmemes.Macro.  On
// success, the filled-in macro object is written back to the caller.
func (s *tmemeServer) serveAPIMacroPost(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "create macros")
	if whois == nil {
		return // error already sent
	}

	// Create a new macro.
	var m tmemes.Macro
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if err := m.ValidForCreate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// If the creator is negative, treat the macro as anonymous.
	if m.Creator < 0 {
		if !s.allowAnonymous {
			http.Error(w, "anonymous macros not allowed", http.StatusForbidden)
			return
		}
		m.Creator = -1 // normalize anonymous to -1
	} else {
		m.Creator = whois.UserProfile.ID
	}

	if err := s.db.AddMacro(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// creatorUserID parses the "creator" query parameter to identify a user ID for
// which filtering should be done.
//
// If the query parameter is not present, it returns (0, nil).
// If the query parameter is "anon" or "anonymous", it returns (-1, nil).
// Otherwise, on success, it returns a positive user ID, but note that the
// caller is responsible for checking whether that ID corresponds to a real
// user on the tailnet.
func creatorUserID(r *http.Request) (tailcfg.UserID, error) {
	c := r.URL.Query().Get("creator")
	if c == "" {
		return 0, nil
	}
	if c == "anon" || c == "anonymous" {
		return -1, nil
	}
	id, err := strconv.ParseUint(c, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("bad creator: %v", err)
	}
	if id <= 0 {
		return 0, errors.New("invalid creator")
	}
	return tailcfg.UserID(id), nil
}

// serveAPIMacroGet returns metadata about image macros.
//
// API: /api/macro/:id   -- one macro by ID
// API: /api/macro       -- all macros defined
//
// This API supports pagination (see parsePageOptions).
// The result objects are JSON tmemes.Macro values.
func (s *tmemeServer) serveAPIMacroGet(w http.ResponseWriter, r *http.Request) {
	m, ok, err := getSingleFromIDInPath(r.URL.Path, "api/macro", s.db.Macro)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if ok {
		if err := json.NewEncoder(w).Encode(m); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	var all []*tmemes.Macro
	// If a creator parameter is set, filter to macros matching that user ID.
	// As a special case, "anon" or "anonymous" selects unattributed macros.
	uid, err := creatorUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if uid == 0 {
		all = s.db.Macros()
	} else {
		all = s.db.MacrosByCreator(uid)
	}
	total := len(all)

	// Check for sorting order.
	if err := sortMacros(r.FormValue("sort"), all); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Handle pagination.
	page, count, err := parsePageOptions(r, 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pageItems := slicePage(all, page, count)

	rsp := struct {
		M []*tmemes.Macro `json:"macros"`
		N int             `json:"total"`
	}{M: pageItems, N: total}
	if err := json.NewEncoder(w).Encode(rsp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveAPIMacroDelete implements deletion of image macros. Only the user who
// created a macro or an admin can delete a macro. Note that because
// unattributed macros do not store a user ID, this means only admins can
// remove anonymous macros.
//
// API: DELETE /api/macro/:id
//
// On success, the deleted macro object is written back to the caller.
func (s *tmemeServer) serveAPIMacroDelete(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "delete macros")
	if whois == nil {
		return // error already sent
	}

	m, ok, err := getSingleFromIDInPath(r.URL.Path, "api/macro", s.db.Macro)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if !ok {
		http.Error(w, "missing macro ID", http.StatusBadRequest)
		return
	}

	// The creator of a macro can delete it, otherwise the caller must be a
	// superuser.
	if whois.UserProfile.ID != m.Creator && !s.superUser[whois.UserProfile.LoginName] {
		http.Error(w, "permission denied", http.StatusUnauthorized)
		return
	}
	if err := s.db.DeleteMacro(m.ID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveAPIVotePut implements voting on macros. Unlike images, votes cannot be
// unattributed; each user may vote at most once for a macro.
//
// API: PUT /api/vote/:id/up   -- upvote a macro by ID
// API: PUT /api/vote/:id/down -- downvote a macro by ID
func (s *tmemeServer) serveAPIVotePut(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "vote")
	if whois == nil {
		return // error already sent
	}

	// Accept /api/vote/:id/{up,down}
	path, op := r.URL.Path, 0
	if v, ok := strings.CutSuffix(path, "/up"); ok {
		path, op = v, 1
	} else if v, ok := strings.CutSuffix(path, "/down"); ok {
		path, op = v, -1
	} else {
		http.Error(w, "missing vote type", http.StatusBadRequest)
		return
	}

	m, ok, err := getSingleFromIDInPath(path, "api/vote", s.db.Macro)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if !ok {
		http.Error(w, "missing macro ID", http.StatusBadRequest)
		return
	}
	m, err = s.db.SetVote(whois.UserProfile.ID, m.ID, op)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *tmemeServer) serveAPITemplate(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("api-template", 1)
	switch r.Method {
	case "GET":
		s.serveAPITemplateGet(w, r)
	case "POST":
		s.serveAPITemplatePost(w, r)
	case "DELETE":
		s.serveAPITemplateDelete(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveAPITemplateGet returns metadata about template images.
//
// API: /api/template/:id   -- one template by ID
// API: /api/template       -- all templates defined
//
// This API supports pagination (see parsePageOptions).
// The result objects are JSON tmemes.Template values.
func (s *tmemeServer) serveAPITemplateGet(w http.ResponseWriter, r *http.Request) {
	t, ok, err := getSingleFromIDInPath(r.URL.Path, "api/template", s.db.Template)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if ok {
		if err := json.NewEncoder(w).Encode(t); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	var all []*tmemes.Template
	// If a creator parameter is set, filter to templates matching that user ID.
	// As a special case, "anon" or "anonymous" selects unattributed templates.
	uid, err := creatorUserID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if uid == 0 {
		all = s.db.Templates()
	} else {
		all = s.db.TemplatesByCreator(uid)
	}
	total := len(all)

	// Handle pagination.
	page, count, err := parsePageOptions(r, 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	pageItems := slicePage(all, page, count)

	rsp := struct {
		T []*tmemes.Template `json:"templates"`
		N int                `json:"total"`
	}{T: pageItems, N: total}
	if err := json.NewEncoder(w).Encode(rsp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveAPITemplatePost implements creating (uploading) new template images.
//
// API: POST /api/template
//
// The payload must be of type multipart/form-data, and supports the fields:
//
//   - image: the image file to upload (required)
//   - name: a text description of the template (required)
//   - anon: if present and true, create an unattributed template
func (s *tmemeServer) serveAPITemplatePost(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "create templates")
	if whois == nil {
		return // error already sent
	}

	// Create a new image.
	t := &tmemes.Template{
		Name:    r.FormValue("name"),
		Creator: whois.UserProfile.ID,
	}
	if anon := r.FormValue("anon"); anon != "" {
		anonBool, err := strconv.ParseBool(anon)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if anonBool {
			if !s.allowAnonymous {
				http.Error(w, "anonymous templates not allowed", http.StatusUnauthorized)
				return
			}
			t.Creator = -1
		}
	}

	img, header, err := r.FormFile("image")
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if header.Size > *maxImageSize<<20 {
		http.Error(w, "image too large", http.StatusBadRequest)
		return
	}
	ext := filepath.Ext(header.Filename)
	if ext != ".png" && ext != ".jpg" && ext != ".jpeg" && ext != ".gif" {
		http.Error(w, "invalid image format", http.StatusBadRequest)
		return
	}
	imageConfig, _, err := image.DecodeConfig(img)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t.Width = imageConfig.Width
	t.Height = imageConfig.Height
	img.Seek(0, io.SeekStart)

	etagHash := sha256.New()
	if err := s.db.AddTemplate(t, ext, newHashPipe(img, etagHash)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.imageFileEtags.Store(t.Path, formatEtag(etagHash))
	redirect := fmt.Sprintf("/create/%v", t.ID)
	http.Redirect(w, r, redirect, http.StatusFound)
}

// serveAPITemplateDelete implements deletion of templates. Only the user who
// created a template or an admin can delete a template. Note that because
// unattributed templates do not store a user ID, this means only admins can
// remove anonymous templates.
//
// API: DELETE /api/template/:id
//
// On success, the deleted template object is written back to the caller.
func (s *tmemeServer) serveAPITemplateDelete(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "delete templates")
	if whois == nil {
		return // error already sent
	}

	t, ok, err := getSingleFromIDInPath(r.URL.Path, "api/template", s.db.Template)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if !ok {
		http.Error(w, "missing template ID", http.StatusBadRequest)
		return
	}

	// The creator of a template can delete it, otherwise the caller must be a
	// superuser.
	if whois.UserProfile.ID != t.Creator && !s.superUser[whois.UserProfile.LoginName] {
		http.Error(w, "permission denied", http.StatusUnauthorized)
		return
	}
	if err := s.db.SetTemplateHidden(t.ID, true); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(t); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *tmemeServer) serveAPIVote(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("api-vote", 1)
	switch r.Method {
	case "GET":
		s.serveAPIVoteGet(w, r)
	case "DELETE":
		s.serveAPIVoteDelete(w, r)
	case "PUT":
		s.serveAPIVotePut(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// serveAPIVoteGet reports vote data for the calling user.
//
// API: /api/vote     -- report all votes for the caller
// API: /api/vote/:id -- report the user's vote on a macro ID
//
// Vote values are -1 (downvote), 0 (unvoted), and 1 (upvote).
func (s *tmemeServer) serveAPIVoteGet(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "get votes")
	if whois == nil {
		return // error already sent
	}

	m, ok, err := getSingleFromIDInPath(r.URL.Path, "api/vote", s.db.Macro)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	type macroVote struct {
		M int `json:"macroID"`
		V int `json:"vote"`
	}

	w.Header().Set("Content-Type", "application/json")
	if ok {
		// Report the user's vote on a single macro.
		vote, err := s.db.UserMacroVote(whois.UserProfile.ID, m.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := json.NewEncoder(w).Encode(macroVote{
			M: m.ID,
			V: vote,
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}

	// Report all the user's non-zero votes.
	uv, err := s.db.UserVotes(whois.UserProfile.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	votes := make([]macroVote, 0, len(uv))
	for mid, vote := range uv {
		votes = append(votes, macroVote{mid, vote})
	}
	slices.SortFunc(votes, func(a, b macroVote) bool {
		return a.M < b.M
	})

	all := struct {
		U tailcfg.UserID `json:"userID"`
		V []macroVote    `json:"votes"`
	}{
		U: whois.UserProfile.ID,
		V: votes,
	}
	if err := json.NewEncoder(w).Encode(all); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// serveAPIVoteDelete implements removal of a user's vote from a macro.
//
// API: DELETE /api/vote/:id
//
// This succeeds even if the user had not voted on the specified macro,
// provided the user is valid and the macro exists.
func (s *tmemeServer) serveAPIVoteDelete(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "delete votes")
	if whois == nil {
		return // error already sent
	}

	m, ok, err := getSingleFromIDInPath(r.URL.Path, "api/vote", s.db.Macro)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if !ok {
		http.Error(w, "missing macro ID", http.StatusBadRequest)
		return
	}

	if _, err := s.db.SetVote(whois.UserProfile.ID, m.ID, 0); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	} else if err := json.NewEncoder(w).Encode(m); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
