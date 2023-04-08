// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/tailscale/tmemes"
	"golang.org/x/exp/slices"
	"tailscale.com/tailcfg"
	"tailscale.com/words"
)

//go:embed ui/*
var uiFS embed.FS

//go:embed static
var staticFS embed.FS

var ui = template.Must(template.New("ui").Funcs(template.FuncMap{
	"timestamp": func(ts time.Time) string {
		return ts.Local().Format(time.Stamp)
	},
}).ParseFS(uiFS, "ui/*.tmpl"))

// uiData is the value passed to HTML templates.
type uiData struct {
	Macros        []*uiMacro
	Templates     []*uiTemplate
	CallerID      tailcfg.UserID
	AllowAnon     bool
	CallerIsAdmin bool
}

type uiMacro struct {
	*tmemes.Macro
	Template    *uiTemplate
	ImageURL    string
	CreatorName string
	CreatorID   tailcfg.UserID
	Upvoted     bool
	Downvoted   bool
}

type uiTemplate struct {
	*tmemes.Template
	ImageURL    string
	Extension   string
	CreatorName string
	CreatorID   tailcfg.UserID
	AllowAnon   bool
}

func (s *tmemeServer) newUITemplate(ctx context.Context, t *tmemes.Template) *uiTemplate {
	ext := filepath.Ext(t.Path)
	return &uiTemplate{
		Template:    t,
		ImageURL:    fmt.Sprintf("/content/template/%d%s", t.ID, ext),
		Extension:   ext,
		CreatorName: s.userDisplayName(ctx, t.Creator, t.CreatedAt),
		CreatorID:   t.Creator,
		AllowAnon:   s.allowAnonymous,
	}
}

func (s *tmemeServer) newUIData(ctx context.Context, templates []*tmemes.Template, macros []*tmemes.Macro, caller tailcfg.UserID) *uiData {
	data := &uiData{
		AllowAnon:     s.allowAnonymous,
		CallerID:      caller,
		CallerIsAdmin: s.userIsAdmin(ctx, caller),
	}

	tid := make(map[int]*uiTemplate)
	for _, t := range templates {
		ut := s.newUITemplate(ctx, t)
		data.Templates = append(data.Templates, ut)
		tid[t.ID] = ut
	}
	uv, err := s.db.UserVotes(caller)
	if err != nil {
		log.Printf("error getting user votes: %v", err)
	}

	for _, m := range macros {
		mt := tid[m.TemplateID]
		if mt == nil {
			t, err := s.db.AnyTemplate(m.TemplateID)
			if err != nil {
				panic(err) // this should not be possible
			}
			mt = s.newUITemplate(ctx, t)
		}
		vote := uv[m.ID]
		um := &uiMacro{
			Macro:       m,
			Template:    mt,
			ImageURL:    fmt.Sprintf("/content/macro/%d%s", m.ID, mt.Extension),
			CreatorName: s.userDisplayName(ctx, m.Creator, m.CreatedAt),
			CreatorID:   m.Creator,
		}
		if vote > 0 {
			um.Upvoted = true
		} else if vote < 0 {
			um.Downvoted = true
		}
		data.Macros = append(data.Macros, um)
	}

	return data
}

var pick = [2][]string{words.Tails(), words.Scales()}

func tailyScalyName(ts time.Time) string {
	var names []string
	v := int(ts.UnixMicro())
	for i := 0; i < 3; i++ {
		j := int(v & 1)
		v >>= 1
		n := len(pick[j])
		k := v % n
		v /= n
		w := pick[j][k]
		names = append(names, strings.ToUpper(w[:1])+w[1:])
	}
	return strings.Join(names, " ")
}

func (s *tmemeServer) userDisplayName(ctx context.Context, id tailcfg.UserID, ts time.Time) string {
	p, err := s.userFromID(ctx, id)
	if err != nil {
		return tailyScalyName(ts)
	} else if p.DisplayName != "" {
		return p.DisplayName
	}
	return p.LoginName
}

func (s *tmemeServer) userIsAdmin(ctx context.Context, id tailcfg.UserID) bool {
	p, err := s.userFromID(ctx, id)
	if err != nil {
		return false // fail closed
	}
	return s.superUser[p.LoginName]
}

func getSingleFromIDInPath[T any](path, key string, f func(int) (T, error)) (T, bool, error) {
	var zero T
	idStr, ok := strings.CutPrefix(path, "/"+key+"/")
	if !ok || idStr == "" {
		return zero, false, nil
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		return zero, false, fmt.Errorf("invalid %s ID: %w", key, err)
	}
	v, err := f(id)
	if err != nil {
		return v, false, err
	}
	return v, true, nil
}

