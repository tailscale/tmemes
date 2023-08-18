# tmemes API

The `tmemes` server exposes an API over HTTP.  There are three buckets of
methods:

- `/` top-level methods serve the UI and navigation (`ui.go`).
- `/api/` methods are for programmatic use and return JSON blobs (`api.go`).
- `/content/` methods grant access to image data (`api.go`).

Access to the API requires the caller be a user of the tailnet hosting the
server node, or a tailnet into which the server node has been shared.
Access is via plain HTTP (not HTTPS).
No authentication tokens are required.

# Methods

## User Interface

- `GET /` serve a UI page for all known macros (equivalent to `/m`)

- `GET /t` serve a UI page for all known templates. Supports [pagination](#pagination).

- `GET /t/:id` serve a UI page for one template by ID.

- `GET /m` serve a UI page for all known templates. Supports [pagination](#pagination).

- `GET /m/:id` serve a UI page for one macro by ID.

- `GET /create/:id` serve a UI page to create a macro from the template with
  the given ID.

- `GET /upload` serve a UI page to upload a new template image.

Other top-level endpoints exist to serve styles, scripts, etc.  See `newMux()`
in [tmemes/api.go](../tmemes/api.go).


## Programmatic (`/api`)

- `(GET|DELETE) /api/macro/:id` get or delete one macro by ID. Only a server
  admin, or the user who created a macro, can delete it. Anonymous macros can
  only be deleted by server admins.

- `POST /api/macro` create a new macro. The `POST` body must be a JSON
  `tmemes.Macro` object (`types.go`).

- `GET /api/macro` get all macros `{"macros":[...], "total":<num>}`.
  This call supports [pagination](#pagination) and [filtering](#filtering).
  Paging past the end returns `"macros":null`.

- `(GET|POST|DELETE) /api/template/:id` get, set, delete one template by ID.
  The `POST` body must be `multipart/form-data` (TODO: document keys).

- `GET /api/template` get all templates `{"templates":[...], "total":<num>}`.
  This call supports [pagination](#pagination) and [filtering](#filtering).
  Paging past the end returns `"templates":null`.

- `GET /api/vote` to fetch the vote from the calling user on all macros for
  which the user has cast a nonzero vote.

- `GET /api/vote/:id` to fetch the vote from the calling user on the specified
  macro. It reports a vote of `0` if the user did not vote on that macro.

- `DELETE /api/vote/:id` to delete the vote from the calling user on the
  specified macro ID. This is a no-op without error if the user did not vote on
  that macro.

- `PUT /api/vote/:id/up` and `PUT /api/vote/:id/down` to set an upvote or
  downvote for a single macro by ID, for the calling user.


## Content (`/content`)

- `GET /content/template/:id` fetch image content for the specified template.
  An optional trailing `.ext` (e.g., `.jpg`) is allowed, but it must match the
  stored format.

- `GET /content/macro/:id` fetch image content for the specified macro.  An
  optional trailing `.ext` is allowed, but it must match the stored template.
  Macros are cached and re-generated on-the-fly for this method.


## Pagination

For APIs that support pagination, the query parameters `page=N` and `count=M`
specify a subset of the available results, returning the Nth page (N > 0) of up
to M values. If `count` is omitted a default is chosen. Regardless whether the
result is paged, the total is the aggregate total for the whole collection.

## Sorting

The query parameter `sort` changes the sort ordering of macro results. Note
that sorting affects pagination, so you can't change the sort order while
paging and expect the results to make sense.

Sort orders currently defined:

- `default`, `id`, or omitted: sort by ID ascending. This roughly corresponds
  to order of creation, but that's not guaranteed to remain true.

- `recent` sorts in reverse order of creation time (newest first).

- `popular` sorts in decreasing order of (upvotes - downvotes), breaking ties
   by recency (newest first).

- `top-popular` sorts entries from the last 1 hour in reverse order of creation
  time (as `recent`); entries older than that are sorted by popularity.

- `score` sorts entries by a blended score that is based on popularity but
  which gives extra weight to recent entries.

## Filtering

Where relevant, the query parameter `creator=ID` filters for results created by
the specified user ID. As a special case, `anon` or `anonymous`can be passed to
filter for unattributed templates.
