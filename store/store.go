// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

// Package store implements a data store for memes.
//
// # Structure
//
// A DB manages a directory in the filesystem. At the top level of the
// directory is a SQLite database (index.db) that keeps track of metadata about
// templates, macros, and votes. There are also subdirectories to store the
// image data, "templates" and "macros".
//
// The "macros" subdirectory is a cache, and the DB maintains a background
// polling thread that cleans up files that have not been accessed for a while.
// It is safe to manually delete files inside the macros directory; the server
// will re-create them on demand. Templates images are persistent, and should
// not be modified or deleted.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tailscale/tmemes"
	"golang.org/x/exp/maps"
	"tailscale.com/tailcfg"
)

var subdirs = []string{"templates", "macros"}

// A DB is a meme database. It consists of a directory containing files and
// subdirectories holding images and metadata. A DB is safe for concurrent use
// by multiple goroutines.
type DB struct {
	dir           string
	stop          context.CancelFunc
	tasks         sync.WaitGroup
	minPruneBytes int64
	maxAccessAge  time.Duration

	mu             sync.Mutex
	sqldb          *sql.DB
	cacheSeed      []byte
	macros         map[int]*tmemes.Macro
	nextMacroID    int
	templates      map[int]*tmemes.Template
	nextTemplateID int
}

// Options are optional settings for a DB.  A nil *Options is ready for use
// with default values.
type Options struct {
	// Do not prune the macro cache until it is at least this big.
	// Default: 50MB.
	MinPruneBytes int64

	// When pruning the cache, discard entries that have not been accessed in at
	// least this long. Default: 30m.
	MaxAccessAge time.Duration
}

func (o *Options) minPruneBytes() int64 {
	if o == nil || o.MinPruneBytes <= 0 {
		return 50 << 20
	}
	return o.MinPruneBytes
}

func (o *Options) maxAccessAge() time.Duration {
	if o == nil || o.MaxAccessAge <= 0 {
		return 30 * time.Minute
	}
	return o.MaxAccessAge
}

// New creates or opens a data store.  A store is a directory that is created
// if necessary. The DB assumes ownership of the directory contents.  A nil
// *Options provides default settings (see [Options]).
//
// The caller should Close the DB when it is no longer in use, to ensure the
// cache maintenance routine is stopped and cleaned up.
func New(dirPath string, opts *Options) (*DB, error) {
	if err := os.MkdirAll(dirPath, 0700); err != nil {
		return nil, fmt.Errorf("store.New: %w", err)
	}

	// Create the standard subdirectories for image data.
	for _, sub := range subdirs {
		path := filepath.Join(dirPath, sub)
		if err := os.MkdirAll(path, 0700); err != nil {
			return nil, err
		}
	}

	dbPath := filepath.Join(dirPath, "index.db")
	sqldb, err := openDatabase(dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	db := &DB{
		dir:           dirPath,
		minPruneBytes: opts.minPruneBytes(),
		maxAccessAge:  opts.maxAccessAge(),
		stop:          cancel,
		sqldb:         sqldb,
	}
	if err := db.loadSQLiteIndex(); err != nil {
		db.Close()
		return nil, err
	}
	db.tasks.Add(1)
	go func() {
		defer db.tasks.Done()
		db.cleanMacroCache(ctx)
	}()
	return db, err
}

// Close stops background tasks and closes the index database.
func (db *DB) Close() error {
	db.stop()
	db.tasks.Wait()
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.sqldb != nil {
		err := db.sqldb.Close()
		db.sqldb = nil
		return err
	}
	return nil
}

// SetCacheSeed sets the base string used when generating cache keys for
// generated macros. If not set, the value persisted in the index is used.
// Changing the cache seed invalidates cached entries.
func (db *DB) SetCacheSeed(s string) error {
	db.mu.Lock()
	defer db.mu.Unlock()

	if s == string(db.cacheSeed) {
		return nil
	}
	_, err := db.sqldb.Exec(`INSERT OR REPLACE INTO Meta (key, value) VALUES (?,?)`,
		"cacheSeed", []byte(s))
	if err == nil {
		db.cacheSeed = []byte(s)
	}
	return err
}

// Templates returns all the non-hidden templates in the store.
// Templates are ordered non-decreasing by ID.
func (db *DB) Templates() []*tmemes.Template {
	db.mu.Lock()
	all := make([]*tmemes.Template, 0, len(db.templates))
	for _, t := range db.templates {
		if !t.Hidden {
			all = append(all, t)
		}
	}
	db.mu.Unlock()
	sort.Slice(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})
	return all
}

// TemplatesByCreator returns all the non-hidden templates in the store created
// by the specified user. The results are ordered non-decreasing by ID.
func (db *DB) TemplatesByCreator(creator tailcfg.UserID) []*tmemes.Template {
	db.mu.Lock()
	defer db.mu.Unlock()
	var all []*tmemes.Template
	for _, t := range db.templates {
		if !t.Hidden && t.Creator == creator {
			all = append(all, t)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})
	return all
}

