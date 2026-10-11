-- name: GetObject :one
SELECT kind, project, name, revision, data
FROM objects
WHERE kind = ? AND project = ? AND name = ?;

-- name: ListObjects :many
SELECT kind, project, name, revision, data
FROM objects
WHERE kind = sqlc.arg(kind)
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR project = sqlc.arg(project))
  AND (project > sqlc.arg(after_project)
       OR (project = sqlc.arg(after_project) AND name > sqlc.arg(after_name)))
ORDER BY project, name
LIMIT sqlc.arg(max_rows);

-- name: InsertObject :exec
INSERT INTO objects (kind, project, name, revision, data)
VALUES (?, ?, ?, ?, ?);

-- name: UpdateObject :exec
UPDATE objects
SET revision = ?, data = ?
WHERE kind = ? AND project = ? AND name = ?;

-- name: DeleteObject :exec
DELETE FROM objects
WHERE kind = ? AND project = ? AND name = ?;

-- name: InsertEvent :exec
INSERT INTO events (revision, type, kind, project, name, data)
VALUES (?, ?, ?, ?, ?, ?);

-- name: ListEvents :many
SELECT revision, type, kind, project, name, data
FROM events
WHERE revision > sqlc.arg(after)
  AND revision <= sqlc.arg(through)
  AND (CAST(sqlc.arg(kind) AS TEXT) = '' OR kind = sqlc.arg(kind))
  AND (CAST(sqlc.arg(project) AS TEXT) = '' OR project = sqlc.arg(project))
ORDER BY revision
LIMIT sqlc.arg(max_rows);

-- name: DeleteEventsThrough :exec
DELETE FROM events
WHERE revision <= ?;

-- name: GetMeta :one
SELECT value FROM meta WHERE key = ?;

-- name: SetMeta :exec
UPDATE meta SET value = ? WHERE key = ?;
