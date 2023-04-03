// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"fmt"
	"image"
	"image/draw"
	"image/gif"
	"image/jpeg"
	"image/png"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fogleman/gg"
	"github.com/tailscale/tmemes"
	"github.com/tailscale/tmemes/store"
	"golang.org/x/exp/slices"
	"golang.org/x/image/font"
	"tailscale.com/client/tailscale"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/metrics"
	"tailscale.com/tailcfg"
	"tailscale.com/tsnet"
	"tailscale.com/util/singleflight"
)

type tmemeServer struct {
	db        *store.DB
	srv       *tsnet.Server
	lc        *tailscale.LocalClient
	superUser map[string]bool // logins of admin users
	staticDir string          // static assets not reachable from embed

	macroGenerationSingleFlight singleflight.Group[string, string]

	mu sync.Mutex // guards userProfiles

	userProfiles            map[tailcfg.UserID]tailcfg.UserProfile
	lastUpdatedUserProfiles time.Time
}

var (
	serveMetrics = &metrics.LabelMap{Label: "type"}
	macroMetrics = &metrics.LabelMap{Label: "type"}
)

func init() {
	expvar.Publish("tmemes_serve_metrics", serveMetrics)
	expvar.Publish("tmemes_macro_metrics", macroMetrics)
}

var errNotFound = errors.New("not found")

// userFromID returns the user profile for the given user ID.
// If the user profile is not found, it will attempt to fetch
// the latest user profiles from the tsnet server.
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
	uiMux.HandleFunc("/templates/", s.serveUITemplates) // view one template by ID
	uiMux.HandleFunc("/templates", s.serveUITemplates)  // view all templates
	uiMux.HandleFunc("/create/", s.serveUICreate)       // view create page for given template ID
	uiMux.HandleFunc("/macro/", s.serveUIMacros)        // view one macro by ID
	uiMux.HandleFunc("/macro", s.serveUIMacros)         // view all macros
	uiMux.HandleFunc("/", s.serveUIMacros)              // alias for /macros/
	uiMux.HandleFunc("/upload", s.serveUIUpload)        // template upload view

	uiMux.HandleFunc("/style.css", s.serveCSS)
	uiMux.HandleFunc("/script.js", s.serveJS)

	mux := http.NewServeMux()
	mux.Handle("/api/", apiMux)
	mux.Handle("/content/", contentMux)
	mux.Handle("/static/", http.StripPrefix("/static", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fp := filepath.Join(s.staticDir, r.URL.Path)
		if fs, err := os.Stat(fp); err != nil || fs.IsDir() {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		serveMetrics.Add("static", 1)
		http.ServeFile(w, r, fp)
	})))
	mux.Handle("/", uiMux)

	return mux
}

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

	http.ServeFile(w, r, t.Path)
}

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
		http.ServeFile(w, r, cachePath)
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
	http.ServeFile(w, r, cachePath)
}

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
	font, err := s.fontForImage(srcGif.Image[0])
	if err != nil {
		return err
	}

	if len(srcGif.Image) == 0 {
		return errors.New("no frames in GIF")
	}
	bounds := srcGif.Image[0].Bounds()
	dc := gg.NewContext(bounds.Dx(), bounds.Dy())
	dc.SetFontFace(font)
	for _, t := range m.TextOverlay {
		overlayTextOnImage(dc, t, bounds)
	}
	img := dc.Image()
	// Add text to each frame of the GIF
	for i, frame := range srcGif.Image {
		// Create a new image context
		pt := image.NewPaletted(frame.Bounds(), frame.Palette)
		draw.Draw(pt, frame.Bounds(), frame, frame.Bounds().Min, draw.Src)

		draw.Draw(pt, bounds, img, bounds.Min, draw.Over)
		// Update the frame
		srcGif.Image[i] = pt
	}

	// Save the modified GIF
	dstFile, err := os.Create(cachePath)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			dstFile.Close()
			os.Remove(cachePath)
		}
	}()

	err = gif.EncodeAll(dstFile, srcGif)
	if err != nil {
		return err
	}
	return dstFile.Close()
}

