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

// ParseEvent is published by parser service when a file is parsed
type ParseEvent struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// EnrichedFeature is the payload sent for indexing
type EnrichedFeature struct {
	ID          int64             `json:"id"`
	Type        string            `json:"type"`     // POINT, LINESTRING, POLYGON
	Geometry    string            `json:"geometry"` // WKT representation
	Tags        map[string]string `json:"tags"`
	Parents     []ParentInfo      `json:"parents"` // sorted by area desc
	StreetNames map[string]string `json:"streets,omitempty"`
}

// ParentInfo holds parent polygon details
type ParentInfo struct {
	ID         int64             `json:"id"`
	Type       string            `json:"type"`
	Names      map[string]string `json:"names"`
	AdminLevel string            `json:"admin_level"`
}

func main() {
	// CLI flags
	natsURL := flag.String("nats", "nats://nats:4222", "NATS URL")
	pgDSN := flag.String("pg", "postgres://postgres:postgres@postgis:5432/osm?sslmode=disable", "Postgres DSN")
	inSubj := flag.String("in", "mapy.osm.parse.completed", "input subject")
	//outSubj := flag.String("out", "mapy.osm.place.enriched", "output subject")
	flag.Parse()

	// Connect to Postgres
	db, err := sql.Open("postgres", *pgDSN)
	if err != nil {
		log.Fatalf("Postgres connect error: %v", err)
	}
	defer db.Close()

	// Connect to NATS JetStream
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		log.Fatalf("NATS connect error: %v", err)
	}
	defer nc.Drain()

	js, err := nc.JetStream()
	if err != nil {
		log.Fatalf("JetStream context error: %v", err)
	}

	// Subscribe to parse completion events
	sub, err := js.Subscribe(*inSubj, func(msg *nats.Msg) {
		var ev ParseEvent
		if err := json.Unmarshal(msg.Data, &ev); err != nil {
			log.Printf("invalid parse event: %v", err)
			msg.Ack()
			return
		}
		log.Printf("Enriching file: %s", ev.Path)

		if err := enrichFile(db, js, ev); err != nil {
			log.Printf("error enriching %s: %v", ev, err)
		}
		msg.Ack()
	}, nats.Durable("enricher"), nats.ManualAck())
	if err != nil {
		log.Fatalf("subscription error: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("Enricher service started, awaiting parse events...")

	// Wait for termination
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down enricher...")
}

// enrichFile decodes the PBF and publishes enriched features
func enrichFile(db *sql.DB, js nats.JetStreamContext, ev ParseEvent) error {
	f, err := os.Open(ev.Path)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}
	defer f.Close()

	dec := osmpbf.NewDecoder(f)
	dec.SetBufferSize(osmpbf.MaxBlobSize)
	if err := dec.Start(runtime.GOMAXPROCS(-1)); err != nil {
		return fmt.Errorf("start decoder: %w", err)
	}

	// Load nodes for geometry reconstruction
	nodes := make(map[int64][2]float64)
	var items []interface{}
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
			nodes[o.ID] = [2]float64{o.Lon, o.Lat}
			items = append(items, o)
		case *osmpbf.Way:
			items = append(items, o)
		}
	}
	// Process features in sequence
	for _, v := range items {
		switch o := v.(type) {
		case *osmpbf.Node:
			if err := publishFeature(db, js, o.ID, "POINT", fmt.Sprintf("POINT(%f %f)", o.Lon, o.Lat), o.Tags, nodes, ev.Name); err != nil {
				log.Printf("error publishing node %d: %v", o.ID, err)
			}
		case *osmpbf.Way:
			pts := make([]string, 0, len(o.NodeIDs))
			for _, id := range o.NodeIDs {
				if xy, ok := nodes[id]; ok {
					pts = append(pts, fmt.Sprintf("%f %f", xy[0], xy[1]))
				}
			}
			if len(pts) < 2 {
				continue
			}
			typ := "LINESTRING"
			if len(pts) > 2 && pts[0] == pts[len(pts)-1] {
				typ = "POLYGON"
			}
			wkt := fmt.Sprintf("%s(%s)", typ, strings.Join(pts, ","))
			if err := publishFeature(db, js, o.ID, typ, wkt, o.Tags, nodes, ev.Name); err != nil {
				log.Printf("error publishing way %d: %v", o.ID, err)
			}
		}
	}
	return nil
}

// publishFeature enriches a single feature and publishes it
func publishFeature(db *sql.DB, js nats.JetStreamContext, id int64, geomType, wkt string, tags map[string]string, nodes map[int64][2]float64, tableName string) error {
	// 1) Parents: polygons containing this feature, sorted by area desc
	if len(tags) == 0 {
		//log.Printf("skip feature %d (%s): no tags", id, geomType)
		return nil
	}
	parentRows, err := db.Query(
		`SELECT id, geom_type, names, admin_level FROM `+tableName+`
         WHERE geom_type='POLYGON' AND ST_Contains(geom, ST_GeomFromText($1,4326))
         ORDER BY ST_Area(geom) DESC`, wkt)
	if err != nil {
		return fmt.Errorf("query parents: %w", err)
	}
	defer parentRows.Close()

	var parents []ParentInfo
	for parentRows.Next() {
		var p ParentInfo
		var nm []byte
		if err := parentRows.Scan(&p.ID, &p.Type, &nm, &p.AdminLevel); err != nil {
			return err
		}
		json.Unmarshal(nm, &p.Names)
		parents = append(parents, p)
	}

	// 2) Street name for points: nearest LINESTRING
	var raw []byte
	streets := make(map[string]string)
	if geomType == "POINT" {
		err := db.QueryRow(
			`SELECT names FROM `+tableName+`
            WHERE geom_type='LINESTRING'
            ORDER BY geom <-> ST_GeomFromText($1,4326)
            LIMIT 1`, wkt,
		).Scan(&raw)
		if err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("nearest line fetch: %w", err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &streets); err != nil {
				return fmt.Errorf("unmarshal street names: %w", err)
			}
			// ensure default key always exists
			if _, ok := streets["default"]; !ok {
				for _, v := range streets {
					streets["default"] = v
					break
				}
			}
		}
	}
	// 3) Assemble enriched payload
	feat := EnrichedFeature{
		ID:          id,
		Type:        geomType,
		Geometry:    wkt,
		Tags:        tags,
		Parents:     parents,
		StreetNames: streets,
	}
	data, err := json.Marshal(feat)
	if err != nil {
		return fmt.Errorf("marshal feature: %w", err)
	}

	// 4) Publish for indexing
	if _, err := js.Publish("mapy.osm.place.enriched", data); err != nil {
		return fmt.Errorf("publish enriched: %w", err)
	}
	return nil
}