// Template returns the template data for the specified ID.
// Hidden templates are treated as not found.
func (db *DB) Template(id int) (*tmemes.Template, error) {
	db.mu.Lock()
	t, ok := db.templates[id]
	db.mu.Unlock()
	if !ok || t.Hidden {
		return nil, fmt.Errorf("template %d not found", id)
	}
	return t, nil
}

// AnyTemplate returns the template data for the specified ID.
// Hidden templates are included.
func (db *DB) AnyTemplate(id int) (*tmemes.Template, error) {
	db.mu.Lock()
	t, ok := db.templates[id]
	db.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("template %d not found", id)
	}
	return t, nil
}

// SetTemplateHidden sets (or clears) the "hidden" flag of a template.  Hidden
// templates are not available for use in creating macros.
func (db *DB) SetTemplateHidden(id int, hidden bool) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	t, ok := db.templates[id]
	if !ok {
		return fmt.Errorf("template %d not found", id)
	}
	if t.Hidden != hidden {
		t.Hidden = hidden
		return db.updateTemplateLocked(t)
	}
	return nil
}

var sep = strings.NewReplacer(" ", "-", "_", "-")

func canonicalTemplateName(name string) string {
	base := strings.Join(strings.Fields(strings.TrimSpace(name)), "-")
	return sep.Replace(strings.ToLower(base))
}

// TemplateByName returns the template data matching the given name.
// Comparison is done without regard to case, leading and trailing whitespace
// are removed, and interior whitespace, "-", and "_" are normalized to "-".
// HIdden templates are excluded.
func (db *DB) TemplateByName(name string) (*tmemes.Template, error) {
	cn := canonicalTemplateName(name)
	if cn == "" {
		return nil, errors.New("empty template name")
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	for _, t := range db.templates {
		if !t.Hidden && t.Name == cn {
			return t, nil
		}
	}
	return nil, fmt.Errorf("template %q not found", cn)
}

// TemplatePath returns the path of the file containing a template image.
// Hidden templates are included.
func (db *DB) TemplatePath(id int) (string, error) {
	// N.B. We include hidden templates in this query, since the image may still
	// be used by macros created before the template was hidden.
	db.mu.Lock()
	t, ok := db.templates[id]
	db.mu.Unlock()
	if !ok {
		return "", fmt.Errorf("template %d not found", id)
	}
	return t.Path, nil
}

// Macro returns the macro data for the specified ID.
func (db *DB) Macro(id int) (*tmemes.Macro, error) {
	db.mu.Lock()
	m, ok := db.macros[id]
	db.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("macro %d not found", id)
	}
	return m, nil
}

// MacrosByCreator returns all the macros created by the specified user.
func (db *DB) MacrosByCreator(creator tailcfg.UserID) []*tmemes.Macro {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.fillAllMacroVotesLocked(); err != nil {
		log.Printf("WARNING: filling macro votes: %v (continuing)", err)
	}
	var all []*tmemes.Macro
	for _, m := range db.macros {
		if m.Creator == creator {
			all = append(all, m)
		}
	}
	sort.Slice(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})
	return all
}

// Macros returns all the macros in the store.
func (db *DB) Macros() []*tmemes.Macro {
	db.mu.Lock()
	defer db.mu.Unlock()
	if err := db.fillAllMacroVotesLocked(); err != nil {
		log.Printf("WARNING: filling macro votes: %v (continuing)", err)
	}
	all := maps.Values(db.macros)
	sort.Slice(all, func(i, j int) bool {
		return all[i].ID < all[j].ID
	})
	return all
}

// CachePath returns a cache file path for the specified macro.  The path is
// returned even if the file is not cached.
func (db *DB) CachePath(m *tmemes.Macro) (string, error) {
	t, err := db.AnyTemplate(m.TemplateID)
	if err != nil {
		return "", err
	}
	return db.cachePath(m, t), nil
}

func (db *DB) cachePath(m *tmemes.Macro, t *tmemes.Template) string {
	key := string(db.cacheSeed)
	if key == "" {
		key = "0000"
	}
	name := fmt.Sprintf("%s-%d%s", key, m.ID, filepath.Ext(t.Path))
	return filepath.Join(db.dir, "macros", name)
}

