package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/elastic/go-elasticsearch/v8"
	"github.com/elastic/go-elasticsearch/v8/esapi"
	"github.com/nats-io/nats.go"
	typesense "github.com/typesense/typesense-go/typesense"
	api "github.com/typesense/typesense-go/typesense/api"
)

// Parent holds minimal parent polygon info
type Parent struct {
	ID         int64             `json:"id"`
	Type       string            `json:"type"`
	Names      map[string]string `json:"names"`
	AdminLevel string            `json:"admin_level"`
}

// EnrichedFeature as published by enricher service
type EnrichedFeature struct {
	ID          int64             `json:"id"`
	Type        string            `json:"type"`     // POINT, LINESTRING, POLYGON
	Geometry    string            `json:"geometry"` // WKT representation
	Tags        map[string]string `json:"tags"`
	Parents     []Parent          `json:"parents"` // sorted by area desc
	StreetNames map[string]string `json:"streets,omitempty"`
}

func waitForES(es *elasticsearch.Client, maxAttempts int) error {
	for i := 1; i <= maxAttempts; i++ {
		_, err := es.Info()
		if err == nil {
			log.Println("Elasticsearch is up!")
			return nil
		}
		log.Printf("Waiting for Elasticsearch (attempt %d/%d): %v", i, maxAttempts, err)
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("Elasticsearch did not respond after %d attempts", maxAttempts)
}
func waitForTS(ctx context.Context, ts *typesense.Client, attempts int) error {
	for i := 1; i <= attempts; i++ {
		if _, err := ts.Health(ctx, time.Second); err == nil {
			log.Println("Typesense is ready")
			return nil
		} else {
			log.Printf("Waiting for Typesense (try %d/%d): %v", i, attempts, err)
			time.Sleep(2 * time.Second)
		}
	}
	return fmt.Errorf("Typesense did not become ready after %d attempts", attempts)
}
func main() {
	// CLI flags
	natsURL := flag.String("nats", "nats://nats:4222", "NATS server URL")
	esURL := flag.String("es", "http://elasticsearch:9200", "Elasticsearch URL")
	tsURL := flag.String("ts", "http://typesense:8108", "Typesense server URL")
	tsKey := flag.String("ts-key", "xyz", "Typesense API key")
	subj := flag.String("subject", "mapy.osm.place.enriched", "NATS subject for enriched features")
	flag.Parse()

	// Connect Elasticsearch
	es, err := elasticsearch.NewClient(elasticsearch.Config{Addresses: []string{*esURL}})
	if err != nil {
		log.Fatalf("Elasticsearch connect error: %v", err)
	}
	waitForES(es, 600)
	esExistsRes, err := es.Indices.Exists([]string{"places"})
	if err != nil {
		log.Fatalf("ES exists check error: %v", err)
	}

	if esExistsRes.StatusCode == 404 {
		// Create with a mapping that includes search_text
		mapping := `{
      "mappings": {
        "properties": {
          "search_text": {
            "type":     "text",
            "analyzer": "standard"
          }
        }
      }
    }`
		_, err := es.Indices.Create(
			"places",
			es.Indices.Create.WithBody(strings.NewReader(mapping)),
		)
		if err != nil {
			log.Fatalf("ES create index error: %v", err)
		}
		log.Println("Created Elasticsearch index 'places' with search_text")
	} else {
		// If the index already existed, ensure it has our field
		putMap := `{
      "properties": {
        "search_text": {
          "type":     "text",
          "analyzer": "standard"
        }
      }
    }`
		_, err := es.Indices.PutMapping(
			[]string{"places"},
			strings.NewReader(putMap),
		)
		if err != nil {
			log.Fatalf("ES put-mapping search_text error: %v", err)
		}
		log.Println("Ensured search_text field exists on 'places'")
	}
	// Connect Typesense
	ts := typesense.NewClient(
		typesense.WithServer(*tsURL),
		typesense.WithAPIKey(*tsKey),
	)
	waitForTS(context.Background(), ts, 600)
	ctx := context.Background()
	// Ensure Typesense collection “places” exists
	if _, err := ts.Collection("places").Retrieve(ctx); err != nil {
		enableNested := true
		log.Println("Typesense collection ‘places’ not found, creating…")
		schema := &api.CollectionSchema{
			Name: "places",
			Fields: []api.Field{
				{Name: "id", Type: "int64"},
				{Name: "type", Type: "string"},
				{Name: "geometry", Type: "string"},
				{Name: "names", Type: "object"},
				{Name: "addresses", Type: "object"},
				{Name: "place_types", Type: "string[]"},
				{Name: "wiki", Type: "object"},
				{Name: "contact", Type: "object"},
				{Name: "images", Type: "object"},
				{Name: "search_text", Type: "string"},
			},
			EnableNestedFields: &enableNested,
		}
		if _, err := ts.Collections().Create(ctx, schema); err != nil {
			log.Fatalf("TS create collection error: %v", err)
		}
		log.Println("Created Typesense collection 'places'")
	}
	// Connect NATS
	nc, err := nats.Connect(*natsURL)
	if err != nil {
		log.Fatalf("NATS connect error: %v", err)
	}
	defer nc.Drain()

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

	// Subscribe to enriched features
	sub, err := js.Subscribe(*subj, func(msg *nats.Msg) {
		var feat EnrichedFeature
		if err := json.Unmarshal(msg.Data, &feat); err != nil {
			log.Printf("Invalid enriched feature: %v", err)
			msg.Ack()
			return
		}
		names := make(map[string]string)
		for k, v := range feat.Tags {
			if strings.Contains(k, "name") {
				key := dropBeforeColon(k)
				names[key] = v
			}
		}
		if len(names) == 0 {
			msg.Ack()
			return
		}
		contacts := make(map[string]string)
		for k, v := range feat.Tags {
			if strings.Contains(k, "contact") {
				key := dropBeforeColon(k)
				contacts[key] = v
			}
		}

		// Build address strings per language
		addresses := make(map[string]string)
		// collect all languages
		langs := map[string]struct{}{}
		for lang := range names {
			langs[lang] = struct{}{}
		}
		for _, p := range feat.Parents {
			for lang := range p.Names {
				langs[lang] = struct{}{}
			}
		}
		for lang := range langs {
			parts := []string{}
			// parent addresses
			for _, p := range feat.Parents {
				name := p.Names[lang]
				if name == "" {
					name = p.Names["default"]
				}
				parts = append(parts, name)
			}
			// street for points
			if feat.Type == "POINT" {
				street := feat.StreetNames[lang]
				if street == "" {
					street = feat.StreetNames["default"]
				}
				if street != "" {
					parts = append(parts, street)
				}
			}
			addresses[lang] = strings.Join(parts, ", ")
		}

		// Skip if no names and no addresses
		if len(addresses) == 0 {
			//log.Printf("Skipping feature %d: no names or addresses", feat.ID)
			msg.Ack()
			return
		}
		// Determine place types (can be multiple) as key:value pairs
		typeKeys := []string{
			"amenity", "shop", "tourism", "leisure", "historic",
			"natural", "building", "highway", "man_made", "landuse",
			"power", "waterway", "office", "craft", "emergency",
			"aeroway", "barrier", "sport", "public_transport",
			"education", "railway",
		}
		var placeTypes []string
		for _, key := range typeKeys {
			if val, ok := feat.Tags[key]; ok && val != "" {
				// capture both the major key and its subtype
				placeTypes = append(placeTypes, fmt.Sprintf("%s:%s", key, val))
			}
		}
		// fallback to geometry type if no OSM tags matched
		if len(placeTypes) == 0 {
			placeTypes = append(placeTypes, feat.Type)
		}
		images := make(map[string]string)
		for k, v := range feat.Tags {
			if strings.Contains(feat.Type, "image") {
				key := dropBeforeColon(k)
				images[key] = v
			}
		}
		wikipedia := make(map[string]string)
		for k, v := range feat.Tags {
			if strings.Contains(k, "wikipedia") {
				key := dropBeforeColon(k)
				wikipedia[key] = v
			}
		}
		id := fmt.Sprintf("%d", feat.ID)
		var parts []string
		for _, nm := range names {
			parts = append(parts, nm)
		}
		for _, addr := range addresses {
			parts = append(parts, addr)
		}
		parts = DeduplicateStrings(parts)
		searchText := strings.Join(parts, " ")
		// Build index document
		esDoc := map[string]interface{}{
			"id":          id,
			"type":        feat.Type,
			"geometry":    feat.Geometry,
			"names":       names,
			"addresses":   addresses,
			"place_types": placeTypes,
			"wiki":        wikipedia,
			"contact":     contacts,
			"images":      images,
			"search_text": searchText,
		}
		tsDoc := map[string]interface{}{
			"id":          id,
			"geometry":    feat.Geometry,
			"names":       names,
			"addresses":   addresses,
			"place_types": placeTypes,
			"search_text": parts,
		}
		if len(esDoc["names"].(map[string]string)) < 1 {
			return
		}
		if len(esDoc["addresses"].(map[string]string)) < 1 {
			return
		}

		// Marshal document
		body, err := json.Marshal(esDoc)
		if err != nil {
			log.Printf("JSON marshal error: %v", err)
			msg.Ack()
			return
		}

		// Index into Elasticsearch
		req := esapi.IndexRequest{
			Index:      "places",
			DocumentID: strconv.FormatInt(feat.ID, 10),
			Body:       strings.NewReader(string(body)),
			Refresh:    "true",
		}
		res, err := req.Do(context.Background(), es)
		if err != nil {
			log.Printf("ES index error: %v", err)
		} else {
			res.Body.Close()
		}

		// Upsert into Typesense
		if _, err := ts.Collection("places").Documents().Upsert(context.Background(), tsDoc); err != nil {
			log.Printf("TS upsert error: %v", err)
		}

		msg.Ack()
	}, nats.Durable("indexer"), nats.ManualAck())
	if err != nil {
		log.Fatalf("Subscription error: %v", err)
	}
	defer sub.Unsubscribe()

	log.Println("Indexer started, awaiting enriched features...")

	// Wait for termination
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Println("Shutting down indexer...")
}
func dropBeforeColon(s string) string {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) == 2 {
		return parts[1]
	}
	return s
}

// DeduplicateStrings returns a new slice containing the unique elements
// of input, in the same order they first appear.
func DeduplicateStrings(input []string) []string {
	seen := make(map[string]bool, len(input))
	result := make([]string, 0, len(input))

	for _, s := range input {
		if !seen[s] {
			seen[s] = true
			result = append(result, s)
		}
	}

	return result
}