func (s *tmemeServer) serveUICreate(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("ui-create", 1)
	id := strings.TrimPrefix(r.URL.Path, "/create/")
	if id == "" {
		http.Error(w, "missing id", http.StatusBadRequest)
		return
	}
	idInt, err := strconv.Atoi(id)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	t, err := s.db.Template(idInt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	switch r.Method {
	case "GET":
		s.serveUICreateGet(w, r, t)
	case "POST":
		s.serveUICreatePost(w, r, t)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *tmemeServer) serveUICreateGet(w http.ResponseWriter, r *http.Request, t *tmemes.Template) {
	template := s.newUITemplate(r.Context(), t)

	w.Header().Set("Content-Type", "text/html")
	var buf bytes.Buffer
	if err := ui.ExecuteTemplate(&buf, "create.tmpl", template); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

type webTemplateData struct {
	Overlays []tmemes.TextLine `json:"overlays"`
	Anon     bool              `json:"anon"`
}

func (s *tmemeServer) serveUICreatePost(w http.ResponseWriter, r *http.Request, t *tmemes.Template) {
	// TODO: need to refactor out whois protection
	whois, err := s.lc.WhoIs(r.Context(), r.RemoteAddr)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if whois == nil {
		http.Error(w, "not logged in", http.StatusUnauthorized)
		return
	}
	if whois.Node.IsTagged() {
		http.Error(w, "tagged nodes cannot create macros", http.StatusForbidden)
		return
	}

	// actual processing starts here
	var webData webTemplateData
	if err := json.NewDecoder(r.Body).Decode(&webData); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if len(webData.Overlays) == 0 {
		http.Error(w, "must specify at least one overlay", http.StatusBadRequest)
		return
	}
	for _, o := range webData.Overlays {
		if o.Text == "" {
			http.Error(w, "overlay text cannot be empty", http.StatusBadRequest)
			return
		}
	}

	m := tmemes.Macro{
		TemplateID:  t.ID,
		TextOverlay: webData.Overlays,
	}

	if webData.Anon {
		if !s.allowAnonymous {
			http.Error(w, "anonymous macros not allowed", http.StatusForbidden)
			return
		}
		m.Creator = -1
	} else {
		m.Creator = whois.UserProfile.ID
	}

	if err := s.db.AddMacro(&m); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	created := struct {
		CreatedID int `json:"createdId"`
	}{
		CreatedID: m.ID,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(created); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (s *tmemeServer) serveUITemplates(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("ui-templates", 1)
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var templates []*tmemes.Template
	if t, ok, err := getSingleFromIDInPath(r.URL.Path, "t", s.db.Template); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if !ok {
		creator, err := creatorUserID(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if creator != 0 {
			templates = s.db.TemplatesByCreator(creator)
		} else {
			templates = s.db.Templates()
		}
	} else {
		templates = append(templates, t)
	}
	slices.SortFunc(templates, func(a, b *tmemes.Template) bool {
		return a.CreatedAt.After(b.CreatedAt)
	})

	caller := s.getCallerID(r)
	w.Header().Set("Content-Type", "text/html")
	var buf bytes.Buffer
	data := s.newUIData(r.Context(), templates, nil, caller)
	if err := ui.ExecuteTemplate(&buf, "templates.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

func (s *tmemeServer) getCallerID(r *http.Request) tailcfg.UserID {
	caller := tailcfg.UserID(-1)
	whois, err := s.lc.WhoIs(r.Context(), r.RemoteAddr)
	if err == nil {
		caller = whois.UserProfile.ID
	}
	return caller
}

func (s *tmemeServer) serveUIMacros(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("ui-macros", 1)
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var macros []*tmemes.Macro
	if m, ok, err := getSingleFromIDInPath(r.URL.Path, "m", s.db.Macro); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if !ok {
		creator, err := creatorUserID(r)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if creator != 0 {
			macros = s.db.MacrosByCreator(creator)
		} else {
			macros = s.db.Macros()
		}
	} else {
		macros = append(macros, m)
	}
	defaultSort := "top-popular"
	if v := r.URL.Query().Get("sort"); v != "" {
		defaultSort = v
	}
	if err := sortMacros(defaultSort, macros); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	var buf bytes.Buffer
	data := s.newUIData(r.Context(), s.db.Templates(), macros, s.getCallerID(r))
	if err := ui.ExecuteTemplate(&buf, "macros.tmpl", data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}

func (s *tmemeServer) serveUIUpload(w http.ResponseWriter, r *http.Request) {
	serveMetrics.Add("ui-upload", 1)
	if r.Method != "GET" {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html")
	var buf bytes.Buffer
	uiD := s.newUIData(r.Context(), nil, nil, s.getCallerID(r))
	if err := ui.ExecuteTemplate(&buf, "upload.tmpl", uiD); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	buf.WriteTo(w)
}