// AddMacro adds m to the database. It reports an error if m.ID != 0, or
// updates m.ID on success.
func (db *DB) AddMacro(m *tmemes.Macro) error {
	if m.ID != 0 {
		return errors.New("macro ID must be zero")
	} else if m.TemplateID == 0 {
		return errors.New("macro must have a template ID")
	} else if m.TextOverlay == nil {
		return errors.New("macro must have an overlay")
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	db.mu.Lock()
	defer db.mu.Unlock()
	m.ID = db.nextMacroID
	m.CreatedAt = time.Now().UTC()
	db.nextMacroID++
	db.macros[m.ID] = m
	return db.updateMacroLocked(m)
}

// DeleteMacro deletes the specified macro ID from the database.
func (db *DB) DeleteMacro(id int) error {
	db.mu.Lock()
	defer db.mu.Unlock()
	m, ok := db.macros[id]
	if !ok {
		return fmt.Errorf("macro %d not found", id)
	}
	if t, ok := db.templates[m.TemplateID]; ok {
		os.Remove(db.cachePath(m, t))
	}
	delete(db.macros, id)
	_, err := db.sqldb.Exec(`DELETE FROM Macros WHERE id = ?`, id)
	return err
}

// AddTemplate adds t to the database. The ID must be 0 and the Path must be
// empty, these are populated by a successful add.  The other fields of t
// should be initialized by the caller.
//
// If set, fileExt is used as the filename extension for the image file. The
// contents of the template image are fully read from r.
func (db *DB) AddTemplate(t *tmemes.Template, fileExt string, data io.Reader) error {
	if t.ID != 0 {
		return errors.New("template ID must be zero")
	}
	if fileExt == "" {
		fileExt = "png"
	} else {
		fileExt = strings.TrimPrefix(fileExt, ".")
	}
	t.Name = canonicalTemplateName(t.Name)
	if t.Name == "" {
		return errors.New("empty template name")
	} else if _, err := db.TemplateByName(t.Name); err == nil {
		return fmt.Errorf("duplicate template name %q", t.Name)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}

	db.mu.Lock()
	defer db.mu.Unlock()
	id := db.nextTemplateID
	path := filepath.Join(db.dir, "templates", fmt.Sprintf("%d.%s", id, fileExt))
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	t.ID = id
	t.Path = path
	db.nextTemplateID++
	db.templates[t.ID] = t
	return db.updateTemplateLocked(t)
}

// GetVote returns the given user's vote on a single macro.
// If vote < 0, the user downvoted this macro.
// If vote == 0, the user did not vote on this macro.
// If vote > 0, the user upvoted this macro.
func (db *DB) GetVote(userID tailcfg.UserID, macroID int) (vote int, err error) {
	tx, err := db.sqldb.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if err := tx.QueryRow(`SELECT vote FROM Votes WHERE user_id = ? AND macro_id = ?`,
		userID, macroID).Scan(&vote); err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return vote, nil
}

// SetVote records a user vote on the specified macro.
// If vote < 0, a downvote is recorded; if vote > 0 an upvote is recorded.
// If vote == 0 the user's vote is removed.
// Each user can vote at most once for a given macro.
func (db *DB) SetVote(userID tailcfg.UserID, macroID, vote int) (*tmemes.Macro, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	m, ok := db.macros[macroID]
	if !ok {
		return nil, fmt.Errorf("macro %d not found", macroID)
	}
	tx, err := db.sqldb.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if vote == 0 {
		_, err := tx.Exec(`DELETE FROM Votes WHERE user_id = ? AND macro_id = ?`,
			userID, macroID)
		if err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		return m, db.fillMacroVotesLocked(m)
	}

	// Pin votes to the allowed values, +1 for up, -1 for down.
	flag := 1
	if vote < 0 {
		flag = -1
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO Votes (user_id, macro_id, vote) VALUES (?, ?, ?)`,
		userID, macroID, flag)
	if err != nil {
		return nil, err
	} else if err := tx.Commit(); err != nil {
		return nil, err
	}
	if err := db.fillMacroVotesLocked(m); err != nil {
		return nil, err
	}
	return m, nil
}

// UserMacroVote reports the vote status of the given user for a single macro.
// The result is -1 for a downvote, 1 for an upvote, 0 for no vote.
func (db *DB) UserMacroVote(userID tailcfg.UserID, macroID int) (int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	if _, ok := db.macros[macroID]; !ok {
		return 0, fmt.Errorf("macro %d not found", macroID)
	}
	var vote int
	if err := db.sqldb.QueryRow(`SELECT vote FROM Votes WHERE user_id = ? AND macro_id = ?`,
		userID, macroID).Scan(&vote); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return vote, nil
}

// UserVotes all the votes for the given user, as a map from macroID to vote.
// The votes are -1 for a downvote, 1 for an upvote. Macros on which the user
// has not voted are not included.
func (db *DB) UserVotes(userID tailcfg.UserID) (map[int]int, error) {
	db.mu.Lock()
	defer db.mu.Unlock()
	tx, err := db.sqldb.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT macro_id, vote FROM Votes WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	out := make(map[int]int)
	for rows.Next() {
		var macroID, vote int
		if err := rows.Scan(&macroID, &vote); err != nil {
			return nil, err
		}
		out[macroID] = vote
	}
	return out, rows.Err()
}
