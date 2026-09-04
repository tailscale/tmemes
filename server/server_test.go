// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package server

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/ipn/ipnstate"
)

type fakeLocalClient struct{}

func (fakeLocalClient) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return nil, nil
}
func (fakeLocalClient) Status(context.Context) (*ipnstate.Status, error) { return nil, nil }

func TestNewRejectsInvalidOptions(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{name: "nil_database"},
		{name: "nil_local_client", opts: Options{MaxImageBytes: 1}},
		{name: "zero_max_image_bytes", opts: Options{LocalClient: fakeLocalClient{}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.opts); err == nil {
				t.Fatal("New succeeded")
			}
		})
	}
}

func TestHandlerRoutes(t *testing.T) {
	s := new(tmemeServer)
	h := s.Handler()
	for _, path := range []string{"/api/macro", "/content/template/1", "/static/style.css", "/macros/"} {
		if _, pattern := h.Handler(&http.Request{URL: mustParseURL(t, path)}); pattern == "" {
			t.Errorf("no route for %q", path)
		}
	}
}

func TestUploadTemplateCSRFField(t *testing.T) {
	var got bytes.Buffer
	if err := ui.ExecuteTemplate(&got, "upload.tmpl", &uiData{
		CSRFFieldName: "custom-csrf-field",
		CSRFToken:     "token",
	}); err != nil {
		t.Fatal(err)
	}
	if want := `name="custom-csrf-field" value="token"`; !strings.Contains(got.String(), want) {
		t.Errorf("upload template does not contain %q:\n%s", want, got.String())
	}
}

func TestSafewebMuxRoutes(t *testing.T) {
	s := new(tmemeServer)
	browser, api := s.BrowserMux(), s.APIMux()
	for _, test := range []struct {
		path string
		mux  *http.ServeMux
	}{
		{path: "/api/template", mux: browser},
		{path: "/api/macro", mux: api},
		{path: "/api/context/1", mux: api},
		{path: "/api/vote/1/up", mux: api},
		{path: "/api/unknown", mux: api},
		{path: "/content/template/1", mux: browser},
		{path: "/create/1", mux: browser},
	} {
		if _, pattern := test.mux.Handler(&http.Request{URL: mustParseURL(t, test.path)}); pattern == "" {
			t.Errorf("no route for %q", test.path)
		}
	}
}

func TestUnknownAPIRouteNotFound(t *testing.T) {
	w := httptest.NewRecorder()
	new(tmemeServer).Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
	if got, want := w.Code, http.StatusNotFound; got != want {
		t.Errorf("status code: got %d, want %d", got, want)
	}
}

func mustParseURL(t *testing.T, path string) *url.URL {
	t.Helper()
	u, err := url.Parse(path)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
