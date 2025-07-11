package main

import (
    "context"
    "encoding/json"
    "fmt"
    "log"
    "os"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/nats-io/nats.go"
    "github.com/qedus/osmpbf"
)

// Completed mirrors the downloader event payload
// published on osm.download.completed
// Name is dataset name, Path local path to the downloaded PBF file
// SizeBytes is the size of the file in bytes, SHA256 is the hash
// Status is the status string, FinishedAt is the completion time
// Only Name and Path are used by the parser.
type Completed struct {
    Name       string `json:"name"`
    Path       string `json:"path"`
    SizeBytes  int64  `json:"sizeBytes"`
    SHA256     string `json:"sha256"`
    Status     string `json:"status"`
    FinishedAt string `json:"finishedAt"`
}

func main() {
    natsURL := os.Getenv("NATS_URL")
    if natsURL == "" {
        natsURL = nats.DefaultURL
    }
    pgURL := os.Getenv("PG_CONN")
    if pgURL == "" {
        log.Fatal("PG_CONN not set")
    }

    nc, err := nats.Connect(natsURL)
    if err != nil {
        log.Fatalf("connect nats: %v", err)
    }
    defer nc.Drain()

    dbpool, err := pgxpool.New(context.Background(), pgURL)
    if err != nil {
        log.Fatalf("connect db: %v", err)
    }
    defer dbpool.Close()

    _, err = nc.QueueSubscribe("osm.download.completed", "parser", func(m *nats.Msg) {
        var evt Completed
        if err := json.Unmarshal(m.Data, &evt); err != nil {
            log.Printf("invalid message: %v", err)
            return
        }
        if err := parseFile(context.Background(), dbpool, evt.Path); err != nil {
            log.Printf("parse error: %v", err)
        }
    })
    if err != nil {
        log.Fatalf("subscribe: %v", err)
    }

    select {}
}

func parseFile(ctx context.Context, db *pgxpool.Pool, path string) error {
    f, err := os.Open(path)
    if err != nil {
        return fmt.Errorf("open pbf: %w", err)
    }
    defer f.Close()

    d := osmpbf.NewDecoder(f)
    if err := d.Start(ctx); err != nil {
        return fmt.Errorf("start decoder: %w", err)
    }

    for {
        v, err := d.Decode()
        if err == osmpbf.ErrEOF {
            break
        }
        if err != nil {
            return fmt.Errorf("decode: %w", err)
        }
        switch el := v.(type) {
        case *osmpbf.Way:
            if err := upsert(ctx, db, el.ID, el.Tags); err != nil {
                return err
            }
        case *osmpbf.Relation:
            if err := upsert(ctx, db, el.ID, el.Tags); err != nil {
                return err
            }
        }
    }
    return nil
}

func upsert(ctx context.Context, db *pgxpool.Pool, id int64, tags map[string]string) error {
    b, err := json.Marshal(tags)
    if err != nil {
        return err
    }
    _, err = db.Exec(ctx, `INSERT INTO osm_features(osm_id, tags)
        VALUES($1,$2)
        ON CONFLICT (osm_id) DO UPDATE SET tags=EXCLUDED.tags`, id, b)
    return err
}

