package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"

	_ "github.com/lib/pq"
	"github.com/nats-io/nats.go"
	"github.com/qedus/osmpbf"
)

// Event represents a download completed event from the downloader service
type Event struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Status    string `json:"status"`
	SHA256    string `json:"sha256"`
	Size      int64  `json:"size_bytes"`
	Timestamp string `json:"timestamp_utc"`
}

func main() {
	// CLI flags
	natsURL := flag.String("nats", "nats://nats:4222", "NATS server URL")
	pgDSN := flag.String("pg", "postgres://postgres:postgres@postgis:5432/osm?sslmode=disable", "Postgres DSN")
	subj := flag.String("subject", "mapy.osm.download.*", "NATS subject for download events")
	flag.Parse()

	// Connect to Postgres
	db, err := sql.Open("postgres", *pgDSN)
	if err != nil {
		log.Fatalf("Postgres connect error: %v", err)
	}
	defer db.Close()

	// Ensure PostGIS extension and places table exist
	if _, err := db.Exec(`CREATE EXTENSION IF NOT EXISTS postgis`); err != nil {
		log.Fatalf("Error creating PostGIS extension: %v", err)
	}

	// Connect to NATS
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		log.Fatalf("NATS connect error: %v", err)
	}
	defer nc.Drain()

	// Create JetStream context
	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("JetStream context error: %v", err)
	}

	// Ensure stream exists for our download events
	_, err = js.AddStream(&nats.StreamConfig{
		Name:     "MAPY_STREAM",
		Subjects: []string{"mapy.>"},
		Storage:  nats.FileStorage,
	})
	if err != nil && !strings.Contains(err.Error(), "stream name already in use") {
		log.Fatalf("AddStream error: %v", err)
	}

	// Subscribe to download events (durable, manual ack)
	sub, err := js.Subscribe(*subj, func(msg *nats.Msg) {
		var ev Event
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			log.Printf("Invalid event data: %v", err)
			msg.Ack()
			return
		}
		if _, err := db.Exec(`
        CREATE TABLE IF NOT EXISTS ` + ev.Name + ` (
            id BIGINT PRIMARY KEY,
            geom_type text,
            geom geometry(Geometry, 4326),
            names JSONB,
            admin_level TEXT
        )
    `); err != nil {
			log.Fatalf("Error creating places table: %v", err)
		}
		log.Printf("Received download event: %s (%s)", ev.Name, ev.Status)
		var exists bool
		checkIdx := `SELECT EXISTS (
            SELECT 1 FROM pg_indexes
            WHERE schemaname = current_schema()
              AND tablename = $1
              AND indexname = $2
        )`
		idxName := "idx_places_geom"
		if err := db.QueryRow(checkIdx, ev.Name, idxName).Scan(&exists); err != nil {
			log.Printf("Error checking index %s on %s: %v", idxName, ev.Name, err)
		}
		if exists {
			log.Printf("Index %s already exists on %s, skipping parse", idxName, ev.Name)
			// Publish parse completed
			evt := map[string]string{"name": ev.Name, "path": ev.Path}
			data, _ := json.Marshal(evt)
			if _, err := js.Publish("mapy.osm.parse.completed", data); err != nil {
				log.Printf("Publish parse.completed error: %v", err)
			}
			msg.Ack()
			return
		}
		if err := processFile(db, ev.Path, ev.Name); err != nil {
			log.Printf("Error processing file %s: %v", ev.Path, err)
		} else {
			// Publish parse completion event
			parseEvent := struct {
				Name string `json:"name"`
				Path string `json:"path"`
			}{ev.Name, ev.Path}
			data, _ := json.Marshal(parseEvent)
			if _, err := js.Publish("mapy.osm.parse.polygon.completed", data); err != nil {
				log.Printf("Publish parse.completed error: %v", err)
			}
		}
		// Add indexes for performance
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_places_id ON ` + ev.Name + `(id)`); err != nil {
			log.Fatalf("Error creating index on id: %v", err)
		}
		if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_places_geom ON ` + ev.Name + ` USING GIST(geom)`); err != nil {
			log.Fatalf("Error creating GIST index on geom: %v", err)
		}
		msg.Ack()
	}, nats.Durable("parser"), nats.ManualAck())
	if err != nil {
		log.Fatalf("Subscription error: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("Parser service started, waiting for download events...")

	// Wait for SIGINT/SIGTERM
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutdown signal received, exiting")
}

func processFile(db *sql.DB, filePath, tableName string) error {
	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("open file %s: %w", filePath, err)
	}
	defer f.Close()

	dec := osmpbf.NewDecoder(f)
	dec.SetBufferSize(osmpbf.MaxBlobSize)
	if err := dec.Start(runtime.GOMAXPROCS(-1)); err != nil {
		return fmt.Errorf("start decoder: %w", err)
	}

	nodeMap := make(map[int64][2]float64)
	for {
		v, err := dec.Decode()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("decode error: %w", err)
		}

		switch o := v.(type) {
		case *osmpbf.Node:
			nodeMap[o.ID] = [2]float64{o.Lon, o.Lat}
		case *osmpbf.Way:
			if err := upsertWay(db, o, nodeMap, tableName); err != nil {
				log.Printf("upsert way %d error: %v", o.ID, err)
			}
		}
	}
	return nil
}

func upsertWay(db *sql.DB, way *osmpbf.Way, nodes map[int64][2]float64, tableName string) error {
	coords := make([]string, 0, len(way.NodeIDs))
	for _, nid := range way.NodeIDs {
		if xy, ok := nodes[nid]; ok {
			coords = append(coords, fmt.Sprintf("%f %f", xy[0], xy[1]))
		}
	}
	if len(coords) < 2 {
		return nil
	}
	geomType := "LINESTRING"
	geom := "LINESTRING(" + strings.Join(coords, ",") + ")"
	if len(coords) > 2 && coords[0] == coords[len(coords)-1] {
		geom = "POLYGON((" + strings.Join(coords, ",") + "))"
		geomType = "POLYGON"
	}

	names := make(map[string]string)
	adminLevel := ""
	for k, v := range way.Tags {
		switch {
		case k == "name":
			names["default"] = v
		case strings.HasPrefix(k, "name:"):
			names[strings.TrimPrefix(k, "name:")] = v
		case k == "admin_level":
			adminLevel = v
		}
	}
	if len(names) == 0 {
		return nil
	}
	namesJSON, _ := json.Marshal(names)
	_, err := db.Exec(
		`INSERT INTO `+tableName+` (id, geom_type, geom, names, admin_level)
	VALUES ($1, $2, ST_GeomFromText($3,4326), $4::jsonb, $5)
	ON CONFLICT (id) DO UPDATE
	SET geom_type = EXCLUDED.geom_type,
		geom = EXCLUDED.geom,
		names = EXCLUDED.names,
		admin_level = EXCLUDED.admin_level`,
		way.ID, geomType, geom, namesJSON, adminLevel)
	return err
}
