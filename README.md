# Mapy Places Platform

This repository contains a micro-services based platform for place search and indexing.

Refer to `docs/` for diagrams and architecture notes.

## Development

The dev stack can be started with docker compose:

```bash
docker compose up --build
```

This launches NATS, PostGIS, Elasticsearch, Typesense, and the downloader and parser services which listen for `osm.download.*` events.
