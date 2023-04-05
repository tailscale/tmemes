# tmemes: putting the meme in TS

This bit of fun was brought to you through the amazing power of Tailscale, and
the collaborative efforts of

- Maisem Ali: "I think we need memegen"
- M. J. Fromberger: "why did I not think of that"
- Jenny Zhang: "ok i’m finally in front of a computer, can I go write some css"
- Salman Aljammaz: (quietly moves heaven and earth inside a `<canvas>`)
- Shayne Sweeney: "Would I be stepping on toes if I built a Slack bot?"

together with a lovely and inspirational crew of supporters. There's lots more
fun still to be had, so if you want to jump in, read on! There is also a
wishlist of TODO items at the bottom.

The source lives in https://tailscale.com/tailscale/tmemes. For now this is a
private repository, but it doesn't depend on anything proprietary.

---

## Synopsis

`tmemes` is a web app built mainly in Go and running on `tsnet`. This is a very
terse description of how it all works.

- The server is `tmemes`, a standalone Go binary using `tsnet`. Run

  ```
  TS_AUTHKEY=$KEY go run ./tmemes
  ```

  to start the server.

- The server "database" is a directory of files. Use `--data-dir` to set the
  location; it defaults to `/tmp/tmemes`.

- Terminology:

  - **Template**: A base image that can be decorated with text.
  - **Macro**: An image macro combining a template and a text overlay.
  - **Text overlay**: Lines of text with position and typographical info.

  Types are in `types.go`.

- The data directory contains an `index.db` which is a SQLite database (schema
  in store/schema.sql), plus various other directories of image content:

  - `templates` are the template images.
  - `macros` are cached macros (re-generated on the fly as needed).
  - `static` are some static assets used by the macro generator (esp. fonts).

  The `store` package kinda provides a thin wrapper around these data.

- UI elements are generated by Go HTML templates in `tmemes/ui`. These are
  statically embedded into the server and served by the handlers.

- Static assets needed by the UI are stored in `tmemes/static`. These are
  served via `/static/` paths in the server mux.

---

## API

The server exposes an API over HTTP. There are three buckets of methods:

- `/api/` methods are for programmatic use and return JSON blobs (`api.go`).
- `/content/` methods grant access to image data (`api.go`).
- `/` top-level methods serve the UI and navigation (`ui.go`).

### Methods

- `(GET|DELETE) /api/macro/:id` get or delete one macro by ID. Only a server
  admin, or the user who created a macro, can delete it. Anonymous macros can
  only be deleted by server admins.

- `POST /api/macro` create a new macro. The `POST` body must be a JSON
  `tmemes.Macro` object (`types.go`).

- `PUT /api/macro/:id/upvote` and `PUT /api/macro/:id/downvote` to set an
  upvote or downvote for a single macro by ID, for the calling user.

- `GET /api/macro` get all macros `{"macros":[...], "total":<num>}`.

  The query parameters `page=N` and `count=M` allow pagination, returning the
  Nth page (N > 0) of up to M values. If `count` is omitted a default is
  chosen, currently 20. Regardless whether the result is paged, the total is
  the aggregate total for the whole collection.

  Paging past the end returns `"macros":null`.

  The query parameter `sort_by` changes the sort ordering of the results. Note
  that sorting affects pagination, so you can't change the sort order while
  paging and expect the results to make sense.

  The query parameter `creator=ID` filters for results created by the specified
  user ID. Pagination applies after filtering. As a special case, `anon` can be
  passed to filter for unattributed macros.

  Sort orders currently defined:

  - `default`, `id`, or omitted: sort by ID ascending. This roughly
    corresponds to order of creation, but that's not guaranteed to remain
    true.

  - `recent` sorts in reverse order of creation time (newest first).

  - `popular` sorts in decreasing order of (upvotes - downvotes), breaking
    ties by recency.

- `(GET|POST|DELETE) /api/template/:id` get, set, delete one template by ID.
  The `POST` body must be `multipart/form-data` (TODO: document keys).

- `GET /api/template` get all templates `{"templates":[...], "total":<num>}`.

  The query parameter `creator=ID` filters for results created by the specified
  user ID. As a special case, `anon` can be passed to filter for unattributed
  templates.

- `GET /api/vote` to fetch the vote from the calling user on all macros for
  which the user has cast a nonzero vote.

- `GET /api/vote/:id` to fetch the vote from the calling user on the specified
  macro. It reports a vote of `0` if the user did not vote on that macro.

- `DELETE /api/vote/:id` to delete the vote from the calling user on the
  specified macro ID. This is a no-op without error if the user did not vote on
  that macro.

- `GET /content/template/:id` fetch image content for the specified template.
  An optional trailing `.ext` (e.g., `.jpg`) is allowed, but it must match the
  stored format.

- `GET /content/macro/:id` fetch image content for the specified macro.
  An optional trailing `.ext` is allowed, but it must match the stored template.
  Macros are cached and re-generated on-the-fly for this method.

- `GET /t` serve a UI page for all known templates.

- `GET /t/:id` serve a UI page for one template by ID.

- `GET /` serve a UI page for all known macros.

- `GET /m/:id` serve a UI page for one macro by ID.

- `GET /create/:id` serve a UI page to create a macro from the template with
  the given ID.

- `GET /upload` serve a UI page to upload a new template image.

Other top-level endpoints exist to serve styles, scripts, etc.
See `newMux()` in tmemes/api.go.

---

## TODO tasks

Add anything you think should be done, even if you don't want to do it yourself.

- [ ] (mjf) See about writing some kind of blog post about this experience. We
      agreed this was like a really fun hackathon or "LAN party programming".
      Maybe something for .dev or a culture post on the main blog?
- [ ] (maisem) show macros by user [API done, UI pending]
- [ ] (maisem) add slack integration [partly done?]
- [ ] (mjf) add a "viewed" counter?
- [x] (jenny) add up/down vote
- [x] (mjf) clean up the macro cache periodically in the background
- [ ] (mjf) downsize big templates when they're uploaded
- [ ] (mjf) self-updating timeline on the macros view or maybe a new view?
- [ ] (mjf) allow text colour and scale to be adjusted in the UI
- [ ] (mjf) more flexible text placement in the UU (more lines, locations)
- [x] (mjf) support pagination for /api/templates too
- [x] (mjf) paint text with a contrast-colour outline
- [ ] (jenny) support pagination in the frontend
- [ ] (jenny) support toggling sort order in the frontend
- [ ] (jenny) support showing memes per creator
- [ ] (mjf) support long polling to refresh the UI, like a websocket or SSE
- [ ] (mjf) render timestamps on the browser so they show the user's local time.
- [x] (mjf) support cache control headers for image data
