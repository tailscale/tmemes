// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Program tmemes is an image macro server that runs as a node on a tailnet.
// It exposes a UI and API service to create and share base images overlaid
// with user-defined text.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/tailscale/tmemes/bot"
	"github.com/tailscale/tmemes/store"
	"tailscale.com/tsnet"
	"tailscale.com/types/logger"

	_ "modernc.org/sqlite"
)

// Flag definitions
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

	// Experimental features.

	enableSlackBot = flag.Bool("enable-slack-bot", false,
		"Enable Slack integration (experimental)")

	// TODO(creachadair): Finish and document the Slack integration.
)

func init() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `Usage: [TS_AUTHKEY=k] %[1]s <options>

Run an image macro service as a node on a tailnet.  The service listens for
HTTP requests (not HTTPS) on port 80.

The first time you start %[1]s, you must authenticate its node on the tailnet
you wnat it to join. To do this, generate an auth key [1] and pass it in via
the TS_AUTHKEY environment variable:

  TS_AUTHKEY=tskey-auth-k______CNTRL-aBC0d1efG2h34iJkLM5nO6pqr7stUV8w9 %[1]s

We recommend you use a tagged auth key so that the node will not expire. Once
the node is authorized, you can just run the program itself.  The server runs
until terminated by SIGINT or SIGTERM.

[1]: https://tailscale.com/kb/1085/auth-keys/

Options:
`, filepath.Base(os.Args[0]))
		flag.PrintDefaults()
	}
}

func main() {
	flag.Parse()
	if *storeDir == "" {
		log.Fatal("You must provide a non-empty --store directory")
	} else if *maxImageSize <= 0 {
		log.Fatal("The -max-image-size must be positive")
	}

	db, err := store.New(*storeDir, &store.Options{
		MaxAccessAge:  *maxAccessAge,
		MinPruneBytes: *minPruneMiB << 20,
	})
	if err != nil {
		log.Fatalf("Opening store: %v", err)
	} else if *cacheSeed != "" {
		err := db.SetCacheSeed(*cacheSeed)
		if err != nil {
			log.Fatalf("Setting cache seed: %v", err)
		}
	}
	defer db.Close()

	logf := logger.Discard
	if *doVerbose {
		logf = log.Printf
	}
	s := &tsnet.Server{
		Hostname: *hostName,
		Dir:      filepath.Join(*storeDir, "tsnet"),
		Logf:     logf,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	go func() {
		<-ctx.Done()
		log.Print("Signal received, stopping server...")
		s.Close()
	}()

	ln, err := s.Listen("tcp", ":80")
	if err != nil {
		panic(err)
	}
	defer ln.Close()

	lc, err := s.LocalClient()
	if err != nil {
		panic(err)
	}

	ms := &tmemeServer{
		db:             db,
		srv:            s,
		lc:             lc,
		allowAnonymous: *allowAnonymous,
	}
	if err := ms.initialize(s); err != nil {
		panic(err)
	}

	log.Print("it's alive!")
	http.Serve(ln, ms.newMux())
}

func startSlackBot() {
	b, err := bot.NewSlackBot(&bot.Config{
		Debug: true,
		// Logf:  logger.Discard,
	})
	if err != nil {
		log.Fatalf("Creating Slack bot: %v", err)
	}
	if err := b.Run(); err != nil {
		log.Fatalf("Running Slack bot: %v", err)
	}
}
