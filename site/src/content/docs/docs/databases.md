---
title: Databases
description: SQLite by default, PostgreSQL by URL. Migrations run at start.
---

Mailinois keeps everything in one database, set by `database.url` or `MAILINOIS_DATABASE_URL`. Migrations run every time it starts, so a new version brings its database up to date on its own.

## SQLite (the default)

```yaml
database:
  url: "sqlite:/var/lib/mailinois/mailinois.db"
```

There is nothing to install: SQLite is built into the binary. The database is one file, run in WAL mode, so while Mailinois runs you will also see `mailinois.db-wal` and `mailinois.db-shm` beside it.

Keep the file on a local disk. SQLite's WAL mode does not work on a network share.

In Docker, the usual place is a volume mounted at `/data`, with `sqlite:/data/mailinois.db`.

### Backing up

There are two safe ways:

1. While Mailinois runs, with the `sqlite3` command line tool's `.backup`, which takes a consistent copy:

   ```sh
   sqlite3 /var/lib/mailinois/mailinois.db ".backup '/backups/mailinois.db'"
   ```

2. Stop Mailinois, copy the file, and start it again.

Copying `mailinois.db` with `cp` while Mailinois runs can miss recent writes that are still in the `-wal` file, or catch the file halfway through one.

The container image has no shell and no `sqlite3`. Either stop the container and copy the volume, or run `sqlite3` on the Docker host against the volume's directory.

To restore, stop Mailinois, put the backup in place, delete any `mailinois.db-wal` and `mailinois.db-shm` left beside it, and start it again.

## PostgreSQL

```yaml
database:
  url: "postgres://mailinois:secret@db.internal:5432/mailinois"
```

Give Mailinois a database of its own, or a schema inside a database you already run, by adding `search_path` to the URL:

```yaml
database:
  url: "postgres://app:secret@db.internal:5432/app?search_path=mailinois"
```

The user needs to be able to create tables in that schema, because migrations run at start. Creating the schema first keeps its ownership clear:

```sql
CREATE SCHEMA mailinois AUTHORIZATION app;
```

Back it up as you back up the rest of PostgreSQL. For the schema form, `pg_dump -n mailinois app` dumps only Mailinois's tables.

To keep the password out of the config file, set the URL with `MAILINOIS_DATABASE_URL` instead.

## Which one?

SQLite suits a single Mailinois with its data on a local disk: there is nothing else to run, patch or connect to.

PostgreSQL suits you if you already run it and back it up, or want Mailinois's data in a managed database.
