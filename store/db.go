package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	_ "embed"

	"github.com/tailscale/tmemes"
	"golang.org/x/sys/unix"
)

//go:embed schema.sql
var schema string

func openDatabase(url string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", url)
	if err != nil {
		return nil, err
	} else if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) loadSQLiteIndex() error {
	db.mu.Lock()
	defer db.mu.Unlock()

	merr := db.loadMacrosLocked()
	terr := db.loadTemplatesLocked()
	derr := db.loadMetadataLocked()

	return errors.Join(merr, terr, derr)
}

func (db *DB) loadMacrosLocked() error {
	db.macros = make(map[int]*tmemes.Macro)
	db.nextMacroID = 0
	mr, err := db.sqldb.Query(`SELECT id, raw FROM Macros`)
	if err != nil {
		return fmt.Errorf("loading macros: %w", err)
	}
	defer mr.Close()
	for mr.Next() {
		var id int
		var macroJSON []byte
		var macro tmemes.Macro

		if err := mr.Scan(&id, &macroJSON); err != nil {
			return fmt.Errorf("scanning macro: %w", err)
		}
		if id > db.nextMacroID {
			db.nextMacroID = id
		}
		if err := json.Unmarshal(macroJSON, &macro); err != nil {
			return fmt.Errorf("decode macro id %d: %w", id, err)
		}
		db.macros[id] = &macro
	}
	db.nextMacroID++
	return mr.Err()
}

func (db *DB) loadTemplatesLocked() error {
	db.templates = make(map[int]*tmemes.Template)
	db.nextTemplateID = 0
	mr, err := db.sqldb.Query(`SELECT id, raw FROM Templates`)
	if err != nil {
		return fmt.Errorf("loading templates: %w", err)
	}
	defer mr.Close()
	for mr.Next() {
		var id int
		var tmplJSON []byte
		var tmpl tmemes.Template

		if err := mr.Scan(&id, &tmplJSON); err != nil {
			return fmt.Errorf("scanning template: %w", err)
		}
		if id > db.nextTemplateID {
			db.nextTemplateID = id
		}
		if err := json.Unmarshal(tmplJSON, &tmpl); err != nil {
			return fmt.Errorf("decode template id %d: %w", id, err)
		}
		db.templates[id] = &tmpl
	}
	db.nextTemplateID++
	return mr.Err()
}

func (db *DB) loadMetadataLocked() error {
	row := db.sqldb.QueryRow(`SELECT value FROM Meta WHERE key = ?`, "cacheSeed")
	if err := row.Scan(&db.cacheSeed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return nil
}

func (db *DB) updateTemplateLocked(t *tmemes.Template) error {
	bits, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = db.sqldb.Exec(`INSERT OR REPLACE INTO Templates (id, raw) VALUES (?, ?)`,
		t.ID, bits)
	return err
}

func (db *DB) updateMacroLocked(m *tmemes.Macro) error {
	cp := *m
	cp.Upvotes = 0
	cp.Downvotes = 0
	bits, err := json.Marshal(cp)
	if err != nil {
		return err
	}
	_, err = db.sqldb.Exec(`INSERT OR REPLACE INTO Macros (id, raw) VALUES (?, ?)`,
		m.ID, bits)
	return err
}

func (db *DB) fillMacroVotesLocked(m *tmemes.Macro) error {
	var up, down int
	row := db.sqldb.QueryRow(`SELECT up, down FROM VoteTotals WHERE macro_id = ?`, m.ID)
	if err := row.Scan(&up, &down); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	m.Upvotes = up
	m.Downvotes = down
	return nil
}

func (db *DB) fillAllMacroVotesLocked() error {
	tx, err := db.sqldb.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT macro_id, up, down FROM VoteTotals`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var macroID, up, down int
		if err := rows.Scan(&macroID, &up, &down); err != nil {
			return err
		}
		if m, ok := db.macros[macroID]; ok {
			m.Upvotes = up
			m.Downvotes = down
		}
	}
	return rows.Err()
}

func (db *DB) cleanMacroCache(ctx context.Context) {
	const pollInterval = time.Minute // how often to scan the cache
	log.Printf("Starting macro cache cleaner (poll=%v, max-age=%v, min-prune=%d bytes)",
		pollInterval, db.maxAccessAge, db.minPruneBytes)

	t := time.NewTicker(pollInterval)
	defer t.Stop()

	cacheDir := filepath.Join(db.dir, "macros")
	for {
		select {
		case <-ctx.Done():
			log.Printf("Macro cache cleaner exiting (%v)", ctx.Err())
			return
		case <-t.C:
		}

		// Phase 1: List all the files in the macro cache.
		es, err := os.ReadDir(cacheDir)
		if err != nil {
			log.Printf("WARNING: reading cache directory: %v (continuing)", err)
			continue
		}

		// Phase 2: Select candidate paths for removal based on access time.
		var totalSize int64
		var cand []string
		for _, e := range es {
			if !e.Type().IsRegular() {
				continue // ignore directories, other nonsense
			}

			path := filepath.Join(cacheDir, e.Name())
			atime, err := getAccessTime(path)
			if err != nil {
				continue // skip
			}

			age := time.Since(atime)
			if age > db.maxAccessAge {
				cand = append(cand, path)
			}

			fi, _ := e.Info()
			totalSize += fi.Size()
		}

		// If we don't have eny candidates, or have not stored enough data to be
		// worried about, go back to sleep.
		if totalSize <= db.minPruneBytes || len(cand) == 0 {
			continue
		}

		// Phase 3: Grab the lock and clean up candidates.  By holding the lock,
		// we ensure we are not racing with a last-minute /content request; if we
		// win the race, the unlucky call will regenerate the file. If we lose,
		// the caller is done with it by the time we unlink.
		func() {
			db.mu.Lock()
			defer db.mu.Unlock()
			for _, path := range cand {
				if os.Remove(path) == nil {
					log.Printf("[macro cache] removed %q", path)
				}

				// N.B. We ignore errors herd, it's not the end of the world if we
				// aren't able to remove everything.
			}
		}()
	}
}

func getAccessTime(path string) (time.Time, error) {
	var sbuf unix.Stat_t
	if err := unix.Stat(path, &sbuf); err != nil {
		return time.Time{}, err
	}
	return time.Unix(sbuf.Atim.Sec, sbuf.Atim.Nsec).UTC(), nil
}