func (s *tmemeServer) fontForImage(img image.Image) (font.Face, error) {
	const typeHeightFraction = 0.15
	fontSize := (float64(img.Bounds().Dy()) * 0.75) * typeHeightFraction
	return gg.LoadFontFace(filepath.Join(s.staticDir, "font", "Oswald-SemiBold.ttf"), fontSize)
}

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

	font, err := s.fontForImage(srcImage)
	if err != nil {
		return err
	}
	dc := gg.NewContext(srcImage.Bounds().Dx(), srcImage.Bounds().Dy())
	dc.SetFontFace(font)
	bounds := srcImage.Bounds()
	for _, tl := range m.TextOverlay {
		overlayTextOnImage(dc, tl, bounds)
	}

	alpha := image.NewNRGBA(bounds)
	draw.Draw(alpha, bounds, srcImage, bounds.Min, draw.Src)
	draw.Draw(alpha, bounds, dc.Image(), bounds.Min, draw.Over)
	dst, err := os.Create(cachePath)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			dst.Close()
			os.Remove(cachePath)
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

	return dst.Close()
}

func oneForZero(v float64) float64 {
	if v == 0 {
		return 1
	}
	return v
}

func overlayTextOnImage(dc *gg.Context, tl tmemes.TextLine, bounds image.Rectangle) {
	text := strings.TrimSpace(tl.Text)
	if text == "" {
		return
	}
	c := tl.Color
	dc.SetRGB(c.R(), c.G(), c.B())

	x := tl.Field.X * float64(bounds.Dx())
	y := tl.Field.Y * float64(bounds.Dy())
	width := oneForZero(tl.Field.Width) * float64(bounds.Dx())
	dc.DrawStringWrapped(text, x, y, 0.5, 0.5, width, 1.5, gg.AlignCenter)
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
	case "PUT":
		s.serveAPIMacroPut(w, r)
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
	}
	for _, tl := range m.TextOverlay {
		if tl.Field.X < 0 || tl.Field.X > 1 {
			http.Error(w, "invalid x", http.StatusBadRequest)
			return
		}
		if tl.Field.Y < 0 || tl.Field.Y > 1 {
			http.Error(w, "invalid y", http.StatusBadRequest)
			return
		}
		if tl.Field.Width < 0 || tl.Field.Width > 1 {
			http.Error(w, "invalid width", http.StatusBadRequest)
			return
		}
	}

	// If the creator is negative, treat the macro as anonymous.  Otherwise the
	// creator must be unset (zero).
	if m.Creator > 0 {
		http.Error(w, "invalid creator", http.StatusBadRequest)
		return
	} else if m.Creator == 0 {
		m.Creator = whois.UserProfile.ID
	} else {
		m.Creator = -1 // normalize anonymous to -1
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

func parseIntOrDefault(s, label string, dflt int) (int, error) {
	if s == "" {
		return dflt, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, err
	} else if v < 0 {
		return v, errors.New("invalid " + label)
	}
	return v, nil
}

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

	// Check for pagination parameters. If none are present, ship everything.
	// Otherwise, start at the given "page" and return up to "count" results,
	// where pages are multiples of count results.
	page, err := parseIntOrDefault(r.FormValue("page"), "page", -1)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	count, err := parseIntOrDefault(r.FormValue("count"), "count", 20)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if page > 0 {
		start := (page - 1) * count
		end := start + count
		if start >= len(all) {
			all = nil
		} else {
			if end > len(all) {
				end = len(all)
			}
			all = all[start:end]
		}
	}

	rsp := struct {
		M []*tmemes.Macro `json:"macros"`
		N int             `json:"total"`
	}{M: all, N: total}
	if err := json.NewEncoder(w).Encode(rsp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

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

func (s *tmemeServer) serveAPIMacroPut(w http.ResponseWriter, r *http.Request) {
	whois := s.checkAccess(w, r, "vote")
	if whois == nil {
		return // error already sent
	}

	// Accept /api/macro/:id/{up,down}vote
	path, op := r.URL.Path, 0
	if v, ok := strings.CutSuffix(path, "/upvote"); ok {
		path, op = v, 1
	} else if v, ok := strings.CutSuffix(path, "/downvote"); ok {
		path, op = v, -1
	} else {
		http.Error(w, "missing vote type", http.StatusBadRequest)
		return
	}

	m, ok, err := getSingleFromIDInPath(path, "api/macro", s.db.Macro)
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

	rsp := struct {
		T []*tmemes.Template `json:"templates"`
		N int                `json:"total"`
	}{T: all, N: total}
	if err := json.NewEncoder(w).Encode(rsp); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

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
	if err := s.db.AddTemplate(t, ext, img); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	redirect := fmt.Sprintf("/create/%v", t.ID)
	http.Redirect(w, r, redirect, 302)
}

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
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

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
