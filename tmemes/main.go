// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tailscale/tmemes/bot"
	"github.com/tailscale/tmemes/store"
	"tailscale.com/tsnet"
	"tailscale.com/tsweb"
	"tailscale.com/types/logger"

	_ "modernc.org/sqlite"
)

var (
	adminUsers     = flag.String("admin", "", "Users with admin rights (comma-separated logins)")
	cacheSeed      = flag.String("cache-seed", "", "Hash seed used to generate cache keys")
	hostName       = flag.String("hostname", "tmemes", "The TS hostname to use for the server")
	storeDir       = flag.String("store", "/tmp/tmemes", "Storage directory (required)")
	maxImageSize   = flag.Int64("max-image-size", 4, "Maximum image size in MiB")
	enableSlackBot = flag.Bool("enable-slack-bot", false, "Enable Slack bot")

	maxAccessAge = flag.Duration("cache-max-access-age", 24*time.Hour,
		"How long after last access a cached macro is eligible for cleanup")
	minPruneMiB = flag.Int64("cache-min-prune-mib", 5120,
		"Minimum size of macro cache in MiB to trigger a cleanup")

	allowAnonymous = flag.Bool("allow-anonymous", true, "allow anonymous uploads")
)

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

	s := &tsnet.Server{
		Hostname: *hostName,
		Logf:     logger.Discard,
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
	if *adminUsers != "" {
		ms.superUser = make(map[string]bool)
		for _, u := range strings.Split(*adminUsers, ",") {
			ms.superUser[u] = true
		}
	}

	go func() {
		ln, err := s.Listen("tcp", ":8383")
		if err != nil {
			panic(err)
		}
		defer ln.Close()
		log.Print("Starting debug server on :8383")
		mux := http.NewServeMux()
		tsweb.Debugger(mux)
		http.Serve(ln, mux)
	}()

	if *enableSlackBot {
		go startSlackBot()
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
