-- Database schema for tmemes.
CREATE TABLE IF NOT EXISTS Templates (
  id INTEGER PRIMARY KEY,
  raw BLOB -- JSON tmemes.Template
);

CREATE TRIGGER IF NOT EXISTS TemplateDel
  AFTER DELETE ON Templates FOR EACH ROW
BEGIN
  DELETE FROM Macros
   WHERE json_extract(raw, '$.templateID') = OLD.id;
END;

CREATE TABLE IF NOT EXISTS Macros (
  id INTEGER PRIMARY KEY,
  raw BLOB -- JSON tmemes.Macro
);

CREATE TRIGGER IF NOT EXISTS MacroDel
 AFTER DELETE ON Macros FOR EACH ROW
BEGIN
 DELETE FROM Votes WHERE macro_id = OLD.id;
END;

CREATE TABLE IF NOT EXISTS Votes (
  user_id INTEGER NOT NULL,
  macro_id INTEGER NOT NULL,
  vote INTEGER NOT NULL,
  last_update TIMESTAMP DEFAULT CURRENT_TIMESTAMP,

  CHECK (vote = -1 OR vote = 1),
  FOREIGN KEY (macro_id) REFERENCES Macros(id),
  UNIQUE (user_id, macro_id)
);

CREATE VIEW IF NOT EXISTS VoteTotals AS
  WITH upvotes AS (
    SELECT macro_id, sum(vote) up FROM Votes
     WHERE vote = 1 GROUP BY macro_id
  ), downvotes AS (
    SELECT macro_id, -sum(vote) down FROM Votes
     WHERE vote = -1 GROUP BY macro_id
  )
  SELECT iif(upvotes.macro_id, upvotes.macro_id, downvotes.macro_id) macro_id,
         iif(up, up, 0) up,
         iif(down, down, 0) down
    FROM upvotes FULL OUTER JOIN downvotes
      ON (upvotes.macro_id = downvotes.macro_id)
;

CREATE TABLE IF NOT EXISTS Meta (
  key TEXT UNIQUE NOT NULL,
  value BLOB
);
