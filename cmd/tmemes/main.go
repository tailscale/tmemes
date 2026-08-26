// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Program tmemes is an image macro server that runs as a node on a tailnet.
// It exposes a UI and API service to create and share base images overlaid
// with user-defined text.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/tmemes/server"
	"github.com/tailscale/tmemes/store"
	"tailscale.com/tsnet"
	"tailscale.com/tsweb"
	"tailscale.com/types/logger"

	_ "modernc.org/sqlite"
)

var (
	doVerbose = flag.Bool("v", false, "Enable verbose debug logging")

	// Users with administrative ("super-user") powers. By default, only the
	// user who created an image can edit or delete it. Marking a user as an
	// admin gives them permission to edit or delete any image.
	adminUsers = flag.String("admin", "",
		"Users with admin rights (comma-separated logins: user@example.com)")

	// If this flag is set true, users are allowed to post unattributed
	// ("anonymous") templates and macros. Unattributed images still require
	// that the user be authorized by the tailnet, but the server will not
	// record their user ID in its database.
	allowAnonymous = flag.Bool("allow-anonymous", true, "allow anonymous uploads")

	// The hostname to advertise on the tailnet.
	hostName = flag.String("hostname", "tmemes",
		"The tailscale hostname to use for the server")

	// This flag controls the maximum image file size the server will allow to
	// be uploaded as a template.
	maxImageSize = flag.Int64("max-image-size", 4,
		"Maximum image size in MiB")

	// The data directory where the server will store its images, caches, and
	// the database of macro definitions.
	storeDir = flag.String("store", "/tmp/tmemes", "Storage directory (required)")

	// Image macros are generated on the fly and cached. The server periodically
	// cleans up cached macros that have not been accessed for some period of
	// time, once the cache exceeds a size threshold.
	maxAccessAge = flag.Duration("cache-max-access-age", 24*time.Hour,
		"How long after last access a cached macro is eligible for cleanup")
	minPruneMiB = flag.Int64("cache-min-prune-mib", 512,
		"Minimum size of macro cache in MiB to trigger a cleanup")
	cacheSeed = flag.String("cache-seed", "",
		"Hash seed used to generate cache keys")
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: [TS_AUTHKEY=k] %[1]s <options>

Run an image macro service as a node on a tailnet. The service listens for
HTTP requests (not HTTPS) on port 80.

The first time you start %[1]s, you must authenticate its node on the tailnet
you want it to join. To do this, generate an auth key [1] and pass it in via
the TS_AUTHKEY environment variable:

  TS_AUTHKEY=tskey-auth-k______CNTRL-aBC0d1efG2h34iJkLM5nO6pqr7stUV8w9 %[1]s

We recommend you use a tagged auth key so that the node will not expire. Once
the node is authorized, you can just run the program itself. The server runs
until terminated by SIGINT or SIGTERM.

[1]: https://tailscale.com/kb/1085/auth-keys/

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	if *storeDir == "" {
		return errors.New("must provide a non-empty --store directory")
	}
	if *maxImageSize <= 0 {
		return errors.New("--max-image-size must be positive")
	}

	db, err := store.New(*storeDir, &store.Options{
		MaxAccessAge:  *maxAccessAge,
		MinPruneBytes: *minPruneMiB << 20,
	})
	if err != nil {
		return fmt.Errorf("opening store: %w", err)
	}
	defer db.Close()
	if *cacheSeed != "" {
		if err := db.SetCacheSeed(*cacheSeed); err != nil {
			return fmt.Errorf("setting cache seed: %w", err)
		}
	}

	logf := logger.Discard
	if *doVerbose {
		logf = log.Printf
	}
	ts := &tsnet.Server{
		Hostname: *hostName,
		Dir:      filepath.Join(*storeDir, "tsnet"),
		Logf:     logf,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	defer ts.Close()
	if _, err := ts.Up(ctx); err != nil {
		return fmt.Errorf("starting tsnet: %w", err)
	}

	lc, err := ts.LocalClient()
	if err != nil {
		return fmt.Errorf("creating tsnet local client: %w", err)
	}
	app, err := server.New(server.Options{
		DB:             db,
		LocalClient:    lc,
		AdminUsers:     splitUsers(*adminUsers),
		AllowAnonymous: *allowAnonymous,
		MaxImageBytes:  *maxImageSize << 20,
	})
	if err != nil {
		return fmt.Errorf("initializing tmemes: %w", err)
	}
	if err := serveDebug(ts); err != nil {
		return err
	}

	ln, err := ts.Listen("tcp", ":80")
	if err != nil {
		return fmt.Errorf("listening on :80: %w", err)
	}
	server := &http.Server{Handler: app.Handler()}
	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Serve(ln) }()

	log.Print("it's alive!")
	select {
	case err := <-serverErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("serving tmemes: %w", err)
		}
		return nil
	case <-ctx.Done():
		log.Print("signal received, stopping server...")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutting down server: %w", err)
		}
		return nil
	}
}

func splitUsers(users string) []string {
	if users == "" {
		return nil
	}
	return strings.Split(users, ",")
}

func serveDebug(ts *tsnet.Server) error {
	ln, err := ts.Listen("tcp", ":8383")
	if err != nil {
		return fmt.Errorf("listening for debug requests on :8383: %w", err)
	}
	go func() {
		mux := http.NewServeMux()
		tsweb.Debugger(mux)
		if err := http.Serve(ln, mux); err != nil && !errors.Is(err, net.ErrClosed) {
			log.Printf("debug server: %v", err)
		}
	}()
	return nil
}
